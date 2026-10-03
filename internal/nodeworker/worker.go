// Package nodeworker is the `holdfast worker` loop: the node side of the lease protocol
// (docs/design/nodes.md#worker).
//
// A worker only ever ENCODES. It asks the server for a lease, reads the source - through its
// own mount of the library (worker_path_map) in mapped mode, or downloaded from the server
// on the lease in http mode (source.go) - runs the command line the lease carried, and
// uploads the output with its length and sha-256. It never writes into the library, never
// decides anything about the source, and nothing it reports licenses a swap: the server
// re-runs every gate on what arrives.
//
// What it refuses is as much of the contract as what it does. An acquire answer that is not
// a 200 carrying the JSON of a lease is NEVER taken for a lease, whatever its body holds; a
// source no path-map entry covers is never guessed at; a source whose size or modification
// time is not the lease's is never encoded; an encoder its own probe did not find is never
// tried. Each is a typed `fail` of the lease, or no lease at all.
package nodeworker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/node"
	"github.com/NSchatz/holdfast/internal/secret"
)

// The worker's own defaults. Every one is ASSUMED: nobody has measured a node deployment.
const (
	// DefaultMinBackoff and DefaultMaxBackoff bound the exponential backoff a refusal or a
	// failed call is retried under (P4 rule 8: 5 s to 5 min).
	DefaultMinBackoff = 5 * time.Second
	DefaultMaxBackoff = 5 * time.Minute
	// DefaultAcquireTimeout bounds one acquire request, which the server holds for its
	// long-poll (30 s) before it answers.
	DefaultAcquireTimeout = 2 * time.Minute
	// DefaultCallTimeout bounds every other JSON call.
	DefaultCallTimeout = 30 * time.Second
	// MaxUploadAttempts bounds how many times one output is sent on one lease. The server's
	// own bound on digest mismatches (3) ends the lease first; this is the worker's own stop.
	MaxUploadAttempts = 5
	// maxAnswer bounds how much of a JSON answer is read.
	maxAnswer = 1 << 20
)

// The typed reasons a worker fails a lease with (the server's vocabulary: a-z, 0-9, `_`).
const (
	ReasonUnmappedSource     = "unmapped_source"
	ReasonSourceMismatch     = "source_mismatch"
	ReasonSourceUnreadable   = "source_unreadable"
	ReasonUnsupportedEncoder = "unsupported_encoder"
	ReasonEncodeFailed       = "encode_failed"
	ReasonOutputTooLarge     = "output_too_large"
	ReasonUploadRefused      = "upload_refused"
	ReasonRefusedPlan        = "refused_plan"
	ReasonWorkerStopping     = "worker_stopping"
	// ReasonSourceDownloadFailed is an http-mode lease whose source the worker could not
	// download as media: the server did not answer 200 with exactly the leased length, or
	// every attempt the bound allows failed. ReasonWorkDirFull is one whose source and
	// output the work directory has no room for.
	ReasonSourceDownloadFailed = "source_download_failed"
	ReasonWorkDirFull          = "work_dir_full"
)

// ErrInsecureServer is a worker_server the worker will not send its credential to.
var ErrInsecureServer = errors.New("worker_server is plain http to a host that is not loopback")

// ErrVersionMismatch is a server that runs another holdfast version. It is fatal: no retry
// makes two builds the same.
var ErrVersionMismatch = errors.New("the worker and the server run different holdfast versions")

// InsecureWarning is the record a worker writes at warn level at EVERY start when
// worker_insecure_http lets it speak plain http to a server that is not loopback
// (docs/design/nodes.md#transport).
const InsecureWarning = "worker_insecure_http is true and worker_server is plain http to a host that is not " +
	"loopback: THE NODE CREDENTIAL CROSSES THE NETWORK IN CLEARTEXT ON EVERY REQUEST, and in http mode so " +
	"does the library's media. Whoever captures the credential can lease jobs, upload outputs and, in http " +
	"mode, read the media; digests do not help against that. Use an https:// worker_server (server_tls_cert " +
	"and server_tls_key on the server, or a reverse proxy in front of it)"

// CheckTransport is CheckServer with the operator's override: a plain http server that is
// not loopback is accepted when insecureHTTP (worker_insecure_http) is true, and cleartext
// then reports that the override is what let it through, so the caller says so loudly.
// With https or a loopback host the override changes nothing and cleartext is false.
func CheckTransport(raw string, insecureHTTP bool) (cleartext bool, err error) {
	err = CheckServer(raw)
	if errors.Is(err, ErrInsecureServer) && insecureHTTP {
		return true, nil
	}
	return false, err
}

// CheckServer refuses a server address the node credential must not be sent to: anything but
// https, or plain http to a loopback host. Over plain http the token crosses the network in
// cleartext on every request, and whoever captures it can lease jobs and upload outputs.
func CheckServer(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return errors.New("worker_server is not an http or https URL")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		// Named without echoing it: what sits there may be a credential.
		return errors.New("worker_server must not carry userinfo, a query or a fragment: the node credential is " +
			"node_token and nothing else is sent")
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		host := u.Hostname()
		if ip := net.ParseIP(host); host == "localhost" || (ip != nil && ip.IsLoopback()) {
			return nil
		}
		return fmt.Errorf("%w (%s): the node credential would cross the network in cleartext on every "+
			"request. Point worker_server at an https:// address (server_tls_cert and server_tls_key on the "+
			"server, or a reverse proxy in front of it), or set worker_insecure_http: true to accept that",
			ErrInsecureServer, u.Host)
	}
	return errors.New("worker_server is not an http or https URL")
}

// EncodeFunc runs one leased encode: ffmpeg over in, writing out, with the lease's options.
// progress receives the encoder's position in the source, in seconds.
type EncodeFunc func(ctx context.Context, in, out string, pre, body []string, progress func(positionSec float64)) error

// Options configures a worker.
type Options struct {
	// Server is the server's base address (worker_server), and Token the node credential.
	Server string
	Token  secret.Value
	// Name is the name the worker gives (worker_name) and Version its build version.
	Name, Version string
	// Slots is how many encodes run at once (worker_slots).
	Slots int
	// Mode is how the worker reaches a leased source (worker_mode): node.ModeMapped, which
	// "" also means, or node.ModeHTTP.
	Mode string
	// InsecureHTTP is worker_insecure_http: it lets Server be plain http to a host that is
	// not loopback, which New then says at warn level.
	InsecureHTTP bool
	// PathMap translates a source path as the server names it into this worker's mount. It
	// is read in mapped mode only, and refused in http mode.
	PathMap config.PathMap
	// WorkDir is where an encode's output - and in http mode the downloaded source - is
	// written. It is created if it is missing, and every file this worker wrote there is
	// removed whatever became of its lease.
	WorkDir string
	// FreeSpace reports the free bytes on the filesystem holding a directory
	// (internal/diskfree.Bytes in production). nil skips the http-mode room check.
	FreeSpace func(dir string) (uint64, error)
	// SourceIdle is how long a source download may go without a byte arriving before it
	// is cut; 0 means DefaultSourceIdle.
	SourceIdle time.Duration
	// Encoders is the encoder registry keys this worker's own probe found.
	Encoders []string
	// Encode runs one encode.
	Encode EncodeFunc
	// Duration reports a source's length in seconds, for the progress a heartbeat carries;
	// nil, or false, reports no progress.
	Duration func(ctx context.Context, path string) (float64, bool)

	// HTTP is the client every call goes through; nil is one with no overall timeout (an
	// upload is as long as its output), each call being bounded by its own context.
	HTTP *http.Client
	Log  *slog.Logger
	// MinBackoff and MaxBackoff default to the constants above. Sleep waits d or until ctx
	// ends, and Jitter returns a number in [0, 1); both are seams for a test's clock.
	MinBackoff, MaxBackoff time.Duration
	Sleep                  func(ctx context.Context, d time.Duration) error
	Jitter                 func() float64
}

// Worker is one running `holdfast worker`.
type Worker struct {
	o    Options
	base string
}

// New checks the options and builds a Worker. It refuses a server the credential must not be
// sent to, a missing credential, and a worker that could encode nothing.
func New(o Options) (*Worker, error) {
	if strings.TrimSpace(o.Server) == "" {
		return nil, errors.New("worker_server is not set: a worker needs the address of the server it leases from")
	}
	cleartext, err := CheckTransport(o.Server, o.InsecureHTTP)
	if err != nil {
		return nil, err
	}
	switch o.Mode {
	case "":
		o.Mode = node.ModeMapped
	case node.ModeMapped:
	case node.ModeHTTP:
		if len(o.PathMap) > 0 {
			return nil, errors.New("worker_mode is http and worker_path_map is set: a worker in http mode " +
				"downloads each source from its server and maps no path")
		}
	default:
		return nil, fmt.Errorf("worker_mode %q is not one of mapped|http", o.Mode)
	}
	if o.Token.Empty() {
		return nil, errors.New("node_token is not set: a worker needs the node credential, by reference " +
			"(file:<path> or cmd:<argv>)")
	}
	if !config.ValidNodeName(o.Name) {
		return nil, errors.New("worker_name must be 1 to 64 characters from letters, digits, '.', '_' and '-'")
	}
	if len(o.Encoders) == 0 {
		return nil, errors.New("this worker's ffmpeg runs no encoder holdfast knows, so it could encode nothing")
	}
	if o.Encode == nil || o.Version == "" || o.WorkDir == "" {
		return nil, errors.New("nodeworker: Encode, Version and WorkDir are required")
	}
	if o.Slots < 1 {
		o.Slots = 1
	}
	if o.HTTP == nil {
		o.HTTP = &http.Client{}
	}
	// No redirect is ever followed: Go's client would carry the Authorization header to a
	// same-host redirect target and re-send an upload's body to wherever a 307 points. A
	// 3xx is therefore the answer itself, and no answer the protocol has is a 3xx.
	client := *o.HTTP
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	o.HTTP = &client
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if cleartext {
		// Said here, where a worker is built, so it is said at every start and cannot be
		// configured away.
		o.Log.Warn(InsecureWarning, "worker_server", o.Server, "worker_mode", o.Mode)
	}
	if o.SourceIdle <= 0 {
		o.SourceIdle = DefaultSourceIdle
	}
	if o.MinBackoff <= 0 {
		o.MinBackoff = DefaultMinBackoff
	}
	if o.MaxBackoff < o.MinBackoff {
		o.MaxBackoff = max(DefaultMaxBackoff, o.MinBackoff)
	}
	if o.Sleep == nil {
		o.Sleep = sleep
	}
	if o.Jitter == nil {
		o.Jitter = func() float64 { return float64(time.Now().UnixNano()%1000) / 1000 }
	}
	return &Worker{o: o, base: strings.TrimRight(strings.TrimSpace(o.Server), "/") + "/api/node/v1"}, nil
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Run runs the worker's slots until ctx ends, which is a clean stop: every encode in flight
// is stopped, its lease is failed `worker_stopping` best-effort, and Run returns nil. The one
// error it returns is fatal: a server at another version.
func (w *Worker) Run(ctx context.Context) error {
	if err := os.MkdirAll(w.o.WorkDir, 0o700); err != nil {
		return fmt.Errorf("worker_work_dir %s: %w", w.o.WorkDir, err)
	}
	w.sweepWorkDir()
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	var wg sync.WaitGroup
	var once sync.Once
	var fatal error
	for i := 0; i < w.o.Slots; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := w.slot(ctx); err != nil {
				once.Do(func() { fatal = err })
				stop()
			}
		}()
	}
	wg.Wait()
	return fatal
}

// outputName is the name an encode's output is written under in the work directory, and
// sourceName the name an http-mode lease's downloaded source is: the same stem with `.src`
// and, where the leased path has a short plain one, the source's own extension, which is
// what a demuxer that reads the name sees. outputNamed recognises exactly those two shapes:
// 32 hex characters, a dot, the epoch, then `.out`, or `.src` and an optional extension of
// 1 to 8 lower-case letters and digits.
func outputName(leaseID string, epoch int64) string {
	return leaseID + "." + strconv.FormatInt(epoch, 10) + ".out"
}

func sourceName(leaseID string, epoch int64, leasedPath string) string {
	name := leaseID + "." + strconv.FormatInt(epoch, 10) + ".src"
	if ext := strings.ToLower(filepath.Ext(leasedPath)); sourceExt.MatchString(ext) {
		name += ext
	}
	return name
}

var (
	outputNamed = regexp.MustCompile(`^[0-9a-f]{32}\.[0-9]+\.(out|src(\.[a-z0-9]{1,8})?)$`)
	sourceExt   = regexp.MustCompile(`^\.[a-z0-9]{1,8}$`)
)

// sweepWorkDir removes the files a previous run of this worker left: a killed worker
// removes nothing on its way out. Only a regular file whose name is this worker's own
// naming - an output, or a downloaded source - is touched: the directory may be shared, and
// nothing else in it is this worker's.
func (w *Worker) sweepWorkDir() {
	ents, err := os.ReadDir(w.o.WorkDir)
	if err != nil {
		return
	}
	for _, ent := range ents {
		if !ent.Type().IsRegular() || !outputNamed.MatchString(ent.Name()) {
			continue
		}
		if err := os.Remove(filepath.Join(w.o.WorkDir, ent.Name())); err == nil {
			w.o.Log.Info("worker: removed an output a previous run left", "file", ent.Name())
		}
	}
}

// backoff is the exponential backoff of one slot: min, doubling to max, each wait jittered
// into the upper half of its step so retries from several workers spread out.
type backoff struct {
	min, max, cur time.Duration
	jitter        func() float64
}

func (b *backoff) reset() { b.cur = 0 }

// next is the wait before the next attempt: at least atLeast (a Retry-After the server
// stated), and otherwise the jittered step.
func (b *backoff) next(atLeast time.Duration) time.Duration {
	if b.cur < b.min {
		b.cur = b.min
	}
	step := b.cur
	b.cur = min(b.cur*2, b.max)
	d := step/2 + time.Duration(b.jitter()*float64(step/2))
	return max(d, atLeast)
}

// slot is one encode slot: acquire, run the lease, again.
func (w *Worker) slot(ctx context.Context) error {
	b := &backoff{min: w.o.MinBackoff, max: w.o.MaxBackoff, jitter: w.o.Jitter}
	for ctx.Err() == nil {
		lease, wait, err := w.acquire(ctx, b)
		if err != nil {
			return err
		}
		if lease != nil {
			if w.runLease(ctx, lease) {
				b.reset()
				continue
			}
			// This worker failed the lease itself. Whatever stopped it - a path map, a
			// mount, an encoder - is very likely still there, so it backs off before it
			// asks again instead of failing the next file a moment later.
			wait = b.next(0)
		}
		if w.o.Sleep(ctx, wait) != nil {
			return nil
		}
	}
	return nil
}

// acquire asks for one lease. It returns the lease, or how long to wait before asking again,
// or the one fatal error. A lease is returned ONLY for a 200 whose body is the JSON of a
// lease this worker can read in full; every other answer - another status with any body, a
// 200 that is not JSON, a JSON object missing what a lease must carry - is no lease.
func (w *Worker) acquire(ctx context.Context, b *backoff) (*node.AcquireResponse, time.Duration, error) {
	req := node.AcquireRequest{Node: w.o.Name, Version: w.o.Version, Slots: 1, Mode: w.o.Mode, Encoders: w.o.Encoders}
	resp, body, err := w.call(ctx, DefaultAcquireTimeout, http.MethodPost, node.RouteLeases, req)
	if err != nil {
		if ctx.Err() == nil {
			w.o.Log.Warn("worker: asking for work failed; backing off", "err", err)
		}
		return nil, b.next(0), nil
	}
	retry := retryAfter(resp)
	switch resp.StatusCode {
	case http.StatusOK:
		lease, err := readLease(resp, body)
		if err == nil && lease.Mode != w.o.Mode {
			err = fmt.Errorf("its mode is %q, and this worker asked for work in %s mode", lease.Mode, w.o.Mode)
		}
		if err != nil {
			w.o.Log.Warn("worker: the server's answer is not a lease, so nothing is encoded", "status", resp.StatusCode, "why", err)
			return nil, b.next(retry), nil
		}
		return lease, 0, nil
	case http.StatusNoContent:
		// No work after a full long-poll: ask again after the server's own Retry-After.
		b.reset()
		return nil, max(retry, w.o.MinBackoff/2), nil
	case http.StatusConflict:
		var e node.ErrorResponse
		_ = json.Unmarshal(body, &e)
		if e.Error == "version_mismatch" {
			return nil, 0, fmt.Errorf("%w: this worker is %s and the server is %s; run the same version on both",
				ErrVersionMismatch, w.o.Version, e.ServerVersion)
		}
	}
	var e node.ErrorResponse
	_ = json.Unmarshal(body, &e)
	w.o.Log.Info("worker: no lease; backing off", "status", resp.StatusCode, "reason", e.Error)
	return nil, b.next(retry), nil
}

// readLease reads a 200 acquire answer as a lease, strictly.
func readLease(resp *http.Response, body []byte) (*node.AcquireResponse, error) {
	if mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mt != "application/json" {
		return nil, fmt.Errorf("its Content-Type is %q, not application/json", resp.Header.Get("Content-Type"))
	}
	var l node.AcquireResponse
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&l); err != nil {
		return nil, fmt.Errorf("its body is not the JSON of a lease: %w", err)
	}
	if id, err := hex.DecodeString(l.LeaseID); err != nil || len(id) != 16 {
		return nil, errors.New("it carries no lease id")
	}
	switch {
	case l.Epoch < 1:
		return nil, errors.New("it carries no epoch")
	case l.Mode != node.ModeMapped && l.Mode != node.ModeHTTP:
		return nil, fmt.Errorf("its mode is %q, which is neither mapped nor http", l.Mode)
	case l.Path == "" || l.Encoder == "" || len(l.Body) == 0:
		return nil, errors.New("it carries no source path, no encoder or no command line")
	case l.SourceSize < 2 || l.MaxOutputBytes < 1 || l.MaxOutputBytes >= l.SourceSize:
		return nil, errors.New("its source size and output cap are not those of a lease")
	case l.HeartbeatSec < 1 || l.TTLSec < 1:
		return nil, errors.New("it carries no heartbeat interval")
	}
	return &l, nil
}

// retryAfter is a response's Retry-After in whole seconds, and 0 where it carries none.
func retryAfter(resp *http.Response) time.Duration {
	s, err := strconv.Atoi(resp.Header.Get("Retry-After"))
	if err != nil || s < 0 {
		return 0
	}
	return time.Duration(s) * time.Second
}

// call makes one JSON call and returns the response with its body read (bounded).
func (w *Worker) call(ctx context.Context, timeout time.Duration, method, route string, in any) (*http.Response, []byte, error) {
	ctx, stop := context.WithTimeout(ctx, timeout)
	defer stop()
	payload, err := json.Marshal(in)
	if err != nil {
		return nil, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, w.base+route, bytes.NewReader(payload))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return w.do(req)
}

func (w *Worker) do(req *http.Request) (*http.Response, []byte, error) {
	req.Header.Set("Authorization", "Bearer "+w.o.Token.Expose())
	resp, err := w.o.HTTP.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
	if err != nil {
		return nil, nil, err
	}
	return resp, body, nil
}

func leaseRoute(route, id string) string { return strings.Replace(route, "{id}", id, 1) }

// fail ends the lease with a typed reason, best-effort and on its own short context: it is
// also what a stopping worker says on its way out, when its own context is already done.
func (w *Worker) fail(l *node.AcquireResponse, reason string) {
	ctx, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	resp, _, err := w.call(ctx, 10*time.Second, http.MethodPost, leaseRoute(node.RouteFail, l.LeaseID),
		node.FailRequest{Epoch: l.Epoch, Reason: reason})
	status := 0
	if resp != nil {
		status = resp.StatusCode
	}
	w.o.Log.Warn("worker: lease failed", "lease", l.LeaseID, "epoch", l.Epoch, "reason", reason, "status", status, "err", err)
}

// supports reports whether this worker's probe found the encoder.
func (w *Worker) supports(enc string) bool {
	for _, e := range w.o.Encoders {
		if e == enc {
			return true
		}
	}
	return false
}

// MtimeTolerance is how far a source's modification time on the worker's mount may sit from
// the one the server leased on. FAT keeps two-second timestamps and SMB and NFS servers round
// in their own ways, so one file read through two mounts can show two times. This is only
// the EARLY refusal of an obviously different file: the proof that the node read the server's
// source is the sha-256 the server compares before its gates.
const MtimeTolerance = 2 * time.Second

// sameSource reports whether the file at path is the size the lease was granted on, exactly,
// and its modification time within MtimeTolerance.
func sameSource(path string, l *node.AcquireResponse) (bool, error) {
	st, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	drift := time.Duration(st.ModTime().UnixNano() - l.SourceMtimeNS)
	if drift < 0 {
		drift = -drift
	}
	return st.Mode().IsRegular() && st.Size() == l.SourceSize && drift <= MtimeTolerance, nil
}

// refusedOptions are the ffmpeg options a leased command line may not carry. The server's
// own plans never emit one between the input and the output: an input, an attachment read
// from or dumped to a file, the overwrite switch, a second progress channel and a filter
// graph read from a file are each a way for a lease to make this worker's ffmpeg read or
// write something other than the mapped source and its own output.
var refusedOptions = map[string]bool{
	"-i": true, "-attach": true, "-dump_attachment": true, "-y": true, "-progress": true,
	"-filter_script": true, "-filter_complex_script": true, "--": true,
}

// refusedPlan says why a lease's command line is one this worker will not run, and "" for
// one it will. A worker holds the node credential's word for what to execute, so it checks
// the shape the server's leasable plans have and refuses anything outside it, unrun: options
// before the input (the device arguments no leased plan carries), a refused option, with or
// without a stream specifier, and any argument that is an absolute path or climbs out of a
// directory.
func refusedPlan(l *node.AcquireResponse) string {
	if len(l.Pre) > 0 {
		return "it carries options before the input"
	}
	for _, a := range l.Body {
		name, _, _ := strings.Cut(a, ":")
		switch {
		case refusedOptions[a] || refusedOptions[name]:
			return "it carries the option " + name
		case strings.HasPrefix(a, "/"):
			return "it carries an absolute path"
		case a == ".." || strings.HasPrefix(a, "../") || strings.HasSuffix(a, "/..") || strings.Contains(a, "/../"):
			return "it carries a path that climbs out of its directory"
		}
	}
	return ""
}

// runLease carries one lease to its end: reach and check the source, encode, hash, upload,
// complete. The local files are removed on every way out this process lives through. It
// reports false when this worker failed the lease itself.
func (w *Worker) runLease(ctx context.Context, l *node.AcquireResponse) (ok bool) {
	log := w.o.Log.With("lease", l.LeaseID, "epoch", l.Epoch, "path", l.Path)
	streamed := w.o.Mode == node.ModeHTTP
	var src string
	if !streamed {
		var err error
		if src, err = node.MapSource(w.o.PathMap, l.Path); err != nil {
			log.Warn("worker: no worker_path_map entry covers the leased source; it is never guessed at", "err", err)
			w.fail(l, ReasonUnmappedSource)
			return false
		}
	}
	if why := refusedPlan(l); why != "" {
		log.Warn("worker: the leased command line is not one this worker runs", "why", why)
		w.fail(l, ReasonRefusedPlan)
		return false
	}
	if !w.supports(l.Encoder) {
		w.fail(l, ReasonUnsupportedEncoder)
		return false
	}
	if !streamed {
		same, err := sameSource(src, l)
		if err != nil {
			log.Warn("worker: the mapped source could not be read", "mapped", src, "err", err)
			w.fail(l, ReasonSourceUnreadable)
			return false
		}
		if !same {
			log.Warn("worker: the mapped source is not the size and modification time the lease was granted on", "mapped", src)
			w.fail(l, ReasonSourceMismatch)
			return false
		}
	}

	out := filepath.Join(w.o.WorkDir, outputName(l.LeaseID, l.Epoch))
	defer func() { _ = os.Remove(out) }()

	// The lease's own context: a 410 from any call cancels it, which stops the download and
	// the encode at once.
	lctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var gone atomic.Bool
	var progress atomic.Uint64
	lost := func() { gone.Store(true); cancel() }
	hbDone := make(chan struct{})
	go func() {
		defer close(hbDone)
		w.heartbeat(lctx, l, &progress, lost)
	}()
	defer func() { cancel(); <-hbDone }()

	// In http mode the source is downloaded first, heartbeating all the while, and hashed as
	// it is written: srcSum is then the sha-256 of exactly the bytes that arrived.
	var srcSum []byte
	if streamed {
		src = filepath.Join(w.o.WorkDir, sourceName(l.LeaseID, l.Epoch, l.Path))
		defer func() { _ = os.Remove(src) }()
		sum, reason := w.fetchSource(lctx, l, src, &gone)
		switch {
		case gone.Load():
			log.Warn("worker: the lease is gone; the source download was stopped and discarded")
			return true
		case ctx.Err() != nil:
			w.fail(l, ReasonWorkerStopping)
			return false
		case reason != "":
			w.fail(l, reason)
			return false
		}
		srcSum = sum
	}

	var dur float64
	if w.o.Duration != nil {
		dur, _ = w.o.Duration(lctx, src)
	}
	started := time.Now()
	err := w.o.Encode(lctx, src, out, l.Pre, l.Body, func(pos float64) {
		if dur > 0 {
			progress.Store(math.Float64bits(min(pos/dur, 1)))
		}
	})
	encodeSec := time.Since(started).Seconds()
	switch {
	case gone.Load():
		log.Warn("worker: the lease is gone; the encode was stopped and its output discarded")
		return true
	case ctx.Err() != nil:
		w.fail(l, ReasonWorkerStopping)
		return false
	case err != nil:
		log.Warn("worker: the encode failed", "err", err)
		w.fail(l, ReasonEncodeFailed)
		return false
	}
	progress.Store(math.Float64bits(1))

	if !streamed {
		srcSum, _, err = hashFile(src)
		if err == nil {
			// The source is hashed after the encode read it; one that moved meanwhile is not the
			// source this output was encoded from.
			if same, serr := sameSource(src, l); serr != nil || !same {
				err = errors.New("the source changed while it was encoded")
			}
		}
		if err != nil {
			log.Warn("worker: the source could not be hashed as leased", "err", err)
			w.fail(l, ReasonSourceMismatch)
			return false
		}
	}
	outSum, size, err := hashFile(out)
	if err != nil {
		log.Warn("worker: the output could not be read back", "err", err)
		w.fail(l, ReasonEncodeFailed)
		return false
	}
	if size < 1 || size > l.MaxOutputBytes {
		log.Warn("worker: the output is not strictly smaller than the source, so the server would refuse it",
			"output_bytes", size, "max_output_bytes", l.MaxOutputBytes)
		w.fail(l, ReasonOutputTooLarge)
		return false
	}
	digest := node.FormatDigest(outSum)
	if !w.upload(lctx, l, out, size, digest, &gone) {
		switch {
		case gone.Load():
			log.Warn("worker: the lease is gone; the output was discarded")
		case ctx.Err() != nil:
			w.fail(l, ReasonWorkerStopping)
		default:
			w.fail(l, ReasonUploadRefused)
			return false
		}
		return true
	}
	w.complete(lctx, l, node.CompleteRequest{Epoch: l.Epoch, OutputDigest: digest,
		SourceDigest: node.FormatDigest(srcSum), OutputBytes: size, EncodeSec: encodeSec}, &gone)
	log.Info("worker: lease done", "output_bytes", size, "encode_sec", encodeSec)
	return true
}

// hashFile is the sha-256 and length of a file, read sequentially.
func hashFile(path string) ([]byte, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return nil, 0, err
	}
	return h.Sum(nil), n, nil
}

// heartbeat renews the lease every heartbeat_sec until ctx ends. A 410 means the lease is
// gone and calls lost; any other failure is logged and tried again at the next beat, because
// whether the lease still lives is the server's clock's call, not this worker's.
func (w *Worker) heartbeat(ctx context.Context, l *node.AcquireResponse, progress *atomic.Uint64, lost func()) {
	every := time.Duration(l.HeartbeatSec) * time.Second
	for w.o.Sleep(ctx, every) == nil {
		resp, _, err := w.call(ctx, DefaultCallTimeout, http.MethodPost, leaseRoute(node.RouteHeartbeat, l.LeaseID),
			node.HeartbeatRequest{Epoch: l.Epoch, Progress: math.Float64frombits(progress.Load())})
		switch {
		case err != nil:
			if ctx.Err() == nil {
				w.o.Log.Warn("worker: heartbeat failed; trying again at the next beat", "lease", l.LeaseID, "err", err)
			}
		case resp.StatusCode == http.StatusGone:
			lost()
			return
		case resp.StatusCode != http.StatusOK:
			w.o.Log.Warn("worker: heartbeat refused; trying again at the next beat", "lease", l.LeaseID, "status", resp.StatusCode)
		}
	}
}

// upload sends the output with its declared length, its Content-Digest and the lease epoch,
// and reports whether the server admitted it. A digest mismatch and a 503 are retried while
// the lease lives, under the worker's own bound; a 410 marks the lease gone; anything else
// the server refuses outright is not sent again.
func (w *Worker) upload(ctx context.Context, l *node.AcquireResponse, out string, size int64, digest string, gone *atomic.Bool) bool {
	b := &backoff{min: w.o.MinBackoff, max: w.o.MaxBackoff, jitter: w.o.Jitter}
	for attempt := 1; attempt <= MaxUploadAttempts; attempt++ {
		resp, body, err := w.put(ctx, l, out, size, digest)
		wait := time.Duration(0)
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return false
			}
			w.o.Log.Warn("worker: upload failed; trying again", "lease", l.LeaseID, "attempt", attempt, "err", err)
		case resp.StatusCode == http.StatusOK:
			return true
		case resp.StatusCode == http.StatusGone:
			gone.Store(true)
			return false
		default:
			var e node.ErrorResponse
			_ = json.Unmarshal(body, &e)
			retriable := resp.StatusCode == http.StatusServiceUnavailable || e.Error == "digest_mismatch" ||
				e.Error == "upload_in_progress" || e.Error == "short_body" || e.Error == "upload_stalled"
			w.o.Log.Warn("worker: upload refused", "lease", l.LeaseID, "attempt", attempt, "status", resp.StatusCode, "reason", e.Error)
			if !retriable {
				return false
			}
			wait = retryAfter(resp)
		}
		if attempt == MaxUploadAttempts || w.o.Sleep(ctx, b.next(wait)) != nil {
			return false
		}
	}
	return false
}

// put is one upload request.
func (w *Worker) put(ctx context.Context, l *node.AcquireResponse, out string, size int64, digest string) (*http.Response, []byte, error) {
	f, err := os.Open(out)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = f.Close() }()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, w.base+leaseRoute(node.RouteOutput, l.LeaseID), f)
	if err != nil {
		return nil, nil, err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-Digest", digest)
	req.Header.Set(node.EpochHeader, strconv.FormatInt(l.Epoch, 10))
	return w.do(req)
}

// complete reports the admitted output's figures and the digest of the source bytes read. A
// 503 or a failed call is retried while the lease lives; the server's answer to anything else
// stands.
func (w *Worker) complete(ctx context.Context, l *node.AcquireResponse, req node.CompleteRequest, gone *atomic.Bool) {
	b := &backoff{min: w.o.MinBackoff, max: w.o.MaxBackoff, jitter: w.o.Jitter}
	for {
		resp, body, err := w.call(ctx, DefaultCallTimeout, http.MethodPost, leaseRoute(node.RouteComplete, l.LeaseID), req)
		wait := time.Duration(0)
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return
			}
			w.o.Log.Warn("worker: reporting completion failed; trying again", "lease", l.LeaseID, "err", err)
		case resp.StatusCode == http.StatusOK:
			return
		case resp.StatusCode == http.StatusServiceUnavailable:
			wait = retryAfter(resp)
		default:
			if resp.StatusCode == http.StatusGone {
				gone.Store(true)
			}
			var e node.ErrorResponse
			_ = json.Unmarshal(body, &e)
			w.o.Log.Warn("worker: the server refused the completion", "lease", l.LeaseID, "status", resp.StatusCode, "reason", e.Error)
			return
		}
		if w.o.Sleep(ctx, b.next(wait)) != nil {
			return
		}
	}
}
