package harbor

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
)

// ReadVulnerabilityReport returns the current Harbor report only while its
// immutable image digest and scan ID match the saved scan evidence. Harbor can
// serialize the same findings in a different order on later requests, so the
// raw response hash is not a stable report identity.
func (c *Client) ReadVulnerabilityReport(ctx context.Context, harborReference, reportID, reportSHA256 string) ([]byte, error) {
	ref, err := name.NewDigest(harborReference, name.StrictValidation)
	if err != nil || ref.Context().RegistryStr() != RegistryHost ||
		reportID == "" || !digestPattern.MatchString(reportSHA256) {
		return nil, errors.New("invalid image scan report identity")
	}
	parts := strings.Split(ref.Context().RepositoryStr(), "/")
	if len(parts) != 2 || parts[1] != "agent" || !simpleName.MatchString(parts[0]) {
		return nil, errors.New("image report is outside an account project")
	}
	path, err := artifactPath(parts[0], parts[1], ref.DigestStr())
	if err != nil {
		return nil, err
	}
	status, body, err := c.request(ctx, http.MethodGet, path+"?with_scan_overview=true", nil)
	if err != nil || status != http.StatusOK {
		return nil, errors.New("Harbor scan overview is unavailable")
	}
	var overview struct {
		Digest       string `json:"digest"`
		ScanOverview map[string]struct {
			ReportID   string `json:"report_id"`
			ScanStatus string `json:"scan_status"`
		} `json:"scan_overview"`
	}
	if json.Unmarshal(body, &overview) != nil || overview.Digest != ref.DigestStr() ||
		overview.ScanOverview[reportMediaType].ReportID != reportID ||
		overview.ScanOverview[reportMediaType].ScanStatus != "Success" {
		return nil, errors.New("Harbor scan evidence changed")
	}
	status, report, err := c.request(ctx, http.MethodGet, path+"/additions/vulnerabilities", nil)
	if err != nil || status != http.StatusOK || len(report) == 0 || !json.Valid(report) {
		return nil, errors.New("Harbor vulnerability report is unavailable")
	}
	return report, nil
}
