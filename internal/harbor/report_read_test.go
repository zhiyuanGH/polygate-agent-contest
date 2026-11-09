package harbor

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestReadVulnerabilityReportUsesScanIdentity(t *testing.T) {
	path := "/api/v2.0/projects/test-project/repositories/agent/artifacts/" + testDigest
	currentID := "report-1"
	client, server := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case path:
			fmt.Fprintf(w, `{"digest":%q,"scan_overview":{%q:{"report_id":%q,"scan_status":"Success"}}}`, testDigest, reportMediaType, currentID)
		case path + "/additions/vulnerabilities":
			fmt.Fprint(w, `{"vulnerabilities":[{"severity":"High","id":"CVE-TEST"}]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	defer server.Close()
	ref := RegistryHost + "/test-project/agent@" + testDigest
	// This hash came from the original response. Harbor may later reorder the
	// JSON fields while returning the same scan ID and findings.
	oldHash := "sha256:" + strings.Repeat("b", 64)
	report, err := client.ReadVulnerabilityReport(context.Background(), ref, currentID, oldHash)
	if err != nil || !strings.Contains(string(report), "CVE-TEST") {
		t.Fatalf("report with matching scan identity: %s, %v", report, err)
	}
	currentID = "report-2"
	if _, err := client.ReadVulnerabilityReport(context.Background(), ref, "report-1", oldHash); err == nil {
		t.Fatal("replaced scan must not be returned")
	}
}
