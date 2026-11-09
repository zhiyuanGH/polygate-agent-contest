package harbor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const reportMediaType = "application/vnd.security.vulnerability.report; version=1.1"
const maxReportBytes = 8 << 20

// PolicyVersion identifies the automatic decision rules stored with each
// completed scan. Deployment admission must match this exact version.
const PolicyVersion = "severity-v2"

var simpleName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)
var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

type Config struct {
	BaseURL      string
	Username     string
	Password     string
	CACertPEM    []byte
	DialContext  func(context.Context, string, string) (net.Conn, error)
	PollInterval time.Duration
}

type Client struct {
	base         string
	username     string
	password     string
	http         *http.Client
	pollInterval time.Duration
}

// Evidence is a completed scan, tied to one immutable Harbor manifest digest.
// Scan completion alone does not grant deploy permission.
type Evidence struct {
	Digest       string
	ReportID     string
	ReportSHA256 string
	ScannerName  string
	ScannedAt    time.Time
	Total        int
	Fixable      int
	Severities   map[string]int
	Report       json.RawMessage
}

// Decision rejects Critical and Unknown findings. High findings are recorded
// but allowed for the trusted pilot. Unclassified evidence fails validation.
func (e Evidence) Decision() (string, error) {
	if !digestPattern.MatchString(e.Digest) || e.ReportID == "" || e.ScannerName == "" ||
		e.ScannedAt.IsZero() || e.Total < 0 || e.Fixable < 0 || e.Fixable > e.Total ||
		e.Severities == nil || len(e.Report) == 0 || !json.Valid(e.Report) {
		return "", errors.New("scan evidence is incomplete")
	}
	sum := sha256.Sum256(e.Report)
	if e.ReportSHA256 != "sha256:"+hex.EncodeToString(sum[:]) {
		return "", errors.New("scan report digest mismatch")
	}
	counted := 0
	reject := false
	for severity, count := range e.Severities {
		if count < 0 || count > e.Total || counted > e.Total-count {
			return "", errors.New("scan severity counts are invalid")
		}
		counted += count
		switch strings.ToLower(severity) {
		case "critical", "unknown":
			reject = reject || count > 0
		case "high", "medium", "low", "negligible", "none":
		default:
			return "", fmt.Errorf("unsupported scan severity %q", severity)
		}
	}
	if counted != e.Total {
		return "", errors.New("scan severity counts do not match total")
	}
	if reject {
		return "Rejected", nil
	}
	return "Approved", nil
}

func New(cfg Config) (*Client, error) {
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil ||
		u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("Harbor URL must be an HTTPS origin")
	}
	if cfg.Username == "" || cfg.Password == "" || len(cfg.CACertPEM) == 0 {
		return nil, errors.New("Harbor credentials and CA are required")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(cfg.CACertPEM) {
		return nil, errors.New("Harbor CA is invalid")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}
	if cfg.DialContext != nil {
		transport.DialContext = cfg.DialContext
	}
	interval := cfg.PollInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	return &Client{base: strings.TrimSuffix(cfg.BaseURL, "/"), username: cfg.Username,
		password: cfg.Password, pollInterval: interval, http: &http.Client{
			Transport: transport, Timeout: 30 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return errors.New("Harbor API redirect refused")
			},
		}}, nil
}

func (c *Client) request(ctx context.Context, method, path string, data []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(data))
	if err != nil {
		return 0, nil, err
	}
	req.SetBasicAuth(c.username, c.password)
	if data != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Accept-Vulnerabilities", reportMediaType)
	response, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("Harbor API request failed: %w", err)
	}
	defer response.Body.Close()
	if response.ContentLength > maxReportBytes {
		return response.StatusCode, nil, errors.New("Harbor API response is too large")
	}
	limited := io.LimitReader(response.Body, maxReportBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return response.StatusCode, nil, fmt.Errorf("read Harbor response: %w", err)
	}
	if len(body) > maxReportBytes {
		return response.StatusCode, nil, errors.New("Harbor API response is too large")
	}
	return response.StatusCode, body, nil
}

func artifactPath(project, repository, digest string) (string, error) {
	if !simpleName.MatchString(project) || !simpleName.MatchString(repository) || !digestPattern.MatchString(digest) {
		return "", errors.New("invalid Harbor project, repository or digest")
	}
	return "/api/v2.0/projects/" + project + "/repositories/" + repository + "/artifacts/" + digest, nil
}

// ScanAndReport triggers a vulnerability scan, waits for Success and retrieves
// the report. Error, missing report, digest mismatch and timeout all fail closed.
func (c *Client) ScanAndReport(ctx context.Context, project, repository, digest string) (Evidence, error) {
	path, err := artifactPath(project, repository, digest)
	if err != nil {
		return Evidence{}, err
	}
	status, body, err := c.request(ctx, http.MethodGet, path+"?with_scan_overview=true", nil)
	if err != nil || status != http.StatusOK {
		return Evidence{}, fmt.Errorf("Harbor artifact state unavailable before scan (status %d): %w", status, err)
	}
	var previous struct {
		Digest       string `json:"digest"`
		ScanOverview map[string]struct {
			ReportID string    `json:"report_id"`
			EndTime  time.Time `json:"end_time"`
		} `json:"scan_overview"`
	}
	if json.Unmarshal(body, &previous) != nil || previous.Digest != digest {
		return Evidence{}, errors.New("Harbor artifact changed before scan")
	}
	old := previous.ScanOverview[reportMediaType]
	startedAt := time.Now().UTC()
	status, _, err = c.request(ctx, http.MethodPost, path+"/scan", []byte(`{"scan_type":"vulnerability"}`))
	if err != nil || status != http.StatusAccepted {
		return Evidence{}, fmt.Errorf("Harbor scan was not accepted (status %d): %w", status, err)
	}
	for {
		status, body, err := c.request(ctx, http.MethodGet, path+"?with_scan_overview=true", nil)
		if err != nil || status != http.StatusOK {
			return Evidence{}, fmt.Errorf("Harbor scan status unavailable (status %d): %w", status, err)
		}
		var artifact struct {
			Digest       string `json:"digest"`
			ScanOverview map[string]struct {
				ReportID   string    `json:"report_id"`
				ScanStatus string    `json:"scan_status"`
				EndTime    time.Time `json:"end_time"`
				Summary    *struct {
					Total      int            `json:"total"`
					Fixable    int            `json:"fixable"`
					Severities map[string]int `json:"summary"`
				} `json:"summary"`
				Scanner struct {
					Name string `json:"name"`
				} `json:"scanner"`
			} `json:"scan_overview"`
		}
		if err := json.Unmarshal(body, &artifact); err != nil {
			return Evidence{}, errors.New("Harbor returned invalid scan metadata")
		}
		if artifact.Digest != digest {
			return Evidence{}, errors.New("Harbor artifact digest changed during scan")
		}
		for mediaType, overview := range artifact.ScanOverview {
			if mediaType != reportMediaType {
				continue
			}
			switch overview.ScanStatus {
			case "Success":
				if overview.EndTime.Before(startedAt.Add(-5*time.Second)) ||
					(old.ReportID != "" && overview.ReportID == old.ReportID &&
						!overview.EndTime.After(old.EndTime)) {
					break
				}
				if overview.ReportID == "" || overview.EndTime.IsZero() || overview.Scanner.Name == "" ||
					overview.Summary == nil || overview.Summary.Severities == nil ||
					overview.Summary.Total < 0 || overview.Summary.Fixable < 0 {
					return Evidence{}, errors.New("Harbor scan evidence is incomplete")
				}
				status, report, err := c.request(ctx, http.MethodGet, path+"/additions/vulnerabilities", nil)
				if err != nil || status != http.StatusOK || len(report) == 0 || !json.Valid(report) {
					return Evidence{}, fmt.Errorf("Harbor vulnerability report unavailable (status %d): %w", status, err)
				}
				sha := sha256.Sum256(report)
				return Evidence{Digest: digest, ReportID: overview.ReportID,
					ReportSHA256: "sha256:" + hex.EncodeToString(sha[:]), ScannerName: overview.Scanner.Name,
					ScannedAt: overview.EndTime, Total: overview.Summary.Total,
					Fixable: overview.Summary.Fixable, Severities: overview.Summary.Severities,
					Report: report}, nil
			case "Error", "Stopped":
				return Evidence{}, fmt.Errorf("Harbor scan ended in %s", overview.ScanStatus)
			case "Pending", "Running", "Scheduled", "Queued":
			default:
				return Evidence{}, fmt.Errorf("Harbor returned unknown scan status %q", overview.ScanStatus)
			}
		}
		select {
		case <-ctx.Done():
			return Evidence{}, ctx.Err()
		case <-time.After(c.pollInterval):
		}
	}
}
