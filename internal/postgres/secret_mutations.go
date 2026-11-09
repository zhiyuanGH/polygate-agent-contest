package postgres

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"polygate/internal/server"
)

// ErrSecretMutationPending means an earlier mutation may have reached
// Kubernetes. The reconciler must resolve it before another mutation starts.
var ErrSecretMutationPending = errors.New("student Secret mutation is awaiting reconciliation")

type SecretMutationProbe func(context.Context, string, string, string, string) (bool, error)

// SecretMutationBacklogHealthy is a value-free health signal. A mutation can
// normally remain Pending briefly while Kubernetes and PostgreSQL converge.
func (s *Store) SecretMutationBacklogHealthy(ctx context.Context) (bool, error) {
	var stale bool
	err := s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM operations
		WHERE kind IN ('SECRET_UPSERT','SECRET_DELETE') AND state='Pending'
		AND created_at < now()-interval '2 minutes')`).Scan(&stale)
	return !stale, storeError(err)
}

func (s *Store) PutSecretWithAudit(ctx context.Context, accountID, subject, key string,
	put func(context.Context, string) error) error {
	return s.mutateSecret(ctx, accountID, subject, key, "SECRET_UPSERT", false, put)
}

func (s *Store) DeleteSecretIfUnused(ctx context.Context, accountID, subject, key string,
	remove func(context.Context, string) error) error {
	return s.mutateSecret(ctx, accountID, subject, key, "SECRET_DELETE", true, remove)
}

func (s *Store) mutateSecret(ctx context.Context, accountID, subject, key, kind string,
	checkReferences bool, mutate func(context.Context, string) error) error {
	opID, err := server.NewID()
	if err != nil {
		return err
	}
	detail, err := json.Marshal(map[string]string{"name": key})
	if err != nil {
		return err
	}
	// Commit the intent before making the external change. An uncertain API or
	// database response then leaves a durable Pending operation to inspect.
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return storeError(err)
	}
	defer tx.Rollback(ctx)
	var enabled bool
	if err := tx.QueryRow(ctx, `SELECT enabled FROM accounts WHERE id=$1 FOR UPDATE`, accountID).Scan(&enabled); err != nil {
		return storeError(err)
	}
	if !enabled {
		return server.ErrDisabled
	}
	var pending bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM operations WHERE account_id=$1
		AND kind IN ('SECRET_UPSERT','SECRET_DELETE') AND state='Pending')`, accountID).Scan(&pending); err != nil {
		return storeError(err)
	}
	if pending {
		return ErrSecretMutationPending
	}
	if checkReferences {
		var referenced bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM apps WHERE account_id=$1 AND $2=ANY(secret_keys))`,
			accountID, key).Scan(&referenced); err != nil {
			return storeError(err)
		}
		if referenced {
			return server.ErrSecretInUse
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO operations(id,account_id,actor_subject,kind,state,detail)
		VALUES($1,$2,$3,$4,'Pending',$5::jsonb)`, opID, accountID, subject, kind, detail); err != nil {
		return storeError(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return storeError(err)
	}

	// Hold the account lock through the Kubernetes update. Application creates,
	// account disablement, and other Secret mutations use the same row lock.
	tx, err = s.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return storeError(err)
	}
	defer tx.Rollback(ctx)
	if err := tx.QueryRow(ctx, `SELECT enabled FROM accounts WHERE id=$1 FOR UPDATE`, accountID).Scan(&enabled); err != nil {
		return storeError(err)
	}
	var state string
	if err := tx.QueryRow(ctx, `SELECT state FROM operations WHERE id=$1 AND account_id=$2`,
		opID, accountID).Scan(&state); err != nil {
		return storeError(err)
	}
	if state != "Pending" {
		return ErrSecretMutationPending
	}
	if !enabled {
		if _, err := tx.Exec(ctx, `UPDATE operations SET state='Failed' WHERE id=$1`, opID); err != nil {
			return storeError(err)
		}
		if err := tx.Commit(ctx); err != nil {
			return storeError(err)
		}
		return server.ErrDisabled
	}
	if checkReferences {
		var referenced bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM apps WHERE account_id=$1 AND $2=ANY(secret_keys))`,
			accountID, key).Scan(&referenced); err != nil {
			return storeError(err)
		}
		if referenced {
			if _, err := tx.Exec(ctx, `UPDATE operations SET state='Failed' WHERE id=$1`, opID); err != nil {
				return storeError(err)
			}
			if err := tx.Commit(ctx); err != nil {
				return storeError(err)
			}
			return server.ErrSecretInUse
		}
	}
	if err := mutate(ctx, opID); err != nil {
		// The Kubernetes update may have succeeded even when its response was
		// lost. Leave the intent Pending for marker-based reconciliation.
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE operations SET state='Succeeded' WHERE id=$1`, opID); err != nil {
		return storeError(err)
	}
	return storeError(tx.Commit(ctx))
}

// ReconcileSecretMutations resolves old intents from an operation marker and
// key presence. It never stores Secret values in PostgreSQL.
func (s *Store) ReconcileSecretMutations(ctx context.Context, probe SecretMutationProbe) (int, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id::text,account_id::text FROM operations
		WHERE kind IN ('SECRET_UPSERT','SECRET_DELETE') AND state='Pending'
		AND created_at < now()-interval '30 seconds' ORDER BY created_at LIMIT 10`)
	if err != nil {
		return 0, storeError(err)
	}
	type intent struct{ id, accountID string }
	var intents []intent
	for rows.Next() {
		var item intent
		if err := rows.Scan(&item.id, &item.accountID); err != nil {
			rows.Close()
			return 0, storeError(err)
		}
		intents = append(intents, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, storeError(err)
	}
	resolved := 0
	var failures []error
	for _, item := range intents {
		ok, err := s.reconcileSecretMutation(ctx, item.accountID, item.id, probe)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if ok {
			resolved++
		}
	}
	return resolved, errors.Join(failures...)
}

func (s *Store) reconcileSecretMutation(ctx context.Context, accountID, opID string,
	probe SecretMutationProbe) (bool, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, storeError(err)
	}
	defer tx.Rollback(ctx)
	var locked int
	if err := tx.QueryRow(ctx, `SELECT 1 FROM accounts WHERE id=$1 FOR UPDATE`, accountID).Scan(&locked); err != nil {
		return false, storeError(err)
	}
	var kind, key, state string
	if err := tx.QueryRow(ctx, `SELECT kind,detail->>'name',state FROM operations
		WHERE id=$1 AND account_id=$2`, opID, accountID).Scan(&kind, &key, &state); err != nil {
		return false, storeError(err)
	}
	if state != "Pending" {
		return false, nil
	}
	if key == "" {
		return false, errors.New("student Secret intent has no key")
	}
	applied, err := probe(ctx, accountID, key, kind, opID)
	if err != nil {
		return false, err
	}
	result := "Failed"
	if applied {
		result = "Succeeded"
	}
	if _, err := tx.Exec(ctx, `UPDATE operations SET state=$2 WHERE id=$1 AND state='Pending'`, opID, result); err != nil {
		return false, storeError(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, storeError(err)
	}
	return true, nil
}
