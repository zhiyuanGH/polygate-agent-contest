package applogs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

var ErrHistoryDisabled = errors.New("historical app logs disabled")
var ErrInvalidHistoryCursor = errors.New("invalid historical log cursor")

type HistoryLine struct {
	Timestamp string `json:"timestamp"`
	Pod       string `json:"pod"`
	Container string `json:"container"`
	Line      string `json:"line"`
}

type HistoryResult struct {
	Items      []HistoryLine `json:"items"`
	RangeHours int           `json:"rangeHours"`
	NextBefore string        `json:"nextBefore,omitempty"`
}

func (r *Reader) History(ctx context.Context, accountID, appID string, hours int, before string) (HistoryResult, error) {
	if r.lokiURL == "" {
		return HistoryResult{}, ErrHistoryDisabled
	}
	if hours != 24 && hours != 168 {
		return HistoryResult{}, fmt.Errorf("invalid history range")
	}
	if _, err := uuid.Parse(accountID); err != nil {
		return HistoryResult{}, fmt.Errorf("invalid account identifier")
	}
	if _, err := uuid.Parse(appID); err != nil {
		return HistoryResult{}, fmt.Errorf("invalid app identifier")
	}
	endpoint, err := url.Parse(strings.TrimRight(r.lokiURL, "/") + "/loki/api/v1/query_range")
	if err != nil || endpoint.Scheme != "http" || endpoint.Host == "" {
		return HistoryResult{}, fmt.Errorf("invalid Loki endpoint")
	}
	now := time.Now().UTC()
	end := now.UnixNano()
	if before != "" {
		value, err := strconv.ParseInt(before, 10, 64)
		if err != nil || value <= now.Add(-time.Duration(hours)*time.Hour).UnixNano() || value > end {
			return HistoryResult{}, ErrInvalidHistoryCursor
		}
		end = value - 1
	}
	query := endpoint.Query()
	query.Set("query", `{account_id="`+accountID+`",app_id="`+appID+`"}`)
	query.Set("start", strconv.FormatInt(now.Add(-time.Duration(hours)*time.Hour).UnixNano(), 10))
	query.Set("end", strconv.FormatInt(end, 10))
	query.Set("limit", "200")
	query.Set("direction", "backward")
	endpoint.RawQuery = query.Encode()
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return HistoryResult{}, err
	}
	client := r.lokiHTTP
	if client == nil {
		client = &http.Client{Timeout: 8 * time.Second}
	}
	response, err := client.Do(request)
	if err != nil {
		return HistoryResult{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return HistoryResult{}, fmt.Errorf("Loki query returned HTTP %d", response.StatusCode)
	}
	const maxBody = 2 << 20
	body, err := io.ReadAll(io.LimitReader(response.Body, maxBody+1))
	if err != nil || len(body) > maxBody {
		return HistoryResult{}, fmt.Errorf("Loki query response too large or unreadable")
	}
	var decoded struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Stream map[string]string `json:"stream"`
				Values [][]string        `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil || decoded.Status != "success" {
		return HistoryResult{}, fmt.Errorf("invalid Loki query response")
	}
	type entry struct {
		at   int64
		line HistoryLine
	}
	items := make([]entry, 0, 200)
	for _, stream := range decoded.Data.Result {
		if stream.Stream["account_id"] != accountID || stream.Stream["app_id"] != appID {
			continue
		}
		for _, value := range stream.Values {
			if len(value) != 2 {
				continue
			}
			at, err := strconv.ParseInt(value[0], 10, 64)
			if err != nil {
				continue
			}
			line := value[1]
			if len(line) > 4096 {
				line = line[:4096]
				for !utf8.ValidString(line) {
					line = line[:len(line)-1]
				}
				line += "…"
			}
			items = append(items, entry{at: at, line: HistoryLine{
				Timestamp: time.Unix(0, at).UTC().Format(time.RFC3339Nano),
				Pod:       stream.Stream["pod"], Container: stream.Stream["container"], Line: line,
			}})
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].at > items[j].at })
	result := HistoryResult{Items: make([]HistoryLine, 0, 200), RangeHours: hours}
	bytesUsed := 0
	lastTimestamp := int64(0)
	for _, item := range items {
		if len(result.Items) == 200 || bytesUsed+len(item.line.Line) > 128<<10 {
			break
		}
		bytesUsed += len(item.line.Line)
		result.Items = append(result.Items, item.line)
		lastTimestamp = item.at
	}
	if lastTimestamp > 0 && (len(items) >= 200 || len(result.Items) < len(items)) {
		result.NextBefore = strconv.FormatInt(lastTimestamp, 10)
	}
	return result, nil
}
