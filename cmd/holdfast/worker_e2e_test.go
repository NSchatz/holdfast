package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/engine"
	"github.com/NSchatz/holdfast/internal/node"
	"github.com/NSchatz/holdfast/internal/store"
	"github.com/NSchatz/holdfast/internal/version"
)

// The worker-node end-to-end fixtures (docs/design/nodes.md). Each runs the REAL `holdfast
// serve` - this test binary re-executed as the CLI, so the wiring is main.go's own - over a
// real SQLite ledger and a real two-second lavfi clip in a temp library, on a loopback
// listener. The node is the real `holdfast worker` (another re-execution) where the test is
// about the worker, and a fake that speaks the protocol by hand where a real worker cannot
// misbehave or must outlive a server.
//
// HOW A NODE DETERMINISTICALLY GETS A FILE. A server always has one local worker, and it
// takes the first file the scan offers. Each library here therefore starts with a blocker:
// the server's ffmpeg is a wrapper that holds any LOCAL library encode while a `hold` file
// exists, so the local worker is busy inside the blocker's encode, the scan's next offer
// waits in the feed, and the node that then asks for work is the only taker. The same wrapper
// logs every command line the server runs, which is how "the server ran no encode for that
// file" and "the server ran the gates on that working file" are read off what it really ran.

// labWait bounds every wait for something that must happen. It is generous on purpose: the
// gate runs these under -race on a loaded host, and a wait that passes returns at once.
const labWait = 10 * time.Minute

type nodeLab struct {
	t                *testing.T
	dir, lib, mount  string
	state            string
	addr, base       string
	token            string
	cfgPath          string
	hold, blocked    string
	serverFF, nodeFF string // the wrappers
	serverFFLog      string
	nodeFFLog        string
	ffmpeg, ffprobe  string
	blocker, movie   string
	extraCfg         string
	starts           int
}

func newNodeLab(t *testing.T, extraCfg string) *nodeLab {
	t.Helper()
	ffmpeg := envOr("HOLDFAST_FFMPEG", "ffmpeg")
	ffprobe := envOr("HOLDFAST_FFPROBE", "ffprobe")
	realFF, err := exec.LookPath(ffmpeg)
	if err != nil {
		t.Fatalf("::error:: ffmpeg required for the worker-node proof: %v", err)
	}
	if _, err := exec.LookPath(ffprobe); err != nil {
		t.Fatalf("::error:: ffprobe required for the worker-node proof: %v", err)
	}
	dir := t.TempDir()
	l := &nodeLab{t: t, dir: dir, lib: filepath.Join(dir, "server", "media"), mount: filepath.Join(dir, "node-mount"),
		state: filepath.Join(dir, "state"), token: "node-token-of-this-test", extraCfg: extraCfg,
		cfgPath: filepath.Join(dir, "config.yaml"), hold: filepath.Join(dir, "hold"), blocked: filepath.Join(dir, "blocked"),
		serverFF: filepath.Join(dir, "server-ffmpeg"), nodeFF: filepath.Join(dir, "node-ffmpeg"),
		serverFFLog: filepath.Join(dir, "server-ffmpeg.log"), nodeFFLog: filepath.Join(dir, "node-ffmpeg.log"),
		ffmpeg: realFF, ffprobe: ffprobe}
	if err := os.MkdirAll(l.lib, 0o755); err != nil {
		t.Fatal(err)
	}
	// THE SHARED MOUNT: the same directory, reached by the node under another path.
	if err := os.Symlink(l.lib, l.mount); err != nil {
		t.Fatal(err)
	}
	l.blocker = h264Fixture(t, realFF, filepath.Join(l.lib, "a-blocker.mkv"))
	l.movie = h264Fixture(t, realFF, filepath.Join(l.lib, "movie.mkv"))
	tokenFile := filepath.Join(dir, "node-token")
	writeFile(t, tokenFile, l.token+"\n", 0o400)
	writeFile(t, l.hold, "", 0o600)
	// The server's ffmpeg: logs its command line, and holds a local encode of a library file
	// while the hold file exists.
	writeFile(t, l.serverFF, fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %q
for a in "$@"; do
	case "$a" in *.__transcoding__.*) [ -f "$a" ] && printf 'inode %%s %%s\n' "$(stat -c %%i "$a")" "$a" >> %q ;; esac
done
case "$*" in
*%q*"-c:v libx265"*)
	if [ -e %q ]; then
		: > %q
		while [ -e %q ]; do sleep 0.05; done
	fi
	;;
esac
exec %q "$@"
`, l.serverFFLog, l.serverFFLog, l.lib, l.hold, l.blocked, l.hold, realFF), 0o755)
	writeFile(t, l.nodeFF, fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" >> %q\nexec %q \"$@\"\n", l.nodeFFLog, realFF), 0o755)
	l.pickPort()
	return l
}

// pickPort gives the server a free loopback port and writes its configuration. The port is
// free when it is picked and can be taken before the server binds it, so serve picks again
// when the bind collides.
func (l *nodeLab) pickPort() {
	l.t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		l.t.Fatal(err)
	}
	l.addr = ln.Addr().String()
	l.base = "http://" + l.addr
	_ = ln.Close()
	writeFile(l.t, l.cfgPath, "library_roots:\n  - "+l.lib+"\nstate_dir: "+l.state+"\nserver_addr: "+l.addr+
		"\nnode_token: file:"+filepath.Join(l.dir, "node-token")+"\nmin_bitrate_kbps: 0\npreset: ultrafast\nscan_interval_sec: 0\n"+l.extraCfg, 0o600)
}

func writeFile(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

// proc is one re-execution of this binary as the holdfast CLI, in its own process group.
type proc struct {
	t       *testing.T
	cmd     *exec.Cmd
	logPath string
	done    chan struct{}
	code    int
}

func (l *nodeLab) start(name, ffmpeg string, args ...string) *proc {
	l.t.Helper()
	logPath := filepath.Join(l.dir, name+".log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		l.t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), subprocessEnv+"=1", "HOLDFAST_FFMPEG="+ffmpeg, "HOLDFAST_FFPROBE="+l.ffprobe)
	cmd.Stdout, cmd.Stderr = f, f
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		l.t.Fatal(err)
	}
	p := &proc{t: l.t, cmd: cmd, logPath: logPath, done: make(chan struct{})}
	go func() {
		err := cmd.Wait()
		_ = f.Close()
		p.code = cmd.ProcessState.ExitCode()
		_ = err
		close(p.done)
	}()
	l.t.Cleanup(p.kill)
	return p
}

// kill is a crash: SIGKILL to the whole process group, ffmpeg children included.
func (p *proc) kill() {
	_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
	<-p.done
}

// stop is a graceful stop: SIGTERM, and the exit code.
func (p *proc) stop() int {
	p.t.Helper()
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(labWait):
		p.t.Errorf("the process did not stop on SIGTERM\n%s", p.log())
		p.kill()
	}
	return p.code
}

func (p *proc) log() string {
	b, _ := os.ReadFile(p.logPath)
	return string(b)
}

// waitLog waits for a log record carrying every one of subs, and returns it.
func (p *proc) waitLog(what string, subs ...string) string {
	p.t.Helper()
	deadline := time.Now().Add(labWait)
	for {
		for _, line := range strings.Split(p.log(), "\n") {
			ok := true
			for _, s := range subs {
				ok = ok && strings.Contains(line, s)
			}
			if ok {
				return line
			}
		}
		select {
		case <-p.done:
			p.t.Fatalf("the process exited (%d) before %s\n%s", p.code, what, p.log())
		default:
		}
		if time.Now().After(deadline) {
			p.t.Fatalf("timed out waiting for %s\n%s", what, p.log())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// serve starts the server and waits until its local worker is held inside the blocker's
// encode: from then on the next file the scan offers waits for a node. A start that loses its
// port to another process between the pick and the bind is started again on a new one.
func (l *nodeLab) serve() *proc {
	l.t.Helper()
	_ = os.Remove(l.blocked)
	for attempt := 1; ; attempt++ {
		l.starts++
		p := l.start("serve-"+strconv.Itoa(l.starts), l.serverFF, "serve", "--config", l.cfgPath)
		deadline := time.Now().Add(labWait)
		for {
			log := p.log()
			if strings.Contains(log, "serve listening") && strings.Contains(log, "nodes_enabled=true") {
				if _, err := os.Stat(l.blocked); err == nil {
					return p
				}
			}
			exited := false
			select {
			case <-p.done:
				exited = true
			default:
			}
			if exited && strings.Contains(p.log(), "address already in use") && attempt < 5 {
				l.pickPort()
				break
			}
			if exited {
				l.t.Fatalf("the server exited (%d) before it was serving\n%s", p.code, p.log())
			}
			if time.Now().After(deadline) {
				l.t.Fatalf("the server never reached the blocker's encode\n%s", p.log())
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}

// release lets the held local encode run.
func (l *nodeLab) release() { _ = os.Remove(l.hold) }

// worker starts the real `holdfast worker`, which reaches the library through the mount.
func (l *nodeLab) worker() (*proc, string) {
	l.t.Helper()
	work := filepath.Join(l.dir, "node-work")
	cfg := filepath.Join(l.dir, "worker.yaml")
	writeFile(l.t, cfg, "library_roots:\n  - "+l.mount+"\nstate_dir: "+filepath.Join(l.dir, "node-state")+
		"\nworker_server: "+l.base+"\nnode_token: file:"+filepath.Join(l.dir, "node-token")+
		"\nworker_name: nodeA\nworker_work_dir: "+work+
		"\nworker_path_map:\n  - from: "+l.lib+"\n    to: "+l.mount+"\npreset: ultrafast\n", 0o600)
	return l.start("worker", l.nodeFF, "worker", "--config", cfg), work
}

// reply is one fake-node call's answer.
type nodeReply struct {
	status int
	body   []byte
	reason string
}

func (l *nodeLab) call(method, route string, header map[string]string, body []byte) nodeReply {
	l.t.Helper()
	req, err := http.NewRequest(method, l.base+"/api/node/v1"+route, bytes.NewReader(body))
	if err != nil {
		l.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+l.token)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		l.t.Fatalf("%s %s: %v", method, route, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	r := nodeReply{status: resp.StatusCode, body: b}
	var e node.ErrorResponse
	if json.Unmarshal(b, &e) == nil {
		r.reason = e.Error
	}
	return r
}

func (l *nodeLab) postJSON(route string, in any) nodeReply {
	b, _ := json.Marshal(in)
	return l.call(http.MethodPost, route, map[string]string{"Content-Type": "application/json"}, b)
}

// acquire is a fake node asking for one lease, and getting it.
func (l *nodeLab) acquire() node.AcquireResponse {
	l.t.Helper()
	r := l.postJSON(node.RouteLeases, node.AcquireRequest{Node: "fakeNode", Version: version.Version, Slots: 1,
		Mode: node.ModeMapped, Encoders: []string{"cpu"}})
	if r.status != http.StatusOK {
		l.t.Fatalf("acquire: %d %s", r.status, r.body)
	}
	var lease node.AcquireResponse
	if err := json.Unmarshal(r.body, &lease); err != nil {
		l.t.Fatal(err)
	}
	return lease
}

func leaseAt(route string, lease node.AcquireResponse) string {
	return strings.Replace(route, "{id}", lease.LeaseID, 1)
}

func sha256Digest(b []byte) string {
	sum := sha256.Sum256(b)
	return node.FormatDigest(sum[:])
}

// deliver uploads out as the lease's output and completes the lease, honestly: the digests
// are the real ones of the bytes sent and of the source.
func (l *nodeLab) deliver(lease node.AcquireResponse, out []byte) {
	l.t.Helper()
	if r := l.call(http.MethodPut, leaseAt(node.RouteOutput, lease), map[string]string{
		"Content-Digest": sha256Digest(out), node.EpochHeader: strconv.FormatInt(lease.Epoch, 10)}, out); r.status != http.StatusOK {
		l.t.Fatalf("upload: %d %s", r.status, r.body)
	}
	if r := l.postJSON(leaseAt(node.RouteComplete, lease), node.CompleteRequest{Epoch: lease.Epoch,
		OutputDigest: sha256Digest(out), SourceDigest: sha256Digest(readAll(l.t, lease.Path)), OutputBytes: int64(len(out))}); r.status != http.StatusOK {
		l.t.Fatalf("complete: %d %s", r.status, r.body)
	}
}

// encodeWith runs the real ffmpeg over src with args, and returns what it wrote.
func (l *nodeLab) encodeWith(src string, args ...string) []byte {
	l.t.Helper()
	out := filepath.Join(l.t.TempDir(), "out.mkv")
	argv := append(append([]string{"-hide_banner", "-loglevel", "error", "-y", "-i", src}, args...), "--", out)
	if b, err := exec.Command(l.ffmpeg, argv...).CombinedOutput(); err != nil {
		l.t.Fatalf("ffmpeg %v: %v\n%s", argv, err, b)
	}
	return readAll(l.t, out)
}

// row is the newest ledger row at path, read once the servers are stopped.
func (l *nodeLab) row(path string) store.Job {
	l.t.Helper()
	st := openStore(l.t, l.state)
	jobs, err := st.List(context.Background(), nil, 100)
	if err != nil {
		l.t.Fatal(err)
	}
	for _, j := range jobs {
		if j.Path == path {
			return j
		}
	}
	l.t.Fatalf("no ledger row for %s", path)
	return store.Job{}
}

// waitRow waits, through a read-only door beside the running server's own, until the row at
// path satisfies ok. A log record precedes the write it announces, so a test that stops the
// server on the record alone can cut the write off.
func (l *nodeLab) waitRow(path, what string, ok func(store.Job) bool) {
	l.t.Helper()
	st, err := store.OpenReadOnly(filepath.Join(l.state, "jobs.db"))
	if err != nil {
		l.t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	deadline := time.Now().Add(labWait)
	for {
		jobs, err := st.List(context.Background(), nil, 100)
		if err != nil {
			l.t.Fatal(err)
		}
		for _, j := range jobs {
			if j.Path == path && ok(j) {
				return
			}
		}
		if time.Now().After(deadline) {
			l.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (l *nodeLab) noTemps() {
	l.t.Helper()
	ents, err := os.ReadDir(l.lib)
	if err != nil {
		l.t.Fatal(err)
	}
	for _, e := range ents {
		if engine.IsTempConstructionName(e.Name()) {
			l.t.Errorf("a working file was left beside the sources: %s", e.Name())
		}
	}
}

// linesWith are the logged command lines carrying every one of subs.
func linesWith(t *testing.T, logPath string, subs ...string) []string {
	t.Helper()
	b, _ := os.ReadFile(logPath)
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		ok := line != ""
		for _, s := range subs {
			ok = ok && strings.Contains(line, s)
		}
		if ok {
			out = append(out, line)
		}
	}
	return out
}

var leaseIDInLog = regexp.MustCompile(`lease=([0-9a-f]{32})`)

// TestWorkerEndToEnd_SharedMountPathMapServerRegatesAndRenames is the foundation: the real
// `holdfast serve` and the real `holdfast worker`, sharing one library the worker reaches
// under another path. The worker encodes; the server names the working file, takes the
// upload into it, runs EVERY gate itself and performs the rename.
func TestWorkerEndToEnd_SharedMountPathMapServerRegatesAndRenames(t *testing.T) {
	l := newNodeLab(t, "vmaf_enable: true\n")
	before := readAll(t, l.movie)
	srv := l.serve()
	wrk, work := l.worker()

	granted := srv.waitLog("the lease", "node lease granted", "node=nodeA", "path="+l.movie)
	srv.waitLog("the swap of the node's output", "DONE", "file="+l.movie)
	wrk.waitLog("the worker's own record", "worker: lease done")

	// THE NODE ENCODED, AND THE SERVER DID NOT: the only library encode the server's ffmpeg
	// was ever asked for is the blocker's, still held.
	mapped := filepath.Join(l.mount, "movie.mkv")
	if got := linesWith(t, l.serverFFLog, "-i "+l.movie, "-c:v libx265"); len(got) != 0 {
		t.Errorf("the server ran an encode of the leased file:\n%s", strings.Join(got, "\n"))
	}
	if got := linesWith(t, l.serverFFLog, "-i "+l.lib, "-c:v libx265"); len(got) != 1 || !strings.Contains(got[0], "-i "+l.blocker) {
		t.Errorf("the server's own library encodes = %q, want the blocker's alone", got)
	}
	nodeEncodes := linesWith(t, l.nodeFFLog, "-i "+mapped, "-c:v libx265")
	if len(nodeEncodes) != 1 {
		t.Fatalf("the node ran %d encode(s) of %s through its path map, want 1:\n%s", len(nodeEncodes), mapped, readAll(t, l.nodeFFLog))
	}
	if !strings.Contains(nodeEncodes[0], "-- "+work+string(filepath.Separator)) {
		t.Errorf("the node wrote its output outside its work directory: %s", nodeEncodes[0])
	}
	if strings.Contains(nodeEncodes[0], l.lib) {
		t.Errorf("the node's command line names the server's path: %s", nodeEncodes[0])
	}

	// THE SERVER NAMED THE WORKING FILE, in the source's own directory, by the engine's own
	// temp construction - and its own gates then read exactly that file.
	srv.waitLog("the admitted upload", "node upload admitted", "node=nodeA")
	nodeEncode := srv.waitLog("the record naming the node", "node encode", "node=nodeA", "file="+l.movie, "epoch=1")
	transcode := srv.waitLog("the job's own record", "msg=transcode", "file="+l.movie)
	m := regexp.MustCompile(`working_file=(\S+)`).FindStringSubmatch(transcode)
	if m == nil {
		t.Fatalf("no working file on the job's record: %s", transcode)
	}
	temp := m[1]
	if filepath.Dir(temp) != l.lib || !engine.IsTempConstructionName(filepath.Base(temp)) {
		t.Errorf("the working file %s is not the engine's temp construction in the source's directory", temp)
	}
	// EVERY GATE RAN ON THE SERVER, against the working file IT named and ITS OWN path to the
	// source - read off the command lines the server's ffmpeg was really given.
	if got := linesWith(t, l.serverFFLog, "-xerror", "-err_detect", "-i "+temp+" ", "-f null"); len(got) == 0 {
		t.Errorf("the server's decode-integrity pass never ran on the working file %s", temp)
	}
	vmafRuns := linesWith(t, l.serverFFLog, "libvmaf", "-i "+temp+" -i "+l.movie+" ")
	if len(vmafRuns) == 0 {
		t.Errorf("the server's VMAF gate never compared the working file %s with its own source %s", temp, l.movie)
	}
	if got := linesWith(t, l.serverFFLog, l.mount); len(got) != 0 {
		t.Errorf("a server command line names the NODE's path to the library:\n%s", strings.Join(got, "\n"))
	}
	// THE NODE NEVER WROTE INTO THE LIBRARY: every command it ran reads the mapped source and
	// writes into its own work directory, and none names the working file.
	for _, line := range linesWith(t, l.nodeFFLog, l.mount) {
		if !strings.Contains(line, "-- "+work+string(filepath.Separator)) || strings.Contains(line, "__transcoding__") {
			t.Errorf("a worker command line could write under the library: %s", line)
		}
	}
	// THE RENAME WAS THE SERVER'S, and a rename it was: the file now at the source's path is
	// the very inode the server's gates read as the working file.
	inodes := linesWith(t, l.serverFFLog, "inode ", temp)
	if len(inodes) == 0 {
		t.Fatalf("no gate command was handed the working file %s", temp)
	}
	var fin syscall.Stat_t
	if err := syscall.Stat(l.movie, &fin); err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("inode %d %s", fin.Ino, temp); inodes[len(inodes)-1] != want {
		t.Errorf("the file at the source's path is inode %d; the gates read %q: the swap was not a rename of the gated file",
			fin.Ino, inodes[len(inodes)-1])
	}
	for _, want := range []string{`output_digest="sha-256=:`, `source_digest="` + sha256Digest(before) + `"`} {
		if !strings.Contains(nodeEncode, want) {
			t.Errorf("the record naming the node does not carry %s: %s", want, nodeEncode)
		}
	}

	// The rest of the pass: the held local encode runs, and both processes stop cleanly.
	l.release()
	srv.waitLog("the local job", "DONE", "file="+l.blocker)
	if code := wrk.stop(); code != 0 {
		t.Errorf("the worker exited %d on SIGTERM, want 0\n%s", code, wrk.log())
	}
	if code := srv.stop(); code != 0 {
		t.Errorf("the server exited %d on SIGTERM, want 0", code)
	}
	if strings.Contains(srv.log(), l.token) || strings.Contains(wrk.log(), l.token) {
		t.Error("the node credential reached a log")
	}

	// THE LEASE ROW: completed, on the working file the engine named.
	st := openStore(t, l.state)
	id := leaseIDInLog.FindStringSubmatch(granted)
	if id == nil {
		t.Fatalf("no lease id on %s", granted)
	}
	lease, found, err := st.GetLease(context.Background(), id[1])
	if err != nil || !found {
		t.Fatalf("GetLease: %v, found %v", err, found)
	}
	if lease.State != store.LeaseCompleted || lease.Temp != temp || lease.Node != "nodeA" || lease.Path != l.movie || lease.Epoch != 1 {
		t.Errorf("lease = %+v, want completed on %s by nodeA at epoch 1", lease, temp)
	}

	// EVERY GATE RAN ON THE SERVER AND THE RENAME WAS ITS OWN: the terminal row's proof.
	j := l.row(l.movie)
	o := j.Outcome
	if j.Status != store.Done {
		t.Fatalf("status = %s (%s), want done", j.Status, o.Reason)
	}
	if o.Encoder != "cpu" {
		t.Errorf("row: encoder %q, want cpu", o.Encoder)
	}
	if o.VmafMean == nil || o.VmafMin == nil || o.VmafModel == "" || *o.VmafMean < 80 {
		t.Errorf("row carries no VMAF proof: mean %v min %v model %q", o.VmafMean, o.VmafMin, o.VmafModel)
	}
	if o.SourceBytes == nil || o.OutputBytes == nil || *o.SourceBytes != int64(len(before)) || *o.OutputBytes >= *o.SourceBytes {
		t.Errorf("row sizes: source %v output %v, want the source's %d and a smaller output", o.SourceBytes, o.OutputBytes, len(before))
	}
	if o.OutputBytes != nil && *o.OutputBytes != lease.OutputBytes {
		t.Errorf("the row records %d output bytes and the lease %d: the swapped file is not the uploaded one", *o.OutputBytes, lease.OutputBytes)
	}
	after := readAll(t, l.movie)
	if codec := videoCodec(t, l.movie); codec != "hevc" {
		t.Errorf("the file is %s after the swap, want hevc", codec)
	}
	if int64(len(after)) != lease.OutputBytes || sha256Digest(after) != lease.OutputDigest {
		t.Error("the file at the source's path is not, byte for byte, the output the lease admitted")
	}
	l.noTemps()
	if ents, _ := os.ReadDir(work); len(ents) != 0 {
		t.Errorf("the worker's work directory holds %d file(s) after the lease", len(ents))
	}
}

// TestWorkerEndToEnd_AWorkerVerdictNeverLicensesASwap is the twin: a node that completes its
// lease honestly about the BYTES - right length, right digest, right source digest - and
// wrongly about the media. A valid file in the wrong codec, and a valid HEVC file of ruinous
// quality, are both admitted as candidates and both refused by the server's own gates; each
// source is byte for byte what it was.
func TestWorkerEndToEnd_AWorkerVerdictNeverLicensesASwap(t *testing.T) {
	l := newNodeLab(t, "vmaf_enable: true\n")
	other := h264Fixture(t, l.ffmpeg, filepath.Join(l.lib, "other.mkv"))
	before := map[string][]byte{l.movie: readAll(t, l.movie), other: readAll(t, other)}
	srv := l.serve()

	bad := map[string][]string{
		"a valid file in the wrong codec": {"-map", "0", "-c:v", "libx264", "-preset", "ultrafast", "-crf", "40", "-pix_fmt", "yuv420p", "-f", "matroska"},
		"a valid HEVC file of ruinous quality": {"-map", "0", "-c:v", "libx265", "-preset", "ultrafast", "-x265-params", "log-level=error",
			"-crf", "51", "-vf", "scale=32:24,scale=320:240", "-pix_fmt", "yuv420p10le", "-f", "matroska"},
	}
	reasons := map[string]string{}
	for _, name := range []string{"a valid file in the wrong codec", "a valid HEVC file of ruinous quality"} {
		lease := l.acquire()
		out := l.encodeWith(lease.Path, bad[name]...)
		if int64(len(out)) > lease.MaxOutputBytes {
			t.Fatalf("%s: the bad output is %d bytes, past the cap %d; the fixture proves nothing", name, len(out), lease.MaxOutputBytes)
		}
		l.deliver(lease, out)
		srv.waitLog("the server's verdict on "+name, "FAIL (verify rejected, source untouched)", "file="+lease.Path)
		l.waitRow(lease.Path, "the refusal's row", func(j store.Job) bool { return j.Status == store.Failed })
		reasons[lease.Path] = name
	}
	if len(reasons) != 2 {
		t.Fatalf("the two bad outputs went to %d file(s), want 2", len(reasons))
	}
	for path := range reasons {
		if got := linesWith(t, l.serverFFLog, "-i "+path+" ", "-c:v libx265"); len(got) != 0 {
			t.Errorf("the server encoded a leased file itself:\n%s", strings.Join(got, "\n"))
		}
	}
	srv.stop()

	for path, name := range reasons {
		j := l.row(path)
		if j.Status != store.Failed {
			t.Errorf("%s: status = %s, want failed", name, j.Status)
		}
		if !bytes.Equal(readAll(t, path), before[path]) {
			t.Errorf("%s: the source changed", name)
		}
		if codec := videoCodec(t, path); codec != "h264" {
			t.Errorf("%s: the source's path now holds %s", name, codec)
		}
		t.Logf("%s was refused: %s", name, j.Outcome.Reason)
		if strings.Contains(name, "ruinous") && j.Outcome.VmafMean == nil {
			t.Errorf("%s: the row carries no VMAF figure; it was refused by an earlier gate (%s)", name, j.Outcome.Reason)
		}
		if strings.Contains(name, "wrong codec") && !strings.Contains(j.Outcome.Reason, "codec") {
			t.Errorf("%s: the reason does not name the codec: %s", name, j.Outcome.Reason)
		}
	}
	l.noTemps()
}

// TestWorkerFixture_AServerRestartWithLiveLeasesAdoptsOrAbandonsBeforeAnyGrant is P4 rule 9.
// A lease is granted, the server is KILLED mid-lease, and a new server starts on the same
// ledger and library.
func TestWorkerFixture_AServerRestartWithLiveLeasesAdoptsOrAbandonsBeforeAnyGrant(t *testing.T) {
	// crash grants a lease, plants what a killed process leaves beside the sources, and
	// kills the server.
	crash := func(t *testing.T, l *nodeLab) (lease node.AcquireResponse, temp, strayTemp string) {
		t.Helper()
		first := l.serve()
		lease = l.acquire()
		if lease.Path != l.movie || lease.Epoch != 1 {
			t.Fatalf("leased %s at epoch %d", lease.Path, lease.Epoch)
		}
		// The node heartbeats until the server is killed, so however long this host takes
		// to get from the grant to the kill, the lease is live when the server dies.
		beating, stopBeating := make(chan struct{}), make(chan struct{})
		go func() {
			defer close(beating)
			for {
				select {
				case <-stopBeating:
					return
				case <-time.After(250 * time.Millisecond):
					b, _ := json.Marshal(node.HeartbeatRequest{Epoch: lease.Epoch})
					req, _ := http.NewRequest(http.MethodPost, l.base+"/api/node/v1"+leaseAt(node.RouteHeartbeat, lease), bytes.NewReader(b))
					req.Header.Set("Authorization", "Bearer "+l.token)
					req.Header.Set("Content-Type", "application/json")
					if resp, err := http.DefaultClient.Do(req); err == nil {
						_ = resp.Body.Close()
					}
				}
			}
		}()
		transcode := first.waitLog("the job's own record", "msg=transcode", "file="+l.movie)
		temp = regexp.MustCompile(`working_file=(\S+)`).FindStringSubmatch(transcode)[1]
		first.kill()
		close(stopBeating)
		<-beating
		// What the dead process left: a partial upload in the lease's working file, and - for
		// comparison - the working file of the local encode it was killed in.
		writeFile(t, temp, "half an upload", 0o644)
		strayTemp = strings.Replace(temp, "movie", "a-blocker", 1)
		writeFile(t, strayTemp, "half a local encode", 0o644)
		return lease, temp, strayTemp
	}

	t.Run("a heartbeat inside the grace continues the lease", func(t *testing.T) {
		// A TTL no start-up on a loaded host outlasts: this case is about the grace being
		// honoured, not about it running out.
		l := newNodeLab(t, "vmaf_enable: false\nnode_lease_ttl_sec: 3600\n")
		before := readAll(t, l.movie)
		lease, temp, strayTemp := crash(t, l)

		second := l.serve()
		// THE HOLDS ARE RE-TAKEN BEFORE ANY GRANT: the job that takes the lease back has
		// claimed its row, re-taken its free-space hold and its working file, and reached the
		// seam before the listener exists - and no grant is possible without the listener.
		log := second.log()
		adopt := strings.Index(log, "worker=adopt0")
		listening := strings.Index(log, "serve listening")
		if adopt < 0 || listening < 0 || adopt > listening {
			t.Fatalf("the recovered lease was not taken back before the listener accepted (adopt at %d, listening at %d)\n%s", adopt, listening, log)
		}
		second.waitLog("the adoption", "node lease adopted after a restart", "lease="+lease.LeaseID)
		// The dead process's leavings are treated as a killed local encode's are: gone.
		// Each is cleared when the job that owns the path comes back for it: the killed local
		// encode's by the local worker, the lease's by the job that took the lease back.
		if b, err := os.ReadFile(strayTemp); err == nil && string(b) == "half a local encode" {
			t.Errorf("the killed local encode's working file survived the restart: %s", strayTemp)
		}
		if b, err := os.ReadFile(temp); err == nil && string(b) == "half an upload" {
			t.Errorf("the partial upload the dead process left is still at %s", temp)
		}

		if r := l.postJSON(leaseAt(node.RouteHeartbeat, lease), node.HeartbeatRequest{Epoch: lease.Epoch}); r.status != http.StatusOK {
			t.Fatalf("a heartbeat inside the grace answered %d %s, want 200", r.status, r.body)
		}
		// The node finishes the encode it was leased before the restart, and the upload lands.
		out := l.encodeWith(lease.Path, append(append([]string(nil), lease.Pre...), lease.Body...)...)
		l.deliver(lease, out)
		second.waitLog("the swap", "DONE", "file="+l.movie)
		l.waitRow(l.movie, "the done row", func(j store.Job) bool { return j.Status == store.Done })
		if got := linesWith(t, l.serverFFLog, "-i "+l.movie, "-c:v libx265"); len(got) != 0 {
			t.Errorf("the server encoded the leased file itself:\n%s", strings.Join(got, "\n"))
		}
		second.stop()
		if j := l.row(l.movie); j.Status != store.Done {
			t.Errorf("status = %s (%s), want done", j.Status, j.Outcome.Reason)
		}
		after := readAll(t, l.movie)
		if !bytes.Equal(after, out) || bytes.Equal(after, before) {
			t.Error("the file at the source's path is not the output uploaded after the restart")
		}
	})

	t.Run("heartbeats stop: the lease runs out and the file is offered again at a higher epoch", func(t *testing.T) {
		l := newNodeLab(t, "vmaf_enable: false\nnode_lease_ttl_sec: 8\n")
		before := readAll(t, l.movie)
		lease, _, strayTemp := crash(t, l)

		second := l.serve()
		// The lease was TAKEN BACK first - this is "adopted, then ran out", not a lease that
		// ran out before the engine came for it - and then, with no heartbeat, after the one
		// TTL of grace Ready gave, it is gone and says so.
		adopted := second.waitLog("the adoption", "node lease adopted after a restart", "lease="+lease.LeaseID)
		expired := second.waitLog("the lease running out", "FAIL (node encode failed, source untouched)", "file="+l.movie, "expired")
		if log := second.log(); strings.Index(log, adopted) > strings.Index(log, expired) {
			t.Fatal("the lease ran out before it was taken back")
		}
		if strings.Contains(second.log(), "this job is encoded by the server") {
			t.Fatalf("the lease was not adopted: the server encoded the job\n%s", second.log())
		}
		if r := l.postJSON(leaseAt(node.RouteHeartbeat, lease), node.HeartbeatRequest{Epoch: lease.Epoch}); r.status != http.StatusGone {
			t.Fatalf("a heartbeat after the grace answered %d %s, want 410", r.status, r.body)
		}
		out := []byte("the stale node's output, sent anyway")
		if r := l.call(http.MethodPut, leaseAt(node.RouteOutput, lease), map[string]string{
			"Content-Digest": sha256Digest(out), node.EpochHeader: "1"}, out); r.status != http.StatusGone {
			t.Errorf("the stale node's upload answered %d, want 410", r.status)
		}
		if b, err := os.ReadFile(strayTemp); err == nil && string(b) == "half a local encode" {
			t.Errorf("the killed local encode's working file survived the restart: %s", strayTemp)
		}
		// The file is offered again: the next node to ask gets it, one epoch on.
		again := l.acquire()
		if again.Path != l.movie || again.Epoch != lease.Epoch+1 {
			t.Fatalf("the retry leased %s at epoch %d, want %s at epoch %d", again.Path, again.Epoch, l.movie, lease.Epoch+1)
		}
		l.postJSON(leaseAt(node.RouteFail, again), node.FailRequest{Epoch: again.Epoch, Reason: "encode_failed"})
		second.waitLog("the second attempt ending", "FAIL (node encode failed, source untouched)", "encode_failed")
		l.waitRow(l.movie, "the second failure's row", func(j store.Job) bool { return j.Status == store.Failed && j.FailCount == 2 })
		second.stop()
		if !bytes.Equal(readAll(t, l.movie), before) {
			t.Error("the source changed")
		}
		if j := l.row(l.movie); j.Status != store.Failed || j.FailCount != 2 {
			t.Errorf("status = %s, fail_count = %d, want failed and 2 (one per ended lease)", j.Status, j.FailCount)
		}
	})
}

// TestWorker_TheCommandRefusesToStartWithoutWhatItNeeds: `holdfast worker` names the key an
// operator has to set, and refuses a plain http:// server that is not loopback before it
// resolves its credential.
func TestWorker_TheCommandRefusesToStartWithoutWhatItNeeds(t *testing.T) {
	dir := t.TempDir()
	lib := filepath.Join(dir, "media")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(dir, "token")
	writeFile(t, token, "tok\n", 0o400)
	base := "library_roots:\n  - " + lib + "\nstate_dir: " + filepath.Join(dir, "state") + "\n"
	for name, tc := range map[string]struct{ cfg, want string }{
		"no worker_server":         {"node_token: file:" + token + "\n", "worker_server is not set"},
		"no node_token":            {"worker_server: http://127.0.0.1:9\n", "node_token is not set"},
		"plain http, not loopback": {"worker_server: http://192.0.2.10:8080\nnode_token: file:/nonexistent/never-read\n", "cleartext"},
	} {
		t.Run(name, func(t *testing.T) {
			cfgPath := filepath.Join(t.TempDir(), "config.yaml")
			writeFile(t, cfgPath, base+tc.cfg, 0o600)
			var out, errOut bytes.Buffer
			code := dispatch([]string{"worker", "--config", cfgPath}, &out, &errOut)
			if code != 1 || !strings.Contains(errOut.String(), "refusing to start") || !strings.Contains(errOut.String(), tc.want) {
				t.Errorf("worker exited %d saying %q, want 1 and a refusal naming %q", code, errOut.String(), tc.want)
			}
		})
	}
	var out, errOut bytes.Buffer
	if code := dispatch(nil, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "\n  worker ") {
		t.Errorf("the usage does not list the worker command:\n%s", errOut.String())
	}
}
