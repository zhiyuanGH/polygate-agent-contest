package buildagent

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"time"
)

const MaxGrantLifetime = 5 * time.Minute

var ErrInvalidGrant = errors.New("invalid build task grant")

// Grant is signed by the worker only after it owns the database lease. The
// agent accepts no command, path, resource size, network, or registry target
// from the grant; those are fixed in the host's deployment configuration.
type Grant struct {
	TaskID       string `json:"taskId"`
	Attempt      int    `json:"attempt"`
	AccountID    string `json:"accountId"`
	SourceSHA256 string `json:"sourceSha256"`
	SourceBytes  int64  `json:"sourceBytes"`
	ExpiresUnix  int64  `json:"expiresUnix"`
}

func (g Grant) validFields() bool {
	return taskIDPattern.MatchString(g.TaskID) && taskIDPattern.MatchString(g.AccountID) &&
		g.Attempt >= 1 && g.Attempt <= 3 && sha256Pattern.MatchString(g.SourceSHA256) &&
		g.SourceBytes >= 1 && g.SourceBytes <= 100<<20 && g.ExpiresUnix > 0
}

func (g Grant) signedBytes() []byte {
	return []byte(fmt.Sprintf("polygate-build-v1\n%s\n%d\n%s\n%s\n%d\n%d\n",
		g.TaskID, g.Attempt, g.AccountID, g.SourceSHA256, g.SourceBytes, g.ExpiresUnix))
}

func SignGrant(privateKey ed25519.PrivateKey, g Grant) (string, error) {
	if len(privateKey) != ed25519.PrivateKeySize || !g.validFields() {
		return "", ErrInvalidGrant
	}
	return base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, g.signedBytes())), nil
}

func VerifyGrant(publicKey ed25519.PublicKey, g Grant, signature string, now time.Time) error {
	if len(publicKey) != ed25519.PublicKeySize || !g.validFields() ||
		g.ExpiresUnix <= now.Unix() || g.ExpiresUnix > now.Add(MaxGrantLifetime).Unix() {
		return ErrInvalidGrant
	}
	bytes, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil || len(bytes) != ed25519.SignatureSize || !ed25519.Verify(publicKey, g.signedBytes(), bytes) {
		return ErrInvalidGrant
	}
	return nil
}
