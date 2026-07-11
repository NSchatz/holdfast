package schedule

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
type activityResponse struct {
	Response struct {
		Result string `json:"result"`
		Data   struct {
			StreamCount json.Number `json:"stream_count"`
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
	n, err := ar.Response.Data.StreamCount.Int64()
	if err != nil {
		// A missing/blank stream_count means no activity data — treat as not streaming.
		return false, nil
	}
	return n > 0, nil
}
