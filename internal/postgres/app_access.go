package postgres

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"polygate/internal/server"
)

const (
	AppAccessPublic  = "Public"
	AppAccessPrivate = "Owner and administrators"
	appTicketTTL     = time.Minute
	appSessionTTL    = 8 * time.Hour
)

type AppAccess struct {
	Mode    string
	OwnerID string
}

func (s *Store) GetAppAccess(ctx context.Context, appID string) (AppAccess, error) {
	var access AppAccess
	err := s.Pool.QueryRow(ctx, `SELECT access_mode,account_id::text FROM apps WHERE id=$1`, appID).
		Scan(&access.Mode, &access.OwnerID)
	return access, storeError(err)
}

// PublicAppAllowed is used by every legacy and path-based public gateway
// route. A missing app is denied without revealing whether its ID exists.
func (s *Store) PublicAppAllowed(ctx context.Context, appID string) (bool, error) {
	var allowed bool
	err := s.Pool.QueryRow(ctx, `SELECT COALESCE((SELECT access_mode=$2 FROM apps WHERE id=$1),false)`,
		appID, AppAccessPublic).Scan(&allowed)
	return allowed, storeError(err)
}

func (s *Store) PrivateAppAllowed(ctx context.Context, appID string) (bool, error) {
	var allowed bool
	err := s.Pool.QueryRow(ctx, `SELECT COALESCE((SELECT access_mode=$2 FROM apps WHERE id=$1),false)`,
		appID, AppAccessPrivate).Scan(&allowed)
	return allowed, storeError(err)
}

func accessToken() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

func accessTokenHash(raw string) []byte {
	sum := sha256.Sum256([]byte(raw))
	return sum[:]
}

func validAppReturnPath(raw string) bool {
	if len(raw) < 1 || len(raw) > 2048 || !strings.HasPrefix(raw, "/") ||
		strings.HasPrefix(raw, "//") || strings.ContainsAny(raw, "\\\r\n\x00") {
		return false
	}
	parsed, err := url.ParseRequestURI(raw)
	return err == nil && !parsed.IsAbs() && parsed.Host == "" && parsed.Fragment == "" &&
		!strings.HasPrefix(parsed.Path, "//") && !strings.Contains(parsed.Path, "\\") &&
		!strings.HasPrefix(strings.ToLower(parsed.Path), "/_polygate/")
}

// IssueAppAccessTicket is called only after the console has authenticated the
// actor. It checks the current owner/role and stores no bearer token in clear.
func (s *Store) IssueAppAccessTicket(ctx context.Context, actorID, appID, returnPath string) (string, error) {
	if !validAppReturnPath(returnPath) {
		return "", server.ErrForbidden
	}
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return "", storeError(err)
	}
	defer tx.Rollback(ctx)
	var ownerID, mode, role string
	var enabled bool
	err = tx.QueryRow(ctx, `SELECT app.account_id::text,app.access_mode,actor.role,actor.enabled
		FROM apps app CROSS JOIN accounts actor WHERE app.id=$1 AND actor.id=$2
		FOR SHARE OF app,actor`, appID, actorID).Scan(&ownerID, &mode, &role, &enabled)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && (!enabled || mode != AppAccessPrivate ||
		(actorID != ownerID && role != "admin")) {
		return "", server.ErrForbidden
	}
	if err != nil {
		return "", storeError(err)
	}
	ticket, err := accessToken()
	if err != nil {
		return "", err
	}
	_, err = tx.Exec(ctx, `INSERT INTO app_access_tickets(ticket_hash,app_id,account_id,return_path,expires_at)
		VALUES($1,$2,$3,$4,$5)`, accessTokenHash(ticket), appID, actorID, returnPath, time.Now().Add(appTicketTTL))
	if err != nil {
		return "", storeError(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return "", storeError(err)
	}
	return ticket, nil
}

// ConsumeAppAccessTicket atomically uses a ticket once and binds the new
// session to the exact app. The current account and app mode are rechecked.
func (s *Store) ConsumeAppAccessTicket(ctx context.Context, appID, ticket string) (string, string, error) {
	if len(ticket) != 43 {
		return "", "", server.ErrForbidden
	}
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return "", "", storeError(err)
	}
	defer tx.Rollback(ctx)
	var actorID, returnPath string
	err = tx.QueryRow(ctx, `DELETE FROM app_access_tickets
		WHERE ticket_hash=$1 AND app_id=$2 AND expires_at>now()
		RETURNING account_id::text,return_path`, accessTokenHash(ticket), appID).Scan(&actorID, &returnPath)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", server.ErrForbidden
	}
	if err != nil {
		return "", "", storeError(err)
	}
	var allowed bool
	err = tx.QueryRow(ctx, `SELECT actor.enabled AND app.access_mode=$3 AND
		(actor.id=app.account_id OR actor.role='admin')
		FROM apps app JOIN accounts actor ON actor.id=$2 WHERE app.id=$1
		FOR SHARE OF app,actor`, appID, actorID, AppAccessPrivate).Scan(&allowed)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !allowed {
		// Commit the deletion so a rejected ticket cannot be replayed later.
		if commitErr := tx.Commit(ctx); commitErr != nil {
			return "", "", storeError(commitErr)
		}
		return "", "", server.ErrForbidden
	}
	if err != nil {
		return "", "", storeError(err)
	}
	session, err := accessToken()
	if err != nil {
		return "", "", err
	}
	_, err = tx.Exec(ctx, `INSERT INTO app_access_sessions(session_hash,app_id,account_id,expires_at)
		VALUES($1,$2,$3,$4)`, accessTokenHash(session), appID, actorID, time.Now().Add(appSessionTTL))
	if err != nil {
		return "", "", storeError(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return "", "", storeError(err)
	}
	return session, returnPath, nil
}

// AppAccessSessionAllowed validates every private request against current DB
// state, so mode changes, account disabling and admin demotion take effect.
func (s *Store) AppAccessSessionAllowed(ctx context.Context, appID, session string) (bool, error) {
	if len(session) != 43 {
		return false, nil
	}
	var allowed bool
	err := s.Pool.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM app_access_sessions s
		JOIN apps app ON app.id=s.app_id
		JOIN accounts actor ON actor.id=s.account_id
		WHERE s.session_hash=$1 AND s.app_id=$2 AND s.expires_at>now()
		AND app.access_mode=$3 AND actor.enabled
		AND (actor.id=app.account_id OR actor.role='admin'))`,
		accessTokenHash(session), appID, AppAccessPrivate).Scan(&allowed)
	return allowed, storeError(err)
}
