package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"testing"
)

func TestListenAddressRejectsWildcardAndNonIP(t *testing.T) {
	for _, value := range []string{"0.0.0.0:8443", "127.0.0.1:8443", "localhost:8443", ":8443", "192.0.2.142:0"} {
		if _, err := listenAddress(value); err == nil {
			t.Fatalf("unsafe listener accepted: %s", value)
		}
	}
	if got, err := listenAddress("192.0.2.142:8443"); err != nil || got != "192.0.2.142:8443" {
		t.Fatalf("fixed listener rejected: %q %v", got, err)
	}
}

func TestParseSigningPublicKeyRequiresEd25519(t *testing.T) {
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	encoded := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	parsed, err := parseSigningPublicKey(encoded)
	if err != nil || !parsed.Equal(public) {
		t.Fatalf("valid signing key rejected: %v", err)
	}
	if _, err = parseSigningPublicKey(append(encoded, []byte("extra")...)); err == nil {
		t.Fatal("trailing key material accepted")
	}
}
