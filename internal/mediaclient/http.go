// Package mediaclient is holdfast's native clients for the media servers that describe the
// library it rewrites: Radarr, Sonarr and Plex.
//
// It does two things, and both are off until a target's address and credential are
// configured (docs/design/media-clients.md#media-clients, docs/post-swap-hook.md):
//
//   - AFTER a swap has committed, Hook asks the Radarr movie or Sonarr series that owns the
//     file's directory to rescan, and asks Plex for a partial scan of that directory. It is an
//     engine.Observer of already-committed facts: one attempt per target, off the engine's
//     workers, and a failure is one warn record that changes nothing about the job.
//   - BEFORE a swap, PlayHold answers whether Plex is playing a file right now, so the engine
//     can leave that file alone until it stops. It only ever delays, and it fails open.
//
// No request URL ever carries a credential: every key and token travels in a header. No log
// record carries a resolved credential or the text of a request error either, because a Go
// *url.Error quotes the request URL; a failure is reported as a target name and a class.
package mediaclient

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/NSchatz/holdfast/internal/secret"
)

// RequestTimeout bounds every single request this package sends. A target that accepts a
// connection and never answers costs its caller this long and no longer.
const RequestTimeout = 10 * time.Second

// requestTimeout is the bound do applies. Production never changes it; a test shortens it so
// a target that never answers is graded without waiting the shipped bound out.
var requestTimeout = RequestTimeout

// maxBody bounds how much of a response is read. Radarr and Sonarr answer their list
// endpoints with the whole library, so the bound is generous; a body past it is reported as
// unparseable rather than read without limit.
const maxBody = 256 << 20

// Failure classes: the closed vocabulary a failed request is reported under. A record names
// one of these and never the error's own text.
const (
	// ClassTimeout: the target did not answer within RequestTimeout.
	ClassTimeout = "timeout"
	// ClassUnreachable: the connection could not be made or broke (refused, reset, no route,
	// a name that does not resolve, a TLS failure).
	ClassUnreachable = "unreachable"
	// ClassUnauthorized: the target answered 401 or 403, which means the credential is wrong
	// or, for Plex, is not an admin token.
	ClassUnauthorized = "unauthorized"
	// ClassStatus: the target answered any other status outside 200-299.
	ClassStatus = "http-status"
	// ClassUnparseable: the target answered 2xx with a body this build could not read as the
	// documented shape.
	ClassUnparseable = "unparseable-response"
	// ClassRefused: this build declined to send a request it could not make specific (an
	// owner with no usable identifier); nothing was sent.
	ClassRefused = "refused-unspecific-request"
)

// failure is why a request did not succeed: a class, and the status where there was one. It
// deliberately holds no error and no URL.
type failure struct {
	class  string
	status int
}

// String is the class, with the status where the target answered one: "http-status 500".
func (f *failure) String() string {
	if f.status != 0 {
		return f.class + " " + strconv.Itoa(f.status)
	}
	return f.class
}

// caller sends one target's requests: a base address, the header its credential travels in,
// its other headers, and an HTTP client that never follows a redirect (a redirect would carry
// the credential header to an address the operator did not configure).
//
// The credential stays a secret.Value, which renders as `<redacted>` wherever a caller might
// be formatted; it is exposed once per request, into that request's header and nowhere else.
type caller struct {
	base       string
	authHeader string
	credential secret.Value
	headers    map[string]string
	client     *http.Client
}

func newCaller(base, authHeader string, credential secret.Value, headers map[string]string) *caller {
	return &caller{
		base:       base,
		authHeader: authHeader,
		credential: credential,
		headers:    headers,
		client: &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// do sends one request under RequestTimeout and hands a 2xx body to read. Every way it can
// fail comes back as a failure class; the error net/http returned is classified and dropped.
func (c *caller) do(ctx context.Context, method, pathAndQuery string, body io.Reader, read func(io.Reader) error) *failure {
	_, f := c.send(ctx, method, pathAndQuery, body, read)
	return f
}

// send is do, and also answers the HTTP status the target gave: the 2xx of a success, the
// status of a failure that had one, and 0 when the target never answered. The live check
// (livecheck.go) records it; nothing else reads it.
func (c *caller) send(ctx context.Context, method, pathAndQuery string, body io.Reader, read func(io.Reader) error) (int, *failure) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, c.base+pathAndQuery, body)
	if err != nil {
		return 0, &failure{class: ClassRefused}
	}
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	if c.authHeader != "" {
		req.Header.Set(c.authHeader, c.credential.Expose())
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return 0, &failure{class: classify(ctx, err)}
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return resp.StatusCode, &failure{class: ClassUnauthorized, status: resp.StatusCode}
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return resp.StatusCode, &failure{class: ClassStatus, status: resp.StatusCode}
	}
	if read == nil {
		return resp.StatusCode, nil
	}
	if err := read(io.LimitReader(resp.Body, maxBody)); err != nil {
		if ctx.Err() != nil {
			return resp.StatusCode, &failure{class: classify(ctx, err)}
		}
		return resp.StatusCode, &failure{class: ClassUnparseable}
	}
	return resp.StatusCode, nil
}

// classify names a transport error without quoting it.
func classify(ctx context.Context, err error) string {
	var ne net.Error
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) ||
		(errors.As(err, &ne) && ne.Timeout()) {
		return ClassTimeout
	}
	return ClassUnreachable
}
