package schedule

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Tautulli is a minimal client for the Tautulli (Plex monitoring) API — just enough
// to ask "is anyone streaming right now?" so the scheduler can pause transcoding
// during playback. It is entirely optional; New returns nil when unconfigured.
type Tautulli struct {
	baseURL string
	apiKey  string
	client  *http.Client

	// get is a seam for tests; production issues the real HTTP GET.
	get func(ctx context.Context, rawURL string) ([]byte, error)
}

// NewTautulli builds a client, or returns nil if either the base URL or API key is
// empty (the feature is off unless the operator supplies both).
func NewTautulli(baseURL, apiKey string) *Tautulli {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	apiKey = strings.TrimSpace(apiKey)
	if baseURL == "" || apiKey == "" {
		return nil
	}
	t := &Tautulli{
		baseURL: baseURL,
		apiKey:  apiKey,
		client:  &http.Client{Timeout: 5 * time.Second},
	}
	t.get = t.httpGet
	return t
}

func (t *Tautulli) httpGet(ctx context.Context, rawURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tautulli returned HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

// activityResponse is the subset of Tautulli's get_activity payload we read.
//
// stream_count is captured as RawMessage, NOT json.Number, deliberately. Real
// Tautulli deployments send it in several shapes — a JSON number (2), a numeric
// STRING ("2"), an empty string (""), null, or the field omitted entirely — and a
// json.Number field makes the WHOLE response fail to unmarshal on the empty-string
// case, turning an ordinary idle response into a spurious error. RawMessage never
// fails to unmarshal; streamCount interprets the shape, so "blank = not streaming"
// is actually reachable rather than a dead comment.
type activityResponse struct {
	Response struct {
		Result string `json:"result"`
		Data   struct {
			StreamCount json.RawMessage `json:"stream_count"`
		} `json:"data"`
	} `json:"response"`
}

// Streaming reports whether Tautulli currently sees at least one active stream.
func (t *Tautulli) Streaming(ctx context.Context) (bool, error) {
	q := url.Values{}
	q.Set("apikey", t.apiKey)
	q.Set("cmd", "get_activity")
	body, err := t.get(ctx, t.baseURL+"/api/v2?"+q.Encode())
	if err != nil {
		return false, err
	}
	var ar activityResponse
	if err := json.Unmarshal(body, &ar); err != nil {
		return false, fmt.Errorf("tautulli: parse activity: %w", err)
	}
	if ar.Response.Result != "" && ar.Response.Result != "success" {
		return false, fmt.Errorf("tautulli: result %q", ar.Response.Result)
	}
	return streamCount(ar.Response.Data.StreamCount) > 0, nil
}

// streamCount extracts a non-negative active-stream count from Tautulli's
// stream_count field. It tolerates every shape a real deployment sends: a bare
// number (2), a numeric string ("2"), an empty string (""), null, or an omitted
// field. Only the two numeric shapes carry a count; every other shape means "no
// activity data" and reads as 0 — the fail-safe "blank means nobody is streaming"
// intent, which a json.Number field silently defeated by failing the parse first.
// Any value it cannot make sense of is treated as 0 (not streaming): a monitor that
// returns garbage must never permanently block transcoding.
func streamCount(raw json.RawMessage) int64 {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return 0
	}
	// Unwrap a JSON string ("2" or "") to its contents; a bare number (2) has no
	// quotes and is parsed directly.
	s := string(raw)
	if raw[0] == '"' {
		if err := json.Unmarshal(raw, &s); err != nil {
			return 0
		}
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}
