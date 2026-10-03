package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/engine"
	"github.com/NSchatz/holdfast/internal/node"
	"github.com/NSchatz/holdfast/internal/nodeworker"
	"github.com/NSchatz/holdfast/internal/secret"
	"github.com/NSchatz/holdfast/internal/store"
	"github.com/NSchatz/holdfast/internal/version"
)

// http mode and the transport, end to end (docs/design/nodes.md#http-mode, #transport), on
// the lab of worker_e2e_test.go: the real `holdfast serve` and the real `holdfast worker`,
// re-executed from this binary, over a real ledger and real two-second lavfi clips on a
// loopback listener.

// pemKeyBlock is the PEM block type of a private key, spelled in two halves so that no file
// of this repository carries the marker a secret scanner reads as a committed key. Every key
// these tests use is generated when the test runs and written under its temp directory.
const pemKeyBlock = "PRIVATE" + " KEY"

// unmount takes the node's path to the library away: an http worker has none.
func (l *nodeLab) unmount() {
	l.t.Helper()
	if err := os.Remove(l.mount); err != nil {
		l.t.Fatal(err)
	}
}

// workerHTTP starts the real `holdfast worker` in http mode against scheme://addr. Its
// configuration names no library root and no path map: it has no mount of the library.
func (l *nodeLab) workerHTTP(name, scheme, extra string) (*proc, string) {
	l.t.Helper()
	work := filepath.Join(l.dir, name+"-work")
	cfg := filepath.Join(l.dir, name+".yaml")
	writeFile(l.t, cfg, "state_dir: "+filepath.Join(l.dir, name+"-state")+
		"\nworker_mode: http\nworker_server: "+scheme+"://"+l.addr+"\nnode_token: file:"+filepath.Join(l.dir, "node-token")+
		"\nworker_name: "+name+"\nworker_work_dir: "+work+"\npreset: ultrafast\n"+extra, 0o600)
	return l.start(name, l.nodeFF, "worker", "--config", cfg), work
}

// acquireHTTP is a fake node asking for one lease in http mode, and getting it.
func (l *nodeLab) acquireHTTP() node.AcquireResponse {
	l.t.Helper()
	r := l.postJSON(node.RouteLeases, node.AcquireRequest{Node: "fakeNode", Version: version.Version, Slots: 1,
		Mode: node.ModeHTTP, Encoders: []string{"cpu"}})
	if r.status != http.StatusOK {
		l.t.Fatalf("acquire in http mode: %d %s", r.status, r.body)
	}
	var lease node.AcquireResponse
	if err := json.Unmarshal(r.body, &lease); err != nil {
		l.t.Fatal(err)
	}
	if lease.Mode != node.ModeHTTP {
		l.t.Fatalf("the lease was granted in mode %q, want http", lease.Mode)
	}
	return lease
}

// download is a fake node's source request.
func (l *nodeLab) download(lease node.AcquireResponse, header map[string]string) nodeReply {
	l.t.Helper()
	h := map[string]string{node.EpochHeader: strconv.FormatInt(lease.Epoch, 10)}
	for k, v := range header {
		h[k] = v
	}
	return l.call(http.MethodGet, leaseAt(node.RouteSource, lease), h, nil)
}

// encodeBytes runs the real ffmpeg, with the lease's own options, over source bytes the fake
// node downloaded - from a file of its own, never from the library.
func (l *nodeLab) encodeBytes(lease node.AcquireResponse, source []byte) []byte {
	l.t.Helper()
	in := filepath.Join(l.t.TempDir(), "downloaded.mkv")
	writeFile(l.t, in, string(source), 0o600)
	return l.encodeWith(in, append(append([]string(nil), lease.Pre...), lease.Body...)...)
}

// terminalRow waits for the row at path to be done or failed, and returns it.
func (l *nodeLab) terminalRow(path string) store.Job {
	l.t.Helper()
	terminal := func(j store.Job) bool { return j.Status == store.Failed || j.Status == store.Done }
	l.waitRow(path, "a terminal row for "+path, terminal)
	st, err := store.OpenReadOnly(filepath.Join(l.state, "jobs.db"))
	if err != nil {
		l.t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	jobs, err := st.List(context.Background(), nil, 100)
	if err != nil {
		l.t.Fatal(err)
	}
	for _, j := range jobs {
		if j.Path == path && terminal(j) {
			return j
		}
	}
	l.t.Fatalf("the terminal row for %s is gone", path)
	return store.Job{}
}

var sourceDigestInLog = regexp.MustCompile(`source_digest="?(sha-256=:[A-Za-z0-9+/=]+:)"?`)

// TestWorkerEndToEnd_HTTPModeNoMountSourceAndOutputDigestsCheckedBothWaysServerRegatesAndRenames
// is the goal's line C. The real `holdfast serve` and the real `holdfast worker` on
// loopback, the worker in http mode with NO mount of the library, no path map and no
// library root: the server streams the source on the lease and hashes what it streams, the
// worker downloads it, encodes the downloaded file and uploads the output, and the server
// names the working file, runs EVERY gate itself and performs the rename.
//
// Both digest comparisons are then PROVEN to bite, on the same server, by a node that
// speaks the protocol by hand: an output whose bytes are not its Content-Digest is refused
// and leaves no file, and a completion whose source digest is not the digest of what the
// server streamed fails the job before any gate, with the source untouched.
func TestWorkerEndToEnd_HTTPModeNoMountSourceAndOutputDigestsCheckedBothWaysServerRegatesAndRenames(t *testing.T) {
	l := newNodeLab(t, "vmaf_enable: true\n")
	l.unmount()
	// Two files wait for a node. The first the feed offers goes to a node that speaks the
	// protocol by hand and tampers; the second to the real worker.
	tampered := l.movie
	good := h264Fixture(t, l.ffmpeg, filepath.Join(l.lib, "other.mkv"))
	tamperedBefore := readAll(t, tampered)
	before := readAll(t, good)
	srv := l.serve()

	// ---- THE COMPARISONS BITE: a node that speaks the protocol by hand ----
	lease := l.acquireHTTP()
	if lease.Path != tampered {
		t.Fatalf("the fake node was leased %s, want %s", lease.Path, tampered)
	}
	got := l.download(lease, nil)
	if got.status != http.StatusOK || !bytes.Equal(got.body, tamperedBefore) {
		t.Fatalf("the source request answered %d with %d bytes, want 200 and the source's %d", got.status, len(got.body), len(tamperedBefore))
	}
	out := l.encodeBytes(lease, got.body)
	if int64(len(out)) > lease.MaxOutputBytes {
		t.Fatalf("the fake node's output is %d bytes, past the cap %d; the fixture proves nothing", len(out), lease.MaxOutputBytes)
	}
	epoch := strconv.FormatInt(lease.Epoch, 10)
	// THE OUTPUT DIRECTION: the bytes that arrive are not the bytes the Content-Digest
	// declares - one bit of the output changed in transit. Refused, and nothing is kept.
	damaged := append([]byte(nil), out...)
	damaged[len(damaged)/2] ^= 0x01
	if r := l.call(http.MethodPut, leaseAt(node.RouteOutput, lease), map[string]string{
		"Content-Digest": sha256Digest(out), node.EpochHeader: epoch}, damaged); r.status != http.StatusBadRequest || r.reason != "digest_mismatch" {
		t.Fatalf("an output changed in transit answered %d %s, want 400 digest_mismatch: the server did not hold the upload to its Content-Digest",
			r.status, r.body)
	}
	l.noTemps()
	// The same output, intact, is admitted.
	if r := l.call(http.MethodPut, leaseAt(node.RouteOutput, lease), map[string]string{
		"Content-Digest": sha256Digest(out), node.EpochHeader: epoch}, out); r.status != http.StatusOK {
		t.Fatalf("the intact upload: %d %s", r.status, r.body)
	}
	// THE SOURCE DIRECTION: the node reports the digest of bytes that are one bit off what
	// the server streamed. The completion is refused against the STREAMED digest, the job
	// fails before any gate, the working file is removed and the source is untouched.
	arrived := append([]byte(nil), got.body...)
	arrived[len(arrived)/2] ^= 0x01
	r := l.postJSON(leaseAt(node.RouteComplete, lease), node.CompleteRequest{Epoch: lease.Epoch,
		OutputDigest: sha256Digest(out), SourceDigest: sha256Digest(arrived), OutputBytes: int64(len(out))})
	if r.status != http.StatusConflict || r.reason != "source_digest_mismatch" {
		t.Fatalf("a completion whose source digest is not the streamed one answered %d %s, want 409 source_digest_mismatch: "+
			"the server did not compare the node's source digest with the digest of what it streamed", r.status, r.body)
	}
	failed := srv.waitLog("the job failing before its gates", "FAIL (node encode failed, source untouched)", "file="+tampered)
	if !strings.Contains(failed, "source_digest_mismatch") {
		t.Errorf("the failure does not name the source digest mismatch: %s", failed)
	}
	l.waitRow(tampered, "the failure's row", func(j store.Job) bool { return j.Status == store.Failed })
	if got := linesWith(t, l.serverFFLog, "__transcoding__", "movie"); len(got) != 0 {
		t.Errorf("a gate ran on the working file of a job that failed before its gates:\n%s", strings.Join(got, "\n"))
	}
	if got := linesWith(t, l.serverFFLog, "-i "+tampered, "-c:v libx265"); len(got) != 0 {
		t.Errorf("the server encoded the leased file itself:\n%s", strings.Join(got, "\n"))
	}
	if !bytes.Equal(readAll(t, tampered), tamperedBefore) {
		t.Error("the source of the refused job changed")
	}
	l.noTemps()

	// ---- THE REAL WORKER, in http mode, with no mount ----
	wrk, work := l.workerHTTP("nodeA", "http", "")
	granted := srv.waitLog("the lease", "node lease granted", "node=nodeA", "path="+good, "mode=http")
	streamed := srv.waitLog("the source stream", "node source streamed", "node=nodeA")
	srv.waitLog("the swap of the node's output", "DONE", "file="+good)
	wrk.waitLog("the worker's own record", "worker: lease done")
	id := leaseIDInLog.FindStringSubmatch(granted)
	if id == nil {
		t.Fatalf("no lease id on %s", granted)
	}

	// THE SOURCE DIRECTION, first half: the server hashed what it streamed, and it is the
	// sha-256 of the source.
	wantSource := sha256Digest(before)
	if m := sourceDigestInLog.FindStringSubmatch(streamed); m == nil || m[1] != wantSource {
		t.Errorf("the server recorded %v for what it streamed, want the source's sha-256 %s: %s", m, wantSource, streamed)
	}
	if !strings.Contains(streamed, "bytes="+strconv.Itoa(len(before))) {
		t.Errorf("the stream record does not carry the source's %d bytes: %s", len(before), streamed)
	}

	// THE NODE ENCODED A DOWNLOADED FILE, AND THE SERVER DID NOT ENCODE: the worker's ffmpeg
	// read the source from its own work directory, under this lease's name, and no command it
	// ran names the library at all.
	if got := linesWith(t, l.serverFFLog, "-i "+good, "-c:v libx265"); len(got) != 0 {
		t.Errorf("the server ran an encode of the leased file:\n%s", strings.Join(got, "\n"))
	}
	if got := linesWith(t, l.serverFFLog, "-i "+l.lib, "-c:v libx265"); len(got) != 1 || !strings.Contains(got[0], "-i "+l.blocker) {
		t.Errorf("the server's own library encodes = %q, want the blocker's alone", got)
	}
	downloaded := filepath.Join(work, id[1]+".1.src.mkv")
	nodeEncodes := linesWith(t, l.nodeFFLog, "-i "+downloaded, "-c:v libx265")
	if len(nodeEncodes) != 1 {
		t.Fatalf("the node ran %d encode(s) of the downloaded source %s, want 1:\n%s", len(nodeEncodes), downloaded, readAll(t, l.nodeFFLog))
	}
	if !strings.Contains(nodeEncodes[0], "-- "+work+string(filepath.Separator)) {
		t.Errorf("the node wrote its output outside its work directory: %s", nodeEncodes[0])
	}
	for _, reach := range []string{l.lib, l.mount} {
		if got := linesWith(t, l.nodeFFLog, reach); len(got) != 0 {
			t.Errorf("a worker command line names the library (%s); an http worker has no path to it:\n%s", reach, strings.Join(got, "\n"))
		}
	}
	if _, err := os.Lstat(l.mount); err == nil {
		t.Error("the node's mount exists; this proof needs the worker to have none")
	}

	// THE SERVER NAMED THE WORKING FILE AND RAN EVERY GATE ON IT, against its own source.
	nodeEncode := srv.waitLog("the record naming the node", "node encode", "node=nodeA", "file="+good, "epoch=1")
	transcode := srv.waitLog("the job's own record", "msg=transcode", "file="+good)
	m := regexp.MustCompile(`working_file=(\S+)`).FindStringSubmatch(transcode)
	if m == nil {
		t.Fatalf("no working file on the job's record: %s", transcode)
	}
	temp := m[1]
	if filepath.Dir(temp) != l.lib || !engine.IsTempConstructionName(filepath.Base(temp)) {
		t.Errorf("the working file %s is not the engine's temp construction in the source's directory", temp)
	}
	if got := linesWith(t, l.serverFFLog, "-xerror", "-err_detect", "-i "+temp+" ", "-f null"); len(got) == 0 {
		t.Errorf("the server's decode-integrity pass never ran on the working file %s", temp)
	}
	if got := linesWith(t, l.serverFFLog, "libvmaf", "-i "+temp+" -i "+good+" "); len(got) == 0 {
		t.Errorf("the server's VMAF gate never compared the working file %s with its own source %s", temp, good)
	}
	// THE RENAME WAS THE SERVER'S: the file at the source's path is the inode its gates read.
	inodes := linesWith(t, l.serverFFLog, "inode ", temp)
	if len(inodes) == 0 {
		t.Fatalf("no gate command was handed the working file %s", temp)
	}
	var fin syscall.Stat_t
	if err := syscall.Stat(good, &fin); err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("inode %d %s", fin.Ino, temp); inodes[len(inodes)-1] != want {
		t.Errorf("the file at the source's path is inode %d; the gates read %q: the swap was not a rename of the gated file",
			fin.Ino, inodes[len(inodes)-1])
	}
	// THE SOURCE DIRECTION, second half: the digest the node reported, the digest of what the
	// server streamed and the source's own sha-256 are one figure.
	for _, want := range []string{`source_digest="` + wantSource + `"`, `source_streamed_digest="` + wantSource + `"`, `output_digest="sha-256=:`} {
		if !strings.Contains(nodeEncode, want) {
			t.Errorf("the record naming the node does not carry %s: %s", want, nodeEncode)
		}
	}
	after := readAll(t, good)
	if codec := videoCodec(t, good); codec != "hevc" {
		t.Errorf("the file is %s after the swap, want hevc", codec)
	}

	// The rest of the pass, and a clean stop of both.
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
	if ents, _ := os.ReadDir(work); len(ents) != 0 {
		t.Errorf("the worker's work directory holds %d file(s) after the lease: the source and the output are both removed", len(ents))
	}

	// THE RECORDED FIGURES, BOTH WAYS: the lease row's source digest is the sha-256 of the
	// source as it was, and its output digest the sha-256 of the file now at the source's path.
	st := openStore(t, l.state)
	row, found, err := st.GetLease(context.Background(), id[1])
	if err != nil || !found {
		t.Fatalf("GetLease: %v, found %v", err, found)
	}
	if row.State != store.LeaseCompleted || row.Temp != temp || row.Node != "nodeA" || row.Path != good || row.Epoch != 1 {
		t.Errorf("lease = %+v, want completed on %s by nodeA at epoch 1", row, temp)
	}
	if row.SourceDigest != wantSource {
		t.Errorf("the lease records source digest %s, want the sha-256 of the source %s", row.SourceDigest, wantSource)
	}
	if row.OutputDigest != sha256Digest(after) || row.OutputBytes != int64(len(after)) {
		t.Errorf("the lease records output %s (%d bytes); the file at the source's path is %s (%d bytes)",
			row.OutputDigest, row.OutputBytes, sha256Digest(after), len(after))
	}
	bad, found, err := st.GetLease(context.Background(), lease.LeaseID)
	if err != nil || !found || bad.State != store.LeaseFailed || bad.Reason != "source_digest_mismatch" || bad.SourceDigest != sha256Digest(arrived) {
		t.Errorf("the refused lease = %+v (%v, found %v), want failed source_digest_mismatch recording what the node reported", bad, err, found)
	}
	_ = st.Close()
	j := l.row(good)
	if j.Status != store.Done || j.Outcome.Encoder != "cpu" {
		t.Fatalf("status = %s (%s) encoder %q, want done by cpu", j.Status, j.Outcome.Reason, j.Outcome.Encoder)
	}
	if o := j.Outcome; o.VmafMean == nil || o.VmafModel == "" || *o.VmafMean < 80 {
		t.Errorf("row carries no VMAF proof: mean %v model %q", o.VmafMean, o.VmafModel)
	}
	if o := j.Outcome; o.SourceBytes == nil || o.OutputBytes == nil || *o.SourceBytes != int64(len(before)) || *o.OutputBytes != int64(len(after)) {
		t.Errorf("row sizes: source %v output %v, want %d and %d", o.SourceBytes, o.OutputBytes, len(before), len(after))
	}
	if j := l.row(tampered); j.Status != store.Failed || j.FailCount != 1 {
		t.Errorf("the refused job: status %s, fail_count %d, want failed and 1", j.Status, j.FailCount)
	}
	l.noTemps()
}

// TestWorkerFixture_HTTPMode_ARangedDownloadIsStillProvenByTheServersOwnHash: a node that
// reads the source with Range requests leaves the server no digest of a whole stream, so
// the hub records its completion as reported - and the server's OWN hash of its own copy of
// the source, taken before the gates in every mode, is what refuses a source digest that is
// not the source's. Nothing is gated, no working file is left and the source is untouched.
func TestWorkerFixture_HTTPMode_ARangedDownloadIsStillProvenByTheServersOwnHash(t *testing.T) {
	l := newNodeLab(t, "vmaf_enable: false\n")
	l.unmount()
	before := readAll(t, l.movie)
	srv := l.serve()
	lease := l.acquireHTTP()
	if lease.Path != l.movie {
		t.Fatalf("leased %s, want %s", lease.Path, l.movie)
	}
	half := len(before) / 2
	first := l.download(lease, map[string]string{"Range": fmt.Sprintf("bytes=0-%d", half-1)})
	second := l.download(lease, map[string]string{"Range": fmt.Sprintf("bytes=%d-", half)})
	if first.status != http.StatusPartialContent || second.status != http.StatusPartialContent {
		t.Fatalf("the ranged requests answered %d and %d, want 206", first.status, second.status)
	}
	source := append(append([]byte(nil), first.body...), second.body...)
	if !bytes.Equal(source, before) {
		t.Fatal("the two ranges together are not the source")
	}
	out := l.encodeBytes(lease, source)
	epoch := strconv.FormatInt(lease.Epoch, 10)
	if r := l.call(http.MethodPut, leaseAt(node.RouteOutput, lease), map[string]string{
		"Content-Digest": sha256Digest(out), node.EpochHeader: epoch}, out); r.status != http.StatusOK {
		t.Fatalf("upload: %d %s", r.status, r.body)
	}
	// The node says it read something else. The hub streamed no whole source, so it has
	// nothing to hold this to and records it.
	claimed := sha256Digest([]byte("not the source"))
	if r := l.postJSON(leaseAt(node.RouteComplete, lease), node.CompleteRequest{Epoch: lease.Epoch,
		OutputDigest: sha256Digest(out), SourceDigest: claimed, OutputBytes: int64(len(out))}); r.status != http.StatusOK {
		t.Fatalf("complete after a ranged download: %d %s", r.status, r.body)
	}
	nodeEncode := srv.waitLog("the record naming the node", "node encode", "node=fakeNode", "file="+l.movie)
	if !strings.Contains(nodeEncode, `source_streamed_digest=""`) {
		t.Errorf("ranged requests left a streamed digest on the lease: %s", nodeEncode)
	}
	// The job's terminal row, whichever it is: a server that did not compare would gate the
	// output and swap it, and that must be a named failure here and not a wait that runs out.
	if j := l.terminalRow(l.movie); j.Status != store.Failed {
		t.Fatalf("the job ended %s (%s), want failed: the server did not hold the node's source digest to its own hash of "+
			"its own copy of the source", j.Status, j.Outcome.Reason)
	}
	failed := srv.waitLog("the server's own refusal", "FAIL (node encode failed, source untouched)", "file="+l.movie)
	for _, want := range []string{"the source the node read is not the server's", sha256Digest(before)} {
		if !strings.Contains(failed, want) {
			t.Errorf("the refusal does not say %q: %s", want, failed)
		}
	}
	if got := linesWith(t, l.serverFFLog, "__transcoding__", "movie"); len(got) != 0 {
		t.Errorf("a gate ran on the working file of a job the server's own hash refused:\n%s", strings.Join(got, "\n"))
	}
	if !bytes.Equal(readAll(t, l.movie), before) {
		t.Error("the source changed")
	}
	l.noTemps()
	if strings.Contains(srv.log(), "node source streamed") {
		t.Error("a ranged request was recorded as a whole source stream")
	}
	if code := srv.stop(); code != 0 {
		t.Errorf("the server exited %d on SIGTERM, want 0", code)
	}
	if j := l.row(l.movie); j.Status != store.Failed || j.FailCount != 1 {
		t.Errorf("status = %s, fail_count = %d, want failed and 1", j.Status, j.FailCount)
	}
}

// tlsPair writes a freshly generated self-signed certificate for 127.0.0.1 and its key
// under dir, and returns their paths. The key exists only in the test's temp directory.
func tlsPair(t *testing.T, dir, name string) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "holdfast test " + name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA: true, BasicConstraintsValid: true,
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}, DNSNames: []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPath, keyPath = filepath.Join(dir, name+"-cert.pem"), filepath.Join(dir, name+"-key.pem")
	writeFile(t, certPath, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), 0o644)
	writeFile(t, keyPath, string(pem.EncodeToMemory(&pem.Block{Type: pemKeyBlock, Bytes: pkcs8})), 0o400)
	return certPath, keyPath
}

// TestWorkerEndToEnd_TLS_ServeWithItsOwnCertificateAndAWorkerThatTrustsIt is the TLS stance
// end to end: `serve` listens with a self-signed pair - the certificate by path, the key by
// reference - and answers nothing in the clear; a worker that does not trust the
// certificate is refused by its own TLS handshake and is granted nothing; the same worker
// with the certificate named in worker_tls_ca completes an http-mode lease over TLS.
func TestWorkerEndToEnd_TLS_ServeWithItsOwnCertificateAndAWorkerThatTrustsIt(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := tlsPair(t, dir, "server")
	keyPEM := string(readAll(t, keyPath))
	l := newNodeLab(t, "vmaf_enable: false\nserver_tls_cert: "+certPath+"\nserver_tls_key: file:"+keyPath+"\n")
	l.unmount()
	before := readAll(t, l.movie)
	srv := l.serve()
	if listening := srv.waitLog("the listening record", "serve listening"); !strings.Contains(listening, "tls=true") {
		t.Fatalf("the listening record does not say TLS is on: %s", listening)
	}

	// NOTHING IS ANSWERED IN THE CLEAR: a plain-http request to the listener gets no API
	// answer, with or without the credential.
	req, _ := http.NewRequest(http.MethodGet, "http://"+l.addr+"/api/summary", nil)
	req.Header.Set("Authorization", "Bearer "+l.token)
	if resp, err := labClient.Do(req); err == nil {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK || bytes.Contains(body, []byte(`"`+"paused"+`"`)) {
			t.Fatalf("a plain-http request to the TLS listener was answered %d %s", resp.StatusCode, body)
		}
	}
	// The same request over TLS, trusting the certificate, is answered - at TLS 1.2 or later.
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(readAll(t, certPath))
	trusting := &http.Client{Timeout: labWait, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	resp, err := trusting.Get("https://" + l.addr + "/api/summary")
	if err != nil {
		t.Fatalf("an https request trusting the server's certificate: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.TLS == nil || resp.TLS.Version < tls.VersionTLS12 {
		t.Fatalf("the https read answered %d over TLS state %+v", resp.StatusCode, resp.TLS)
	}
	// A client that offers nothing newer than TLS 1.1 is refused by the handshake.
	old := &http.Client{Timeout: labWait, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool,
		MinVersion: tls.VersionTLS10, MaxVersion: tls.VersionTLS11}}}
	if resp, err := old.Get("https://" + l.addr + "/api/summary"); err == nil {
		_ = resp.Body.Close()
		t.Error("a TLS 1.1 client was served; the floor is TLS 1.2")
	}

	// A WORKER THAT DOES NOT TRUST THE CERTIFICATE: its handshake fails, so it sends no
	// request - no credential - and the server grants it nothing.
	stranger, strangerWork := l.workerHTTP("stranger", "https", "")
	refused := stranger.waitLog("the untrusting worker's failed call", "worker: asking for work failed")
	if !strings.Contains(refused, "certificate") {
		t.Errorf("the untrusting worker's record does not say the certificate was the reason: %s", refused)
	}
	if code := stranger.stop(); code != 0 {
		t.Errorf("the untrusting worker exited %d on SIGTERM, want 0", code)
	}
	if strings.Contains(srv.log(), "node lease granted") {
		t.Fatalf("a lease was granted to a worker that does not trust the server:\n%s", srv.log())
	}
	if ents, _ := os.ReadDir(strangerWork); len(ents) != 0 {
		t.Errorf("the untrusting worker's work directory holds %d file(s)", len(ents))
	}

	// THE SAME WORKER, TRUSTING IT through worker_tls_ca, completes the lease over TLS.
	wrk, work := l.workerHTTP("nodeA", "https", "worker_tls_ca: "+certPath+"\n")
	granted := srv.waitLog("the lease", "node lease granted", "node=nodeA", "path="+l.movie, "mode=http")
	srv.waitLog("the swap of the node's output", "DONE", "file="+l.movie)
	wrk.waitLog("the worker's own record", "worker: lease done")
	if strings.Contains(wrk.log(), "CLEARTEXT") {
		t.Errorf("a worker speaking https warned about cleartext:\n%s", wrk.log())
	}
	if code := wrk.stop(); code != 0 {
		t.Errorf("the worker exited %d on SIGTERM, want 0\n%s", code, wrk.log())
	}
	l.release()
	srv.waitLog("the local job", "DONE", "file="+l.blocker)
	if code := srv.stop(); code != 0 {
		t.Errorf("the server exited %d on SIGTERM, want 0", code)
	}
	// Nothing of the key, and no credential, reached a log.
	for _, line := range strings.Split(keyPEM, "\n") {
		if len(line) > 20 && (strings.Contains(srv.log(), line) || strings.Contains(wrk.log(), line)) {
			t.Error("a line of the TLS private key reached a log")
		}
	}
	if strings.Contains(srv.log(), l.token) || strings.Contains(wrk.log(), l.token) || strings.Contains(stranger.log(), l.token) {
		t.Error("the node credential reached a log")
	}
	id := leaseIDInLog.FindStringSubmatch(granted)
	st := openStore(t, l.state)
	row, found, err := st.GetLease(context.Background(), id[1])
	_ = st.Close()
	after := readAll(t, l.movie)
	if err != nil || !found || row.State != store.LeaseCompleted || row.SourceDigest != sha256Digest(before) || row.OutputDigest != sha256Digest(after) {
		t.Errorf("the lease over TLS = %+v (%v, found %v), want completed with the source's and the swapped file's digests", row, err, found)
	}
	if j := l.row(l.movie); j.Status != store.Done {
		t.Errorf("status = %s (%s), want done", j.Status, j.Outcome.Reason)
	}
	if ents, _ := os.ReadDir(work); len(ents) != 0 {
		t.Errorf("the worker's work directory holds %d file(s) after the lease", len(ents))
	}
	l.noTemps()
}

// TestServeTLS_APairThatCannotBeListenedWithRefusesToStart: half a pair refuses naming the
// missing key; a literal key refuses as a literal; a certificate that cannot be read, a
// file that holds none, a key that does not parse and a key that belongs to another
// certificate each refuse naming the key at fault - and no refusal prints a byte of the
// private key.
func TestServeTLS_APairThatCannotBeListenedWithRefusesToStart(t *testing.T) {
	dir := t.TempDir()
	lib := filepath.Join(dir, "media")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	certA, keyA := tlsPair(t, dir, "a")
	_, keyB := tlsPair(t, dir, "b")
	noCert := filepath.Join(dir, "no-cert.pem")
	writeFile(t, noCert, "this file holds no certificate\n", 0o644)
	garbage := filepath.Join(dir, "garbage-key.pem")
	const marker = "GARBAGE-KEY-MATERIAL-THAT-MUST-NEVER-BE-PRINTED"
	writeFile(t, garbage, marker+"\n", 0o400)
	base := "library_roots:\n  - " + lib + "\nstate_dir: " + filepath.Join(dir, "state") + "\nserver_addr: 127.0.0.1:0\n"

	secrets := []string{marker}
	for _, p := range []string{keyA, keyB} {
		for _, line := range strings.Split(string(readAll(t, p)), "\n") {
			if len(line) > 20 && !strings.HasPrefix(line, "-----") {
				secrets = append(secrets, line)
			}
		}
	}
	for name, tc := range map[string]struct {
		cfg   string
		wants []string
	}{
		"a certificate with no key": {"server_tls_cert: " + certA + "\n", []string{"server_tls_cert is set", "server_tls_key is not"}},
		"a key with no certificate": {"server_tls_key: file:" + keyA + "\n", []string{"server_tls_key is set", "server_tls_cert is not"}},
		"a literal key":             {"server_tls_cert: " + certA + "\nserver_tls_key: " + marker + "\n", []string{"server_tls_key", "file:", "cmd:"}},
		"a certificate that cannot be read": {"server_tls_cert: " + filepath.Join(dir, "missing.pem") + "\nserver_tls_key: file:" + keyA + "\n",
			[]string{"refusing to start", "server_tls_cert", "could not be read"}},
		"a certificate file that holds none": {"server_tls_cert: " + noCert + "\nserver_tls_key: file:" + keyA + "\n",
			[]string{"refusing to start", "server_tls_cert", "holds no PEM certificate"}},
		"a key that is not a key": {"server_tls_cert: " + certA + "\nserver_tls_key: file:" + garbage + "\n",
			[]string{"refusing to start", "server_tls_key", "is not a PEM private key that belongs to the certificate"}},
		"the key of another certificate": {"server_tls_cert: " + certA + "\nserver_tls_key: file:" + keyB + "\n",
			[]string{"refusing to start", "server_tls_key", "is not a PEM private key that belongs to the certificate"}},
		"a key file where the certificate should be": {"server_tls_cert: " + keyA + "\nserver_tls_key: file:" + keyA + "\n",
			[]string{"refusing to start", "server_tls_cert", "holds no PEM certificate"}},
	} {
		t.Run(name, func(t *testing.T) {
			cfgPath := filepath.Join(t.TempDir(), "config.yaml")
			writeFile(t, cfgPath, base+tc.cfg, 0o600)
			var out, errOut bytes.Buffer
			code := dispatch([]string{"serve", "--config", cfgPath}, &out, &errOut)
			msg := errOut.String() + out.String()
			if code == 0 {
				t.Fatalf("serve started: %s", msg)
			}
			for _, want := range tc.wants {
				if !strings.Contains(msg, want) {
					t.Errorf("the refusal does not say %q:\n%s", want, msg)
				}
			}
			for _, s := range secrets {
				if strings.Contains(msg, s) {
					t.Errorf("the refusal prints key material:\n%s", msg)
				}
			}
			if _, err := os.Stat(filepath.Join(dir, "state", "jobs.db")); err == nil {
				t.Error("the store was opened before the refusal")
			}
		})
	}

	// serverTLS itself: off with neither key, a typed error naming the key otherwise, and a
	// good pair gives a configuration with one certificate and a TLS 1.2 floor.
	if c, err := serverTLS(&config.Config{}, secret.Value{}); c != nil || err != nil {
		t.Errorf("serverTLS with neither key = %v, %v, want nil and no error", c, err)
	}
	good, err := serverTLS(&config.Config{ServerTLSCert: " " + certA + " ", ServerTLSKey: "file:" + keyA}, secret.NewValue(string(readAll(t, keyA))))
	if err != nil || good == nil || len(good.Certificates) != 1 || good.MinVersion != tls.VersionTLS12 {
		t.Fatalf("serverTLS with a good pair = %+v, %v, want one certificate and a TLS 1.2 floor", good, err)
	}
	if good.ClientAuth != tls.NoClientCert || good.InsecureSkipVerify {
		t.Error("the listener's TLS configuration asks for a client certificate or skips verification; neither is this build's")
	}
	for name, tc := range map[string]struct {
		cert, key string
		at        string
	}{
		"an empty resolved key":  {certA, "", config.ServerTLSKeyKey},
		"another pair's key":     {certA, string(readAll(t, keyB)), config.ServerTLSKeyKey},
		"a key that is garbage":  {certA, marker, config.ServerTLSKeyKey},
		"a missing certificate":  {filepath.Join(dir, "missing.pem"), string(readAll(t, keyA)), config.ServerTLSCertKey},
		"a certificate-less pem": {noCert, string(readAll(t, keyA)), config.ServerTLSCertKey},
	} {
		c, err := serverTLS(&config.Config{ServerTLSCert: tc.cert, ServerTLSKey: "file:/somewhere"}, secret.NewValue(tc.key))
		var pairErr *TLSPairError
		if c != nil || !errors.As(err, &pairErr) || pairErr.Key != tc.at || !strings.HasPrefix(err.Error(), tc.at+": ") {
			t.Errorf("%s: serverTLS = %v, %v, want a *TLSPairError naming %s", name, c, err, tc.at)
			continue
		}
		for _, s := range secrets {
			if strings.Contains(err.Error(), s) || strings.Contains(fmt.Sprintf("%+v %#v", err, err), s) {
				t.Errorf("%s: the error prints key material", name)
			}
		}
		if errors.Unwrap(err) != nil {
			t.Errorf("%s: the error wraps %v; it must wrap nothing that could carry the key", name, errors.Unwrap(err))
		}
	}
	for data, want := range map[string]bool{
		"": false, "not pem": false, string(readAll(t, certA)): true, string(readAll(t, keyA)): false,
		string(readAll(t, keyA)) + string(readAll(t, certA)): true, string(readAll(t, certA)) + "trailing junk": true,
	} {
		if got := holdsPEM([]byte(data), "CERTIFICATE"); got != want {
			t.Errorf("holdsPEM(%d bytes) = %v, want %v", len(data), got, want)
		}
	}
}

// failingTransport fails every request at once, so a worker under test dials nothing.
type failingTransport struct {
	mu    sync.Mutex
	hosts []string
}

func (f *failingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	f.hosts = append(f.hosts, r.URL.Scheme+"://"+r.URL.Host)
	f.mu.Unlock()
	return nil, errors.New("no network in this test")
}

// TestWorker_TransportAndModeRefusalsAndTheInsecureOverride is the worker command's own
// start-or-refuse decisions about its transport and its mode, through the real CLI:
//
//   - plain http:// to a host that is not loopback refuses, before the credential is
//     resolved, and says which key accepts the risk;
//   - with worker_insecure_http: true it starts, and says so at warn level at that start;
//   - the loopback forms and https start without the key and say nothing;
//   - worker_tls_ca that cannot be read, or holds no certificate, refuses naming the key;
//   - an http worker needs no library root, and one that names a root or a path map is
//     refused by name; `run` and `serve` still refuse a file with no library root.
func TestWorker_TransportAndModeRefusalsAndTheInsecureOverride(t *testing.T) {
	ffmpeg := envOr("HOLDFAST_FFMPEG", "ffmpeg")
	ffprobe := envOr("HOLDFAST_FFPROBE", "ffprobe")
	dir := t.TempDir()
	lib := filepath.Join(dir, "media")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(dir, "token")
	writeFile(t, token, "node-credential-of-this-test\n", 0o400)
	emptyCA := filepath.Join(dir, "empty-ca.pem")
	writeFile(t, emptyCA, "", 0o644)
	mapped := "library_roots:\n  - " + lib + "\nstate_dir: " + filepath.Join(dir, "state") + "\nnode_token: file:" + token + "\n"
	rootless := "state_dir: " + filepath.Join(dir, "state") + "\nnode_token: file:" + token + "\nworker_mode: http\n"

	t.Run("refusals", func(t *testing.T) {
		for name, tc := range map[string]struct {
			cfg   string
			wants []string
		}{
			"plain http, not loopback": {mapped + "worker_server: http://192.0.2.10:8080\n",
				[]string{"refusing to start", "cleartext", "worker_insecure_http: true", "https://"}},
			"plain http, not loopback, the key written false": {mapped + "worker_server: http://192.0.2.10:8080\nworker_insecure_http: false\n",
				[]string{"refusing to start", "cleartext"}},
			"plain http to a name that only starts like localhost": {mapped + "worker_server: http://localhost.example.net:8080\n",
				[]string{"refusing to start", "cleartext"}},
			"http mode, plain http, not loopback": {rootless + "worker_server: http://192.0.2.10:8080\n",
				[]string{"refusing to start", "cleartext"}},
			"an unreadable worker_tls_ca": {mapped + "worker_server: https://192.0.2.10:8443\nworker_tls_ca: " + filepath.Join(dir, "missing-ca.pem") + "\n",
				[]string{"refusing to start", "worker_tls_ca", "could not be read"}},
			"an empty worker_tls_ca": {mapped + "worker_server: https://192.0.2.10:8443\nworker_tls_ca: " + emptyCA + "\n",
				[]string{"refusing to start", "worker_tls_ca", "holds no PEM certificate"}},
			"http mode naming a library root": {"library_roots:\n  - " + lib + "\n" + rootless + "worker_server: http://127.0.0.1:9\n",
				[]string{"invalid config", "worker_mode is http", "library_roots is set"}},
			"http mode with a path map": {rootless + "worker_server: http://127.0.0.1:9\nworker_path_map:\n  - {from: /a, to: /b}\n",
				[]string{"invalid config", "worker_mode is http", "worker_path_map is set"}},
			"mapped mode with no library root": {"state_dir: " + filepath.Join(dir, "state") + "\nnode_token: file:" + token + "\nworker_server: http://127.0.0.1:9\n",
				[]string{"invalid config", "library_roots is empty"}},
			"an unknown mode": {mapped + "worker_server: http://127.0.0.1:9\nworker_mode: streaming\n",
				[]string{"invalid config", "worker_mode", "mapped|http"}},
		} {
			t.Run(name, func(t *testing.T) {
				cfgPath := filepath.Join(t.TempDir(), "config.yaml")
				writeFile(t, cfgPath, tc.cfg, 0o600)
				var out, errOut bytes.Buffer
				code := dispatch([]string{"worker", "--config", cfgPath}, &out, &errOut)
				msg := errOut.String()
				if code != 1 {
					t.Fatalf("worker exited %d, want 1: %s", code, msg)
				}
				for _, want := range tc.wants {
					if !strings.Contains(msg, want) {
						t.Errorf("the refusal does not say %q:\n%s", want, msg)
					}
				}
			})
		}
	})

	t.Run("an http worker's file with no library root", func(t *testing.T) {
		cfgPath := filepath.Join(t.TempDir(), "worker.yaml")
		writeFile(t, cfgPath, rootless+"worker_server: https://holdfast.example.net\n", 0o600)
		var out, errOut bytes.Buffer
		if code := dispatch([]string{"validate", "--config", cfgPath}, &out, &errOut); code != 0 ||
			!strings.Contains(out.String(), "config OK") || !strings.Contains(out.String(), "http mode") {
			t.Errorf("validate on an http worker's file = %d %q %q, want config OK naming http mode", code, out.String(), errOut.String())
		}
		for _, cmd := range []string{"run", "serve"} {
			var out, errOut bytes.Buffer
			if code := dispatch([]string{cmd, "--config", cfgPath}, &out, &errOut); code == 0 || !strings.Contains(errOut.String(), "library_roots is empty") {
				t.Errorf("%s on a file with no library root = %d %q, want the empty-roots refusal", cmd, code, errOut.String())
			}
		}
		// And a file that names no root and is NOT an http worker's is not validated as one.
		plain := filepath.Join(t.TempDir(), "plain.yaml")
		writeFile(t, plain, "state_dir: "+filepath.Join(dir, "state")+"\n", 0o600)
		out.Reset()
		errOut.Reset()
		if code := dispatch([]string{"validate", "--config", plain}, &out, &errOut); code == 0 || !strings.Contains(errOut.String(), "library_roots is empty") {
			t.Errorf("validate on a rootless file that is not an http worker's = %d %q", code, errOut.String())
		}
	})

	// start runs the real worker command's body until it has asked for work once, with a
	// transport that dials nothing, and returns what it logged.
	start := func(t *testing.T, cfgText string) (logged string, hosts []string, code int) {
		t.Helper()
		cfgPath := filepath.Join(t.TempDir(), "config.yaml")
		writeFile(t, cfgPath, cfgText+"worker_name: nodeA\nworker_work_dir: "+filepath.Join(t.TempDir(), "work")+"\npreset: ultrafast\n", 0o600)
		cfg, err := config.Load(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := cfg.ValidateWorker(); err != nil {
			t.Fatalf("the configuration was refused: %v", err)
		}
		out := &syncBuffer{}
		log := slog.New(slog.NewTextHandler(out, nil))
		transport := &failingTransport{}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan int, 1)
		var errOut syncBuffer
		go func() {
			done <- runWorker(ctx, cfg, log, &errOut, ffmpeg, ffprobe, func(o *nodeworker.Options) {
				o.HTTP = &http.Client{Transport: transport}
				o.MinBackoff, o.MaxBackoff = 10*time.Millisecond, 20*time.Millisecond
			})
		}()
		deadline := time.Now().Add(labWait)
		for !strings.Contains(out.String(), "asking for work failed") {
			select {
			case code := <-done:
				t.Fatalf("the worker exited %d before it asked for work: %s\n%s", code, errOut.String(), out.String())
			default:
			}
			if time.Now().After(deadline) {
				t.Fatalf("the worker never asked for work:\n%s", out.String())
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
		select {
		case code = <-done:
		case <-time.After(labWait):
			t.Fatal("the worker did not stop")
		}
		transport.mu.Lock()
		hosts = append(hosts, transport.hosts...)
		transport.mu.Unlock()
		return out.String(), hosts, code
	}

	t.Run("worker_insecure_http starts the worker and warns at that start", func(t *testing.T) {
		for _, cfg := range []string{mapped, rootless} {
			logged, hosts, code := start(t, cfg+"worker_server: http://192.0.2.10:8080\nworker_insecure_http: true\n")
			if code != 0 {
				t.Errorf("the worker exited %d, want 0", code)
			}
			if len(hosts) == 0 || hosts[0] != "http://192.0.2.10:8080" {
				t.Errorf("the worker asked %v for work, want the plain-http server it was allowed", hosts)
			}
			warn := ""
			for _, line := range strings.Split(logged, "\n") {
				if strings.Contains(line, "worker_insecure_http") {
					warn = line
				}
			}
			for _, want := range []string{"level=WARN", "THE NODE CREDENTIAL CROSSES THE NETWORK IN CLEARTEXT ON EVERY REQUEST",
				"in http mode so does the library's media", "worker_server=http://192.0.2.10:8080"} {
				if !strings.Contains(warn, want) {
					t.Errorf("the start-up warning does not say %q: %q", want, warn)
				}
			}
			if strings.Index(logged, "worker_insecure_http") > strings.Index(logged, "worker starting") {
				t.Error("the warning came after the worker had started")
			}
			if strings.Contains(logged, "node-credential-of-this-test") {
				t.Error("the credential reached the log")
			}
		}
	})

	t.Run("loopback and https start without the key and say nothing", func(t *testing.T) {
		for server, insecures := range map[string][]string{
			"http://127.0.0.1:9": {"", "worker_insecure_http: true\n"}, "http://[::1]:9": {""}, "http://localhost:9": {""},
			"https://holdfast.example.net": {"worker_insecure_http: true\n"},
		} {
			for _, insecure := range insecures {
				logged, hosts, code := start(t, mapped+"worker_server: "+server+"\n"+insecure)
				if code != 0 || len(hosts) == 0 || hosts[0] != server {
					t.Errorf("%s: the worker exited %d having asked %v", server, code, hosts)
				}
				if strings.Contains(logged, "CLEARTEXT") || strings.Contains(logged, "worker_insecure_http") {
					t.Errorf("%s (%q): the worker warned about cleartext:\n%s", server, insecure, logged)
				}
			}
		}
	})
}
