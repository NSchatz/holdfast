package schedule

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/NSchatz/holdfast/internal/secret"
)

// Tautulli is a minimal client for the Tautulli (Plex monitoring) API — just enough
// to ask "is anyone streaming right now?" so the scheduler can pause transcoding
// during playback. It is entirely optional; New returns nil when unconfigured.
type Tautulli struct {
	baseURL string
	apiKey  secret.Value
	ref     secret.Ref
	client  *http.Client

	// get is a seam for tests; production issues the real HTTP GET.
	get func(ctx context.Context, rawURL string) ([]byte, error)
}

// NewTautulli builds a client from a base URL and a RESOLVED api key (secrets K1), or
// returns nil if either is empty (the feature is off unless the operator supplies both).
// The reference is kept for the failure path: Tautulli takes its api key in the QUERY
// STRING, so the request URL is credential-bearing and no error may quote it.
func NewTautulli(baseURL string, ref secret.Ref, apiKey secret.Value) *Tautulli {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" || apiKey.Empty() {
		return nil
	}
	t := &Tautulli{
		baseURL: baseURL,
		apiKey:  apiKey,
		ref:     ref,
		client:  &http.Client{Timeout: 5 * time.Second},
	}
	t.get = t.httpGet
	return t
}

// sanitize replaces a transport error with one that names the configuration key and its
// reference instead. net/http wraps every failure in a *url.Error carrying the REQUEST
// URL, and that URL carries `apikey=<the credential>` - so the ordinary "log the error"
// on the fail-open path printed the api key on every Tautulli outage. The failure the
// operator needs to see is "the Tautulli call failed", and the key that decides it is the
// reference; neither needs the destination's query string.
func (t *Tautulli) sanitize(err error) error {
	if err == nil {
		return nil
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("tautulli: the %s request to %s failed: %s (tautulli_api_key is %s; "+
			"neither the key nor the request URL is reported, because the URL carries the key)",
			ue.Op, t.baseURL, unwrapMessage(ue), label(t.ref))
	}
	return err
}

// unwrapMessage is the innermost cause of a *url.Error, which is the part that says WHAT
// went wrong (connection refused, i/o timeout, no such host) without the URL the outer
// error prepends.
func unwrapMessage(ue *url.Error) string {
	if inner := errors.Unwrap(ue); inner != nil {
		return inner.Error()
	}
	return "request failed"
}

// label names a secret-bearing key and the reference it carries, never its value.
func label(ref secret.Ref) string {
	if !ref.Configured() {
		return "not configured"
	}
	return ref.String()
}

func (t *Tautulli) httpGet(ctx context.Context, rawURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, t.sanitize(err)
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, t.sanitize(err)
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
	q.Set("apikey", t.apiKey.Expose())
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
