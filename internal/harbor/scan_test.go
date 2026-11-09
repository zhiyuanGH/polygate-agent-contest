package harbor

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func testClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	cert, err := x509.ParseCertificate(server.TLS.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
	client, err := New(Config{BaseURL: server.URL, Username: "robot", Password: "secret",
		CACertPEM: ca, PollInterval: time.Millisecond})
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	return client, server
}

func TestScanAndReportBindsDigestAndReport(t *testing.T) {
	var polls atomic.Int32
	var started atomic.Bool
	path := "/api/v2.0/projects/test-project/repositories/agent/artifacts/" + testDigest
	client, server := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if user, password, ok := r.BasicAuth(); !ok || user != "robot" || password != "secret" {
			t.Errorf("missing expected Basic authorization")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("X-Accept-Vulnerabilities") != reportMediaType {
			t.Errorf("missing report media type")
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == path+"/scan":
			started.Store(true)
			w.WriteHeader(http.StatusAccepted)
		case r.Method == http.MethodGet && r.URL.Path == path && r.URL.Query().Get("with_scan_overview") == "true":
			if !started.Load() {
				fmt.Fprintf(w, `{"digest":"%s","scan_overview":{}}`, testDigest)
				return
			}
			state := "Running"
			if polls.Add(1) > 1 {
				state = "Success"
			}
			fmt.Fprintf(w, `{"digest":"%s","scan_overview":{"%s":{"scan_status":"%s","report_id":"report-1","end_time":"%s","scanner":{"name":"Trivy"},"summary":{"total":1,"fixable":1,"summary":{"high":1}}}}}`, testDigest, reportMediaType, state, time.Now().UTC().Format(time.RFC3339Nano))
		case r.Method == http.MethodGet && r.URL.Path == path+"/additions/vulnerabilities":
			fmt.Fprint(w, `{"vulnerabilities":[{"id":"CVE-test"}]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	evidence, err := client.ScanAndReport(ctx, "test-project", "agent", testDigest)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Digest != testDigest || evidence.ReportID != "report-1" ||
		evidence.ScannerName != "Trivy" || evidence.Total != 1 || evidence.Severities["high"] != 1 ||
		!strings.HasPrefix(evidence.ReportSHA256, "sha256:") || polls.Load() != 2 {
		t.Fatalf("unexpected scan evidence: %+v", evidence)
	}
}

func TestScanAndReportFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name, artifact, report string
	}{
		{"scan error", `{"digest":"` + testDigest + `","scan_overview":{"` + reportMediaType + `":{"scan_status":"Error"}}}`, `{}`},
		{"digest mismatch", `{"digest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","scan_overview":{}}`, `{}`},
		{"incomplete success", `{"digest":"` + testDigest + `","scan_overview":{"` + reportMediaType + `":{"scan_status":"Success"}}}`, `{}`},
		{"invalid report", `{"digest":"` + testDigest + `","scan_overview":{"` + reportMediaType + `":{"scan_status":"Success","report_id":"r","end_time":"2026-11-07T00:00:00Z","scanner":{"name":"Trivy"},"summary":{"total":0,"fixable":0,"summary":{}}}}}`, `not JSON`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var started atomic.Bool
			client, server := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					started.Store(true)
					w.WriteHeader(http.StatusAccepted)
					return
				}
				if !started.Load() {
					fmt.Fprintf(w, `{"digest":"%s","scan_overview":{}}`, testDigest)
					return
				}
				if strings.HasSuffix(r.URL.Path, "/additions/vulnerabilities") {
					fmt.Fprint(w, tc.report)
					return
				}
				fmt.Fprint(w, strings.ReplaceAll(tc.artifact, "2026-11-07T00:00:00Z",
					time.Now().UTC().Format(time.RFC3339Nano)))
			})
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if _, err := client.ScanAndReport(ctx, "test-project", "agent", testDigest); err == nil {
				t.Fatal("incomplete or failed scan was accepted")
			}
		})
	}
}

func TestScanAndReportIgnoresPreviousSuccess(t *testing.T) {
	var posted atomic.Bool
	var polls atomic.Int32
	old := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	client, server := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posted.Store(true)
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if posted.Load() {
			polls.Add(1)
		}
		fmt.Fprintf(w, `{"digest":"%s","scan_overview":{"%s":{"scan_status":"Success","report_id":"old-report","end_time":"%s","scanner":{"name":"Trivy"},"summary":{"total":0,"fixable":0,"summary":{}}}}}`, testDigest, reportMediaType, old)
	})
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := client.ScanAndReport(ctx, "test-project", "agent", testDigest); err == nil || polls.Load() < 2 {
		t.Fatalf("stale success report was accepted or not polled: %v, polls=%d", err, polls.Load())
	}
}

func TestHarborClientRejectsRedirectAndUnsafeNames(t *testing.T) {
	client, server := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://another.example/steal", http.StatusTemporaryRedirect)
	})
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := client.ScanAndReport(ctx, "test-project", "agent", testDigest); err == nil {
		t.Fatal("redirect was followed or accepted")
	}
	if _, err := client.ScanAndReport(ctx, "../other", "agent", testDigest); err == nil {
		t.Fatal("unsafe project name was accepted")
	}
	if _, err := client.ScanAndReport(ctx, "test-project", "agent", "latest"); err == nil {
		t.Fatal("mutable reference was accepted")
	}
}

func TestEvidenceDecision(t *testing.T) {
	report := []byte(`{"vulnerabilities":[]}`)
	sum := sha256.Sum256(report)
	base := Evidence{Digest: testDigest, ReportID: "report-1",
		ReportSHA256: "sha256:" + hex.EncodeToString(sum[:]), ScannerName: "Trivy",
		ScannedAt: time.Now(), Report: report, Severities: map[string]int{}}
	for _, tc := range []struct {
		name       string
		total      int
		severities map[string]int
		want       string
	}{
		{"clean", 0, map[string]int{}, "Approved"},
		{"medium", 1, map[string]int{"Medium": 1}, "Approved"},
		{"high", 1, map[string]int{"High": 1}, "Approved"},
		{"critical", 1, map[string]int{"Critical": 1}, "Rejected"},
		{"unknown", 1, map[string]int{"Unknown": 1}, "Rejected"},
		{"unclassified", 1, map[string]int{}, ""},
		{"negative", 1, map[string]int{"Low": -1}, ""},
		{"unsupported", 1, map[string]int{"Custom": 1}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			evidence := base
			evidence.Total = tc.total
			evidence.Severities = tc.severities
			got, err := evidence.Decision()
			if got != tc.want || (err != nil) != (tc.want == "") {
				t.Fatalf("decision=%q err=%v, want %q", got, err, tc.want)
			}
		})
	}
	bad := base
	bad.ReportSHA256 = "sha256:" + strings.Repeat("0", 64)
	if _, err := bad.Decision(); err == nil {
		t.Fatal("forged report hash was accepted")
	}
}
