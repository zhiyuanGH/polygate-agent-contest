package buildagent

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type protocolRunner struct {
	calls atomic.Int32
	fail  atomic.Bool
}

func (p *protocolRunner) StopAndClean(context.Context, int) error { return nil }
func (p *protocolRunner) Execute(_ context.Context, _ int, request Request) (Result, error) {
	p.calls.Add(1)
	body := make([]byte, request.SourceBytes)
	if _, err := request.Source.ReadAt(body, 0); err != nil && err != io.EOF {
		return Result{}, err
	}
	hash := sha256.Sum256(body)
	if request.SourceSHA256 != "sha256:"+hex.EncodeToString(hash[:]) {
		return Result{}, ErrInvalidTask
	}
	logData := []byte("test build output\n")
	if request.LogWriter != nil {
		if _, err := request.LogWriter.Write(logData); err != nil {
			return Result{}, err
		}
	}
	logHash := sha256.Sum256(logData)
	if p.fail.Load() {
		return Result{}, errors.New("guest build failed")
	}
	return Result{ImageDigest: "sha256:" + strings.Repeat("a", 64), LogSHA256: "sha256:" + hex.EncodeToString(logHash[:])}, nil
}

func certificatePEM(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, clientURI string, server bool) ([]byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "polygate-test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature}
	if server {
		template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		template.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
	}
	if !server {
		template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
		u, _ := url.Parse(clientURI)
		template.URIs = []*url.URL{u}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	encodedKey, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encodedKey})
}

func testCA(t *testing.T) ([]byte, *x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "polygate-test-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), parsed, key
}

func zipBytes(t *testing.T) []byte {
	t.Helper()
	var body bytes.Buffer
	z := zip.NewWriter(&body)
	f, err := z.Create("Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.Write([]byte("FROM scratch\n"))
	if err = z.Close(); err != nil {
		t.Fatal(err)
	}
	return body.Bytes()
}

func TestMTLSBuildProtocolRejectsOtherPeersAndTamperedSource(t *testing.T) {
	caPEM, ca, caKey := testCA(t)
	serverCert, serverKey := certificatePEM(t, ca, caKey, "", true)
	workerCert, workerKey := certificatePEM(t, ca, caKey, WorkerURI, false)
	otherCert, otherKey := certificatePEM(t, ca, caKey, "spiffe://polygate/other-service", false)
	serverTLS, err := ServerTLSConfig(serverCert, serverKey, caPEM)
	if err != nil {
		t.Fatal(err)
	}
	workerTLS, err := WorkerTLSConfig(workerCert, workerKey, caPEM, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	otherTLS, err := WorkerTLSConfig(otherCert, otherKey, caPEM, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	runner := &protocolRunner{}
	manager := NewManager(runner)
	if err = manager.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	handler, err := NewHandler(manager, public, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.TLS = serverTLS
	server.StartTLS()
	defer server.Close()
	worker := &http.Client{Transport: &http.Transport{TLSClientConfig: workerTLS}}
	other := &http.Client{Transport: &http.Transport{TLSClientConfig: otherTLS}}
	body := zipBytes(t)
	hash := sha256.Sum256(body)
	grant := Grant{TaskID: "11111111-1111-4111-8111-111111111111", Attempt: 1,
		AccountID:    "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		SourceSHA256: "sha256:" + hex.EncodeToString(hash[:]), SourceBytes: int64(len(body)),
		ExpiresUnix: time.Now().Add(4 * time.Minute).Unix()}
	send := func(client *http.Client, g Grant, sourceBody []byte, signature string) int {
		t.Helper()
		encoded, _ := json.Marshal(g)
		req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/builds", bytes.NewReader(sourceBody))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/zip")
		req.Header.Set("X-Polygate-Grant", base64.RawURLEncoding.EncodeToString(encoded))
		req.Header.Set("X-Polygate-Signature", signature)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}
	signature, err := SignGrant(private, grant)
	if err != nil {
		t.Fatal(err)
	}
	if code := send(worker, grant, body, signature); code != http.StatusOK {
		t.Fatalf("valid build: %d", code)
	}
	if code := send(worker, grant, body, signature); code != http.StatusConflict {
		t.Fatalf("replayed grant: %d", code)
	}
	if code := send(other, grant, body, signature); code != http.StatusForbidden {
		t.Fatalf("other client URI: %d", code)
	}
	altered := grant
	altered.AccountID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	if code := send(worker, altered, body, signature); code != http.StatusForbidden {
		t.Fatalf("tampered account: %d", code)
	}
	if code := send(worker, grant, []byte("wrong bytes"), signature); code != http.StatusBadRequest {
		t.Fatalf("tampered ZIP: %d", code)
	}
	if got := runner.calls.Load(); got != 1 {
		t.Fatalf("runner executed %d times", got)
	}
}

func TestWorkerClientRoundTripOverMutualTLS(t *testing.T) {
	caPEM, ca, caKey := testCA(t)
	serverCert, serverKey := certificatePEM(t, ca, caKey, "", true)
	workerCert, workerKey := certificatePEM(t, ca, caKey, WorkerURI, false)
	serverTLS, err := ServerTLSConfig(serverCert, serverKey, caPEM)
	if err != nil {
		t.Fatal(err)
	}
	workerTLS, err := WorkerTLSConfig(workerCert, workerKey, caPEM, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	runner := &protocolRunner{}
	manager := NewManager(runner)
	if err = manager.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	handler, err := NewHandler(manager, public, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.TLS = serverTLS
	server.StartTLS()
	defer server.Close()
	client, err := NewClient(server.URL, workerTLS, private)
	if err != nil {
		t.Fatal(err)
	}
	if err = client.Ready(context.Background()); err != nil {
		t.Fatalf("recovered agent readiness: %v", err)
	}
	body := zipBytes(t)
	hash := sha256.Sum256(body)
	var logOutput bytes.Buffer
	request := Request{TaskID: "11111111-1111-4111-8111-111111111111", Attempt: 1,
		AccountID:    "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		SourceSHA256: "sha256:" + hex.EncodeToString(hash[:]), SourceBytes: int64(len(body)), Source: bytes.NewReader(body), LogWriter: &logOutput}
	result, err := client.Build(context.Background(), request)
	if err != nil || !sha256Pattern.MatchString(result.ImageDigest) {
		t.Fatalf("worker build: %+v %v", result, err)
	}
	if runner.calls.Load() != 1 {
		t.Fatalf("runner calls: %d", runner.calls.Load())
	}
	if logOutput.String() != "test build output\n" {
		t.Fatalf("log output: %q", logOutput.String())
	}
	runner.fail.Store(true)
	var failedLog bytes.Buffer
	request.TaskID = "22222222-2222-4222-8222-222222222222"
	request.LogWriter = &failedLog
	_, err = client.Build(context.Background(), request)
	var agentErr *AgentError
	if !errors.As(err, &agentErr) || agentErr.Code != "BUILD_FAILED" || failedLog.String() != "test build output\n" {
		t.Fatalf("failed build log: %q %v", failedLog.String(), err)
	}
}
