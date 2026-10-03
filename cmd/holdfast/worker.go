package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/diskfree"
	"github.com/NSchatz/holdfast/internal/encoder"
	"github.com/NSchatz/holdfast/internal/engine"
	"github.com/NSchatz/holdfast/internal/hwdevice"
	"github.com/NSchatz/holdfast/internal/logging"
	"github.com/NSchatz/holdfast/internal/node"
	"github.com/NSchatz/holdfast/internal/nodeworker"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/server"
	"github.com/NSchatz/holdfast/internal/store"
	"github.com/NSchatz/holdfast/internal/version"
)

// workerWorkDirName is the directory under the OS temp directory a worker writes its
// outputs in when worker_work_dir is unset.
const workerWorkDirName = "holdfast-worker"

// cmdWorker is `holdfast worker`: the node side of the lease protocol
// (docs/design/nodes.md#worker). It leases encodes from the server worker_server names, reads
// each source through its own mount of the library, and uploads the output. It writes nothing
// into the library and holds one credential, node_token, which can lease and upload and
// nothing else.
func cmdWorker(args []string, _, stderr io.Writer) int {
	fs := flag.NewFlagSet("worker", flag.ContinueOnError)
	cfg, code := loadConfig(fs, args, stderr)
	if cfg == nil {
		return code
	}
	log := logging.New(cfg.LogLevel)
	// SIGINT/SIGTERM stops every encode in flight, fails its lease `worker_stopping`
	// best-effort, and exits 0.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runWorker(ctx, cfg, log, stderr,
		envOr("HOLDFAST_FFMPEG", "ffmpeg"), envOr("HOLDFAST_FFPROBE", "ffprobe"), nil)
}

// runWorker checks what a worker needs, probes its encoders, and runs its loop until ctx
// ends. Every refusal names the key an operator has to set. tune, nil in production, lets a
// test shorten the loop's waits.
func runWorker(ctx context.Context, cfg *config.Config, log *slog.Logger, stderr io.Writer,
	ffmpeg, ffprobe string, tune func(*nodeworker.Options)) int {
	refuse := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "holdfast: refusing to start: "+format+"\n", a...)
		return 1
	}
	srv := strings.TrimSpace(cfg.WorkerServer)
	if srv == "" {
		return refuse("worker_server is not set: a worker needs the address of the server it leases from")
	}
	// D8: the credential never crosses a network in cleartext. Checked before the secret is
	// even resolved.
	if err := nodeworker.CheckServer(srv); err != nil {
		return refuse("%v", err)
	}
	if !cfg.NodesEnabled() {
		return refuse("node_token is not set: a worker needs the node credential, by reference " +
			"(file:<path> or cmd:<argv>) - see docs/secrets.md")
	}
	name := cfg.WorkerName
	if name == "" {
		host, err := os.Hostname()
		if err != nil || !config.ValidNodeName(host) {
			return refuse("worker_name is not set and this host's name is not one a node may give " +
				"(1 to 64 characters from letters, digits, '.', '_' and '-'): set worker_name")
		}
		name = host
	}
	for _, bin := range []string{ffmpeg, ffprobe} {
		if _, err := exec.LookPath(bin); err != nil {
			fmt.Fprintf(stderr, "holdfast: required binary %q not found: %v\n", bin, err)
			return 1
		}
	}
	// THE SECRET, resolved once, through the one resolver every command uses. It goes to the
	// worker's HTTP calls and nowhere else: not into a log record, not into the environment
	// an ffmpeg child inherits.
	secrets, code := resolveSecrets(ctx, cfg, stderr)
	if code != 0 {
		return code
	}

	// What this worker can encode: every SOFTWARE encoder of the registry its own ffmpeg
	// runs, proved by the same real probe encode `run` and `serve` gate their own encoders
	// with. Hardware encoders are not offered to a node in this build (docs/design/nodes.md#leasable).
	var encoders []string
	for _, key := range encoder.Known() {
		spec, _ := encoder.Lookup(key)
		if spec.Hardware {
			continue
		}
		if _, err := probeEncoder(ctx, cfg, ffmpeg, ffprobe, key, hwdevice.Assignment{}); err != nil {
			log.Info("worker: encoder not offered", "encoder", key, "why", err.Error())
			continue
		}
		encoders = append(encoders, key)
	}

	workDir := cfg.WorkerWorkDir
	if workDir == "" {
		workDir = filepath.Join(os.TempDir(), workerWorkDirName)
	}
	memory := engine.DeriveMemoryWatch(envOr(engine.CgroupRootEnv, ""))
	memory.Announce(log)
	enc := engine.FFmpegEncoder{FFmpeg: ffmpeg, Memory: memory.Bound}
	prober := probe.New(ffmpeg, ffprobe)
	opts := nodeworker.Options{
		Server: srv, Token: secrets.Get(config.NodeTokenKey), Name: name, Version: version.Version,
		Slots: cfg.EffectiveWorkerSlots(), PathMap: cfg.WorkerPathMap, WorkDir: workDir,
		Encoders: encoders, Log: log,
		// The command line is assembled by the engine's own function, the one every local
		// encode goes through, so a node's argv and the server's cannot drift.
		Encode: func(ctx context.Context, in, out string, pre, body []string, progress func(float64)) error {
			return enc.RunLeased(ctx, in, out, pre, body, func(p engine.Progress) { progress(p.PositionSec) })
		},
		Duration: func(ctx context.Context, path string) (float64, bool) {
			return prober.VideoProps(ctx, path).DurationSec()
		},
	}
	if tune != nil {
		tune(&opts)
	}
	w, err := nodeworker.New(opts)
	if err != nil {
		return refuse("%v", err)
	}
	log.Info("worker starting", "server", srv, "name", name, "slots", opts.Slots,
		"encoders", strings.Join(encoders, ","), "work_dir", workDir,
		"path_map_entries", len(cfg.WorkerPathMap), "version", version.Version)
	if err := w.Run(ctx); err != nil {
		fmt.Fprintf(stderr, "holdfast: worker stopped: %v\n", err)
		return 1
	}
	log.Info("worker stopped")
	return 0
}

// startNodes wires the worker-node lease protocol into a `serve` (docs/design/nodes.md):
// it builds the hub over the job store's lease ledger, recovers the leases a previous
// process left live, takes each back through the engine - or abandons it - BEFORE the hub
// may grant anything, and only then hands the hub to the server and to the engine's encode
// seam. The returned wait joins the hub's sweep and the adopted jobs at shutdown; it is
// called after ctx has ended.
func startNodes(ctx context.Context, cfg *config.Config, eng *engine.Engine, st store.Store,
	srv *server.Server, log *slog.Logger) (wait func(), err error) {
	ledger, ok := st.(store.LeaseLedger)
	if !ok {
		return nil, fmt.Errorf("the job store keeps no lease ledger")
	}
	hub := node.New(node.Options{
		Ledger: ledger, BaseCtx: ctx, Version: version.Version, TTL: cfg.NodeLeaseTTL(),
		MaxLeases: cfg.EffectiveNodeMaxLeases(), MaxLeasesPerNode: cfg.EffectiveNodeMaxLeasesPerNode(),
		MaxTransfers: cfg.EffectiveNodeMaxTransfers(), FreeSpace: diskfree.Bytes, Log: log,
	})
	eng.Nodes = hub
	live, err := hub.Recover(ctx)
	if err != nil {
		return nil, fmt.Errorf("recovering the node leases: %w", err)
	}
	if len(live) > 0 {
		log.Info("nodes: taking back the leases that were live when the server stopped", "leases", len(live))
	}
	joinAdopted := eng.AdoptLeases(ctx, live)
	hub.Ready()
	srv.SetNodes(hub)
	done := make(chan struct{})
	go func() { defer close(done); hub.Run(ctx) }()
	return func() { <-done; joinAdopted() }, nil
}
