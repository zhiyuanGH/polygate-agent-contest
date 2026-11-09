package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type reportStoreForTest struct {
	*MemoryStore
	archived []byte
}

func (s *reportStoreForTest) GetImageScanReportPointer(_ context.Context, account, id string) (ImageScanReportPointer, error) {
	if account != "student-a" || id != "11111111-1111-4111-8111-111111111111" {
		return ImageScanReportPointer{}, ErrNotFound
	}
	item := ImageScanReportPointer{HarborReference: "harbor/owner/agent@sha256:abc", ReportID: "report-1",
		ReportSHA256: "sha256:abc", ScanStatus: "Rejected", ReportBody: s.archived}
	if len(s.archived) > 0 {
		sum := sha256.Sum256(s.archived)
		item.BodySHA256 = "sha256:" + hex.EncodeToString(sum[:])
	}
	return item, nil
}

func (s *reportStoreForTest) ArchiveImageScanReport(_ context.Context, _, _, _, _ string, body []byte) error {
	s.archived = append([]byte(nil), body...)
	return nil
}

func TestImageScanReportIsOwnerScopedAndDownloadable(t *testing.T) {
	store := &reportStoreForTest{MemoryStore: NewMemoryStore()}
	h := NewHandler(store, func(r *http.Request) (Principal, error) {
		return Principal{AccountID: r.Header.Get("X-Account"), Subject: r.Header.Get("X-Account")}, nil
	}).(*Handler)
	h.SetImageReportReader(func(_ context.Context, ref, reportID, hash string) ([]byte, error) {
		if ref != "harbor/owner/agent@sha256:abc" || reportID != "report-1" || hash != "sha256:abc" {
			t.Fatal("wrong saved report identity")
		}
		return []byte(`{"vulnerabilities":[{"id":"CVE-TEST","severity":"High"}]}`), nil
	})
	path := "/api/v1/images/11111111-1111-4111-8111-111111111111/scan-report"
	foreign := httptest.NewRequest(http.MethodGet, path, nil)
	foreign.Header.Set("X-Account", "student-b")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, foreign)
	if w.Code != http.StatusNotFound {
		t.Fatalf("foreign report: %d %s", w.Code, w.Body.String())
	}
	owned := httptest.NewRequest(http.MethodGet, path+"?download=1", nil)
	owned.Header.Set("X-Account", "student-a")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, owned)
	if w.Code != http.StatusOK || !strings.Contains(w.Header().Get("Content-Disposition"), "attachment") ||
		!strings.Contains(w.Body.String(), "CVE-TEST") || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("owner download: %d %s", w.Code, w.Body.String())
	}
	if len(store.archived) == 0 {
		t.Fatal("Harbor report was not archived")
	}
	// The next request must work from the saved bytes without Harbor.
	h.SetImageReportReader(nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, owned)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "CVE-TEST") {
		t.Fatalf("archived download: %d %s", w.Code, w.Body.String())
	}
}
