package buildagent

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client struct {
	endpoint   string
	httpClient *http.Client
	privateKey ed25519.PrivateKey
}

type AgentError struct {
	Status int
	Code   string
}

// Ready checks the authenticated Agent before a database lease is taken.
// An unavailable host must leave queued work untouched for later recovery.
func (c *Client) Ready(ctx context.Context) error {
	if c == nil || c.httpClient == nil {
		return ErrNotReady
	}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(probeCtx, http.MethodGet, c.endpoint+"/healthz", nil)
	if err != nil {
		return ErrNotReady
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return ErrNotReady
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return ErrNotReady
	}
	var health struct {
		Ready       bool `json:"ready"`
		Busy        int  `json:"busy"`
		Quarantined int  `json:"quarantined"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 256))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&health) != nil || decoder.Decode(&struct{}{}) != io.EOF ||
		!health.Ready || health.Busy < 0 || health.Quarantined < 0 ||
		health.Busy+health.Quarantined >= SlotCount {
		return ErrNotReady
	}
	return nil
}

func (e *AgentError) Error() string {
	return fmt.Sprintf("build agent status %d: %s", e.Status, e.Code)
}

func NewClient(endpoint string, tlsConfig *tls.Config, privateKey ed25519.PrivateKey) (*Client, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" ||
		tlsConfig == nil || tlsConfig.InsecureSkipVerify || tlsConfig.MinVersion < tls.VersionTLS13 || tlsConfig.ServerName == "" ||
		len(privateKey) != ed25519.PrivateKeySize {
		return nil, errors.New("invalid build agent client configuration")
	}
	transport := &http.Transport{TLSClientConfig: tlsConfig.Clone(), ForceAttemptHTTP2: true,
		MaxIdleConnsPerHost: 2, IdleConnTimeout: 30 * time.Second}
	return &Client{endpoint: strings.TrimSuffix(endpoint, "/"), httpClient: &http.Client{Transport: transport, Timeout: 18 * time.Minute},
		privateKey: privateKey}, nil
}

func (c *Client) Build(ctx context.Context, request Request) (Result, error) {
	if c == nil || !taskIDPattern.MatchString(request.TaskID) || !taskIDPattern.MatchString(request.AccountID) ||
		request.Attempt < 1 || request.Attempt > 3 || !sha256Pattern.MatchString(request.SourceSHA256) ||
		request.SourceBytes < 1 || request.SourceBytes > 100<<20 || request.Source == nil {
		return Result{}, ErrInvalidTask
	}
	grant := Grant{TaskID: request.TaskID, Attempt: request.Attempt, AccountID: request.AccountID,
		SourceSHA256: request.SourceSHA256, SourceBytes: request.SourceBytes,
		ExpiresUnix: time.Now().Add(4 * time.Minute).Unix()}
	signature, err := SignGrant(c.privateKey, grant)
	if err != nil {
		return Result{}, err
	}
	grantJSON, err := json.Marshal(grant)
	if err != nil {
		return Result{}, err
	}
	body := io.NewSectionReader(request.Source, 0, request.SourceBytes)
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+"/v1/builds", body)
	if err != nil {
		return Result{}, err
	}
	httpRequest.ContentLength = request.SourceBytes
	httpRequest.Header.Set("Content-Type", "application/zip")
	httpRequest.Header.Set("Accept", "application/x-ndjson")
	httpRequest.Header.Set("X-Polygate-Grant", base64.RawURLEncoding.EncodeToString(grantJSON))
	httpRequest.Header.Set("X-Polygate-Signature", signature)
	response, err := c.httpClient.Do(httpRequest)
	if err != nil {
		return Result{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		decoder := json.NewDecoder(io.LimitReader(response.Body, 4096))
		var failure struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if decoder.Decode(&failure) != nil || len(failure.Error.Code) > 64 {
			failure.Error.Code = "AGENT_ERROR"
		}
		return Result{}, &AgentError{Status: response.StatusCode, Code: failure.Error.Code}
	}
	if response.Header.Get("Content-Type") != "application/x-ndjson" {
		return Result{}, errors.New("invalid build agent content type")
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 2*MaxBuildLogBytes+1<<20))
	decoder.DisallowUnknownFields()
	hash := sha256.New()
	var total int64
	for {
		var event buildEvent
		if err := decoder.Decode(&event); err != nil {
			return Result{}, errors.New("incomplete build agent stream")
		}
		switch event.Type {
		case "log":
			if len(event.Data) > 48<<10 || event.Code != "" || event.Result != nil {
				return Result{}, errors.New("invalid build log event")
			}
			data, err := base64.StdEncoding.DecodeString(event.Data)
			if err != nil || total+int64(len(data)) > MaxBuildLogBytes {
				return Result{}, errors.New("invalid build log data")
			}
			total += int64(len(data))
			_, _ = hash.Write(data)
			if request.LogWriter != nil {
				if _, err = request.LogWriter.Write(data); err != nil {
					return Result{}, err
				}
			}
		case "error":
			if event.Code == "" || len(event.Code) > 64 || event.Result != nil || event.Data != "" {
				return Result{}, errors.New("invalid build error event")
			}
			return Result{}, &AgentError{Status: response.StatusCode, Code: event.Code}
		case "result":
			if event.Result == nil || event.Code != "" || event.Data != "" ||
				!sha256Pattern.MatchString(event.Result.ImageDigest) ||
				"sha256:"+hex.EncodeToString(hash.Sum(nil)) != event.Result.LogSHA256 {
				return Result{}, errors.New("invalid build agent result or log digest")
			}
			if decoder.Decode(&struct{}{}) != io.EOF {
				return Result{}, errors.New("extra build agent events")
			}
			return *event.Result, nil
		default:
			return Result{}, errors.New("invalid build agent event")
		}
	}
}
