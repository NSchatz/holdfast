package nodeworker

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/NSchatz/holdfast/internal/node"
)

// The source download of http mode (docs/design/nodes.md#http-mode).
//
// A response is media ONLY when it is a 200 that delivers exactly the leased source_size
// bytes. An error page is never media: a 404 with a body, a 200 that declares another
// length, a body that ends early and one that runs long all leave nothing to encode. What
// arrives is hashed as it is written, and that sha-256 is what the worker reports in
// `complete`, where the server holds it to the digest of what it streamed.

// The download's own bounds. Both are ASSUMED: nobody has measured a node deployment.
const (
	// MaxSourceAttempts bounds how many times one lease's source is downloaded. A failed
	// download starts again from the first byte; it is never resumed with a Range.
	MaxSourceAttempts = 3
	// DefaultSourceIdle is how long a download may go without a byte arriving before the
	// worker cuts it.
	DefaultSourceIdle = 60 * time.Second
	// sourceChunk is the buffer the body is copied through.
	sourceChunk = 1 << 20
	// maxRefusal bounds how much of a refusal's body is read for its typed reason.
	maxRefusal = 4 << 10
)

// errSourceGone is a source request answered 410: the lease is gone.
var errSourceGone = errors.New("the lease is gone")

// sourceRefusal is one failed download attempt: why, whether another attempt may follow,
// and the Retry-After the server stated.
type sourceRefusal struct {
	why   string
	again bool
	wait  time.Duration
}

func (e *sourceRefusal) Error() string { return e.why }

// roomFor reports whether the work directory can hold a lease's source and the largest
// output it may produce. A failed lookup refuses nothing, as the server's own checks do not.
func roomFor(freeSpace func(string) (uint64, error), dir string, l *node.AcquireResponse) (need uint64, ok bool) {
	need = uint64(l.SourceSize) + uint64(l.MaxOutputBytes)
	if freeSpace == nil {
		return need, true
	}
	free, err := freeSpace(dir)
	return need, err != nil || free >= need
}

// fetchSource downloads the lease's source into dst, restarting from zero on a failure a
// later attempt could cure, at most MaxSourceAttempts times. It returns the sha-256 of the
// bytes written, or the typed reason the lease is failed with. A 410 sets gone. When ctx
// ends the reason it returns is not the one that matters: the caller reads gone and its own
// context first.
func (w *Worker) fetchSource(ctx context.Context, l *node.AcquireResponse, dst string, gone *atomic.Bool) ([]byte, string) {
	log := w.o.Log.With("lease", l.LeaseID, "epoch", l.Epoch)
	if need, ok := roomFor(w.o.FreeSpace, w.o.WorkDir, l); !ok {
		log.Warn("worker: worker_work_dir has no room for the leased source and its output, so nothing is downloaded",
			"work_dir", w.o.WorkDir, "bytes_needed", need)
		return nil, ReasonWorkDirFull
	}
	b := &backoff{min: w.o.MinBackoff, max: w.o.MaxBackoff, jitter: w.o.Jitter}
	for attempt := 1; attempt <= MaxSourceAttempts; attempt++ {
		sum, err := w.download(ctx, l, dst)
		if err == nil {
			return sum, ""
		}
		_ = os.Remove(dst)
		if errors.Is(err, errSourceGone) {
			gone.Store(true)
			return nil, ReasonSourceDownloadFailed
		}
		if ctx.Err() != nil {
			return nil, ReasonSourceDownloadFailed
		}
		var refusal *sourceRefusal
		again, wait := true, time.Duration(0)
		if errors.As(err, &refusal) {
			again, wait = refusal.again, refusal.wait
		}
		log.Warn("worker: the source download failed; nothing of it is encoded", "attempt", attempt, "again", again, "err", err)
		if !again || attempt == MaxSourceAttempts || w.o.Sleep(ctx, b.next(wait)) != nil {
			break
		}
	}
	return nil, ReasonSourceDownloadFailed
}

// download is one attempt: one unranged GET of the lease's source, written to dst and
// hashed on the way.
func (w *Worker) download(ctx context.Context, l *node.AcquireResponse, dst string) ([]byte, error) {
	// The attempt's own context: the idle timer cancels it when no byte has arrived for
	// SourceIdle, so a server that stops sending cannot hold a slot for ever.
	dctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var stalled atomic.Bool
	idle := time.AfterFunc(w.o.SourceIdle, func() { stalled.Store(true); cancel() })
	defer idle.Stop()
	stall := func(err error) error {
		if stalled.Load() {
			return &sourceRefusal{why: fmt.Sprintf("no byte arrived for %s", w.o.SourceIdle), again: true}
		}
		return err
	}

	req, err := http.NewRequestWithContext(dctx, http.MethodGet, w.base+leaseRoute(node.RouteSource, l.LeaseID), nil)
	if err != nil {
		return nil, &sourceRefusal{why: err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+w.o.Token.Expose())
	req.Header.Set(node.EpochHeader, strconv.FormatInt(l.Epoch, 10))
	// The bytes as they are, never a transparently decoded encoding of them.
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := w.o.HTTP.Do(req)
	if err != nil {
		return nil, stall(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxRefusal))
		var e node.ErrorResponse
		_ = json.Unmarshal(body, &e)
		switch resp.StatusCode {
		case http.StatusGone:
			return nil, errSourceGone
		case http.StatusServiceUnavailable:
			return nil, &sourceRefusal{why: "the server answered 503 " + e.Error, again: true, wait: retryAfter(resp)}
		}
		// Whatever the body holds, an answer that is not 200 is not the source.
		return nil, &sourceRefusal{why: fmt.Sprintf("the server answered %d %s with a %d-byte body, which is "+
			"never media", resp.StatusCode, e.Error, len(body))}
	}
	if resp.Header.Get("Content-Encoding") != "" || resp.Uncompressed {
		return nil, &sourceRefusal{why: "the 200 carries a content coding, so its body is not the source's own bytes"}
	}
	if resp.ContentLength >= 0 && resp.ContentLength != l.SourceSize {
		return nil, &sourceRefusal{why: fmt.Sprintf("the 200 declares %d bytes and the leased source is %d",
			resp.ContentLength, l.SourceSize)}
	}

	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, &sourceRefusal{why: "creating the download in worker_work_dir: " + err.Error()}
	}
	hash := sha256.New()
	buf := make([]byte, sourceChunk)
	// One byte past the lease's size is read, so a body that runs long is seen as one.
	body := io.LimitReader(resp.Body, l.SourceSize+1)
	var got int64
	for {
		n, rerr := body.Read(buf)
		if n > 0 {
			idle.Reset(w.o.SourceIdle)
			hash.Write(buf[:n])
			if _, werr := f.Write(buf[:n]); werr != nil {
				_ = f.Close()
				return nil, &sourceRefusal{why: "writing the download in worker_work_dir: " + werr.Error()}
			}
			got += int64(n)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			_ = f.Close()
			return nil, stall(&sourceRefusal{why: fmt.Sprintf("the body broke off after %d of %d bytes: %v",
				got, l.SourceSize, rerr), again: true})
		}
	}
	if err := f.Close(); err != nil {
		return nil, &sourceRefusal{why: "closing the download in worker_work_dir: " + err.Error()}
	}
	switch {
	case got < l.SourceSize:
		return nil, &sourceRefusal{why: fmt.Sprintf("the body ended after %d of the %d bytes leased", got, l.SourceSize), again: true}
	case got > l.SourceSize:
		return nil, &sourceRefusal{why: fmt.Sprintf("the body runs past the %d bytes leased", l.SourceSize)}
	}
	sum := hash.Sum(nil)
	// The trailer is read only after the body's end, which is when a client has it. It is
	// never required; one that is present, readable and different stops the lease here,
	// before anything is encoded.
	if sent, err := node.CanonicalDigest(resp.Trailer.Get(node.ReprDigestTrailer)); err == nil && sent != node.FormatDigest(sum) {
		return nil, &sourceRefusal{why: "the bytes received are not the " + node.ReprDigestTrailer +
			" the server sent after them: the source changed in transit", again: true}
	}
	return sum, nil
}

// TrustingClient is the HTTP client of a worker that trusts the certificates in caFile
// (worker_tls_ca) IN ADDITION to the system roots. It is how a worker reaches a server whose
// certificate is private. Verification is never switched off: a server whose chain leads to
// neither set fails the TLS handshake, before any request - and so before the node
// credential - is sent. An unreadable bundle, and one that holds no certificate, refuse.
//
// The minimum version is TLS 1.2, which is also crypto/tls's own default: "By default, TLS
// 1.2 is currently used as the minimum" (`go doc crypto/tls Config.MinVersion`, Go 1.25.14;
// https://pkg.go.dev/crypto/tls#Config, read 2026-10-03). It is written out so the floor
// does not move with a toolchain.
func TrustingClient(caFile string) (*http.Client, error) {
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("worker_tls_ca: the bundle could not be read: %w", err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("worker_tls_ca: %s holds no PEM certificate", caFile)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	return &http.Client{Transport: transport}, nil
}
