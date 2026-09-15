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

// sanitize is the ONE exit every failure of this client takes, so no branch of it can
// report a failure without naming the configuration key, and none can report the request.
// net/http wraps a transport failure in a *url.Error carrying the REQUEST URL, and that
// URL carries `apikey=<the credential>` - so the ordinary "log the error" on the fail-open
// path printed the api key on every Tautulli outage. An error STATUS is the same failure
// from the other side: an HTTP 401 from Tautulli MEANS this key is wrong, so the report an
// operator reads has to say which key and which reference to go and fix.
func (t *Tautulli) sanitize(err error) error {
	if err == nil {
		return nil
	}
	cause := err.Error()
	var ue *url.Error
	if errors.As(err, &ue) {
		cause = ue.Op + ": " + unwrapMessage(ue)
	}
	return fmt.Errorf("tautulli: the activity check against %s failed: %s (tautulli_api_key "+
		"is %s; neither the key nor the request URL is reported, because the URL carries the key)",
		t.destination(), cause, label(t.ref))
}

// destination is the most of the configured Tautulli URL a failure may name: scheme, host
// and path, with any userinfo, query and fragment dropped. tautulli_url is not itself a
// secret-bearing key, but an operator may have put basic-auth credentials in it, and the
// query is where this client puts the api key - so the whole of both is withheld rather
// than trusted to be credential-free. A URL too malformed to parse is named by its
// configuration key alone, since its unparsed text is the part under suspicion.
func (t *Tautulli) destination() string {
	u, err := url.Parse(t.baseURL)
	if err != nil {
		return "the configured tautulli_url"
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
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

// httpGet returns its causes bare; Streaming sanitizes them. Naming the key branch by
// branch here instead would double-name the ones that also reach sanitize, and would leave
// every branch added later to remember on its own.
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
		return nil, fmt.Errorf("it answered HTTP %d", resp.StatusCode)
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

// Streaming reports whether Tautulli currently sees at least one active stream. It is the
// client's only caller-facing entry point, which is why it is where sanitize sits.
func (t *Tautulli) Streaming(ctx context.Context) (bool, error) {
	streaming, err := t.streaming(ctx)
	if err != nil {
		return false, t.sanitize(err)
	}
	return streaming, nil
}

// streaming returns its causes bare, for sanitize to name.
func (t *Tautulli) streaming(ctx context.Context) (bool, error) {
	q := url.Values{}
	q.Set("apikey", t.apiKey.Expose())
	q.Set("cmd", "get_activity")
	body, err := t.get(ctx, t.baseURL+"/api/v2?"+q.Encode())
	if err != nil {
		return false, err
	}
	var ar activityResponse
	if err := json.Unmarshal(body, &ar); err != nil {
		return false, fmt.Errorf("its activity payload did not parse: %w", err)
	}
	if ar.Response.Result != "" && ar.Response.Result != "success" {
		return false, fmt.Errorf("it answered result %q", ar.Response.Result)
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
