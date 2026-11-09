package metricsrotate

import (
	"bytes"
	"crypto/x509"
	"testing"
	"time"
)

func TestGenerateServingCertificateAndBundle(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	first, firstKey, err := Generate(now)
	if err != nil {
		t.Fatal(err)
	}
	second, secondKey, err := Generate(now)
	if err != nil {
		t.Fatal(err)
	}
	oldCert, err := Validate(first, firstKey, now)
	if err != nil {
		t.Fatal(err)
	}
	newCert, err := Validate(second, secondKey, now)
	if err != nil {
		t.Fatal(err)
	}
	if Fingerprint(oldCert) == Fingerprint(newCert) {
		t.Fatal("rotation reused the old certificate")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(second) {
		t.Fatal("generated certificate is not usable as a trust anchor")
	}
	if _, err := newCert.Verify(x509.VerifyOptions{DNSName: ServiceName, Roots: pool, CurrentTime: now}); err != nil {
		t.Fatal(err)
	}
	bundle, err := Bundle(append(bytes.Clone(first), second...))
	if err != nil || len(bundle) != 2 || Fingerprint(bundle[0]) != Fingerprint(oldCert) ||
		Fingerprint(bundle[1]) != Fingerprint(newCert) {
		t.Fatalf("invalid dual-trust bundle: %v", err)
	}
	if _, err := Bundle(append(bytes.Clone(first), []byte("untrusted trailer")...)); err == nil {
		t.Fatal("trailing data in trust bundle was accepted")
	}
	if _, err := Validate(first, secondKey, now); err == nil {
		t.Fatal("mismatched private key was accepted")
	}
}
