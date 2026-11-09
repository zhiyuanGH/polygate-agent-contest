package buildagent

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestGrantBindsLeaseOwnerFieldsAndExpires(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Second)
	g := Grant{TaskID: "11111111-1111-4111-8111-111111111111", Attempt: 2,
		AccountID:    "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		SourceSHA256: "sha256:" + strings.Repeat("b", 64), SourceBytes: 123,
		ExpiresUnix: now.Add(4 * time.Minute).Unix()}
	signature, err := SignGrant(private, g)
	if err != nil {
		t.Fatal(err)
	}
	if err = VerifyGrant(public, g, signature, now); err != nil {
		t.Fatal(err)
	}
	modified := g
	modified.AccountID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	if err = VerifyGrant(public, modified, signature, now); !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("account swap: %v", err)
	}
	modified = g
	modified.Attempt++
	if err = VerifyGrant(public, modified, signature, now); !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("attempt swap: %v", err)
	}
	modified = g
	modified.SourceSHA256 = "sha256:" + strings.Repeat("c", 64)
	if err = VerifyGrant(public, modified, signature, now); !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("source swap: %v", err)
	}
	modified = g
	modified.SourceBytes++
	if err = VerifyGrant(public, modified, signature, now); !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("source length swap: %v", err)
	}
	if err = VerifyGrant(public, g, signature, now.Add(5*time.Minute)); !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("expired grant: %v", err)
	}
	modified = g
	modified.ExpiresUnix = now.Add(10 * time.Minute).Unix()
	futureSig, err := SignGrant(private, modified)
	if err != nil {
		t.Fatal(err)
	}
	if err = VerifyGrant(public, modified, futureSig, now); !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("long-lived grant: %v", err)
	}
}
