package engine

// Owner records: which process owns a work-in-progress temp beside a source, written so
// that ANOTHER process can later ask one question of it and get a trustworthy answer - is
// that owner provably dead?
//
// Why the question matters. A bounded run used to skip the stale-temp sweep because a pass
// that did not list the whole library has no evidence a temp is orphaned, so a bounded run
// killed with SIGKILL, or by the OOM killer, left its working file beside the source until
// some later unbounded pass swept it. An owner record IS that evidence, and it is evidence
// about the temp itself rather than about the library, so a bounded run can act on it
// wherever the temp is and whatever the run's bound reaches.
//
// THE FORM: a record the owner holds an open file description lock on for as long as it
// owns the temp. The record is one small JSON file per temp under the state directory
// (TempOwnersDirName), named by a digest of the temp's path, so it is found from the temp
// and the temp is found from it - neither needs a listing of the library. The owner takes
// an exclusive open-file-description lock (F_OFD_SETLK) on the record BEFORE the first
// byte of the temp exists, keeps it until it has finished with the temp, and removes the
// record on every exit path it lives through. A SIGKILL runs none of that: the record stays
// on disk, and the kernel drops the lock when it tears the process down.
//
// THE EVIDENCE: another process may take the same lock only once nothing holds it. So a
// record whose lock this process could take, whose file is still the one linked at the
// record's name, whose content this build reads, and which lives on storage this build
// classifies local (internal/fsclass), belongs to an owner that no longer exists. That is
// the ONLY answer that reads as provably dead. A lock somebody holds reads as alive. Every
// other answer - a record that cannot be opened, read or parsed, one at a format version
// this build does not read, a lock call that errors, storage fsclass does not call local -
// is "cannot decide", and a bounded run leaves the temp where it is.
//
// Why a process id is not the evidence. Ids are reused, and inside containers two processes
// can share one, so "no process has this id" and "a process has this id" both prove
// nothing. The record carries the id and the host for the operator to read and nothing here
// decides on them. The lock needs no identity at all: it belongs to the open file
// description, the kernel releases it when the last descriptor on it closes, and it
// conflicts across process-id namespaces on one kernel because it lives on the file, not on
// the process. It is an OFD lock rather than flock(2) because an OFD lock is a POSIX
// record lock, so it also conflicts with a lock another host takes through an NFS server
// exporting this directory; and rather than a classic fcntl lock because those do not
// conflict within one process, and a sweep must see its own process's in-flight temps as
// alive.
//
// Where a lock is not evidence. On storage this build does not classify local - a network
// filesystem, FUSE, overlay, tmpfs - a lock may be taken and released by rules this process
// cannot see, and a lock it CAN take is not proof that nobody else holds one. So there the
// answer is "cannot decide", which degrades to the behaviour before owner records existed:
// the temp waits for an unbounded pass.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/NSchatz/holdfast/internal/fsclass"
)

// TempOwnersDirName is the directory, under the state directory, that holds the owner
// records.
const TempOwnersDirName = "temp-owners"

const (
	// tempOwnerFormat names what a record is, so a file that merely parses as JSON is not
	// mistaken for one.
	tempOwnerFormat = "holdfast-temp-owner"
	// tempOwnerVersion is the record format this build writes and the only one it reads
	// (data-migration M6). A reader meeting any other version answers "cannot decide".
	tempOwnerVersion = 1
	// tempOwnerExt is the owner record's file extension; nothing else in the directory is
	// read as a record.
	tempOwnerExt = ".owner"
)

// takeAttempts and takeBackoff bound how long a job waits for a record another process has
// locked. The only lawful holder besides a live owner of the same temp path is a sweep
// deciding a dead record at that path, which holds it for the few milliseconds a removal
// takes; anything held longer is a live owner, and the job fails rather than waits.
const (
	takeAttempts = 50
	takeBackoff  = 20 * time.Millisecond
)

// errRecordBusy is a lock another open file description holds.
var errRecordBusy = errors.New("the owner record is locked by another open file description")

// tempOwnerRecord is the stored form of an owner record.
type tempOwnerRecord struct {
	Format  string `json:"format"`
	Version int    `json:"version"`
	// Temp is the working file this record is about. The record's own name is derived
	// from it, and a record whose name was not derived from its Temp is not read.
	Temp string `json:"temp"`
	// Source is the file the temp is an encode of.
	Source string `json:"source"`
	// PID, Host and Started identify the owner FOR THE OPERATOR. Nothing decides on them.
	PID     int    `json:"pid"`
	Host    string `json:"host"`
	Started string `json:"started"`
}

// tempOwners is where this engine keeps its owner records and how it classifies the
// storage they are on.
type tempOwners struct {
	dir    string
	lookup fsclass.Lookup
}

// TrackTempOwners turns owner records on: dir is the directory they live in (the state
// directory's TempOwnersDirName), and lookup is the filesystem-type lookup the liveness
// decision classifies that directory with (nil is the real one). `run` and `serve` call it
// on every engine they build. An engine it was never called on writes no record, and a
// bounded run on it has no owner evidence to act on, so it removes nothing and says so.
func (e *Engine) TrackTempOwners(dir string, lookup fsclass.Lookup) {
	e.owners = &tempOwners{dir: dir, lookup: lookup}
}

// recordPath is where the owner record for temp lives.
func (o *tempOwners) recordPath(temp string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(temp)))
	return filepath.Join(o.dir, hex.EncodeToString(sum[:])+tempOwnerExt)
}

// picturesBeside is every attached-picture file a job writing temp names after it
// (picturePath) that is on disk, in order. Pictures are copied out one after another, so
// the first index with nothing at it ends the list.
func picturesBeside(temp string) []string {
	var out []string
	for i := 0; i < maxPathCandidates; i++ {
		p := picturePath(temp, i)
		if _, err := os.Lstat(p); err != nil {
			break
		}
		out = append(out, p)
	}
	return out
}

// pictureSuffix matches the ".picture<N>" a picture file adds to its working file's name.
var pictureSuffix = regexp.MustCompile(`\.picture[0-9]+$`)

// ownerKey is the temp whose owner record decides path's fate: the working file itself, or
// for an attached-picture file the working file it was named after. A picture file whose
// name had to be shortened (picturePath's digest form) no longer carries the working file's
// name and answers for itself, which finds no record.
func ownerKey(path string) string {
	if loc := pictureSuffix.FindStringIndex(path); loc != nil {
		if base := path[:loc[0]]; isTempName(filepath.Base(base)) {
			return base
		}
	}
	return path
}

// ownedTemp is a record its owner holds: the open, locked record file.
type ownedTemp struct {
	f    *os.File
	path string
}

// ownTemp writes the owner record for temp, the working file this job is about to create
// beside source, and returns the handle whose release clears it. It runs BEFORE the first
// byte of temp is written. An engine with no owner records returns a nil handle, whose
// release does nothing.
func (e *Engine) ownTemp(temp, source string) (*ownedTemp, error) {
	if e.owners == nil || !ownerLocksSupported {
		return nil, nil
	}
	return e.owners.take(temp, source)
}

// take creates or takes over the record for temp, locks it, and writes it.
func (o *tempOwners) take(temp, source string) (*ownedTemp, error) {
	if err := os.MkdirAll(o.dir, 0o755); err != nil {
		return nil, fmt.Errorf("create the owner-record directory %s: %w", o.dir, err)
	}
	rec := o.recordPath(temp)
	host, _ := os.Hostname()
	body, err := json.Marshal(tempOwnerRecord{
		Format: tempOwnerFormat, Version: tempOwnerVersion,
		Temp: filepath.Clean(temp), Source: source,
		PID: os.Getpid(), Host: host, Started: time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return nil, err
	}
	for attempt := 1; ; attempt++ {
		f, err := os.OpenFile(rec, os.O_RDWR|os.O_CREATE, 0o644)
		if err != nil {
			return nil, fmt.Errorf("open the owner record %s: %w", rec, err)
		}
		lerr := lockRecord(f)
		// A record a sweep cleared between this open and this lock is a name this handle
		// no longer holds: taking it would own a file nobody can find. Open it again.
		if lerr == nil && !stillLinked(f, rec) {
			lerr = errRecordBusy
		}
		if errors.Is(lerr, errRecordBusy) {
			_ = f.Close()
			if attempt >= takeAttempts {
				return nil, fmt.Errorf("the owner record %s for %s is held by another live process", rec, temp)
			}
			time.Sleep(takeBackoff)
			continue
		}
		if lerr != nil {
			_ = f.Close()
			return nil, fmt.Errorf("lock the owner record %s: %w", rec, lerr)
		}
		if err := writeRecord(f, body); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("write the owner record %s: %w", rec, err)
		}
		return &ownedTemp{f: f, path: rec}, nil
	}
}

// release clears the record and drops the lock. The record is removed while the lock is
// still held and only where the name still links to this handle's file, so a record a
// later job has since taken over at the same name is never the one removed.
func (h *ownedTemp) release() {
	if h == nil {
		return
	}
	if stillLinked(h.f, h.path) {
		_ = os.Remove(h.path)
	}
	_ = h.f.Close()
}

// writeRecord replaces f's content with body and makes it durable.
func writeRecord(f *os.File, body []byte) error {
	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := f.WriteAt(body, 0); err != nil {
		return err
	}
	return f.Sync()
}

// stillLinked reports whether the name path still links to the file f has open.
func stillLinked(f *os.File, path string) bool {
	held, err := f.Stat()
	if err != nil {
		return false
	}
	named, err := os.Lstat(path)
	if err != nil {
		return false
	}
	return os.SameFile(held, named)
}

// ownerState is what the liveness decision found out about a temp's owner.
type ownerState int

const (
	// ownerNoRecord: no record names this temp, which is every temp an older build wrote.
	ownerNoRecord ownerState = iota
	// ownerAlive: another open file description holds the record's lock.
	ownerAlive
	// ownerDead: the affirmative evidence described at the top of this file.
	ownerDead
	// ownerUndecided: anything else. It is NOT provably dead.
	ownerUndecided
)

// String is the state as the log records spell it.
func (s ownerState) String() string {
	switch s {
	case ownerNoRecord:
		return "no-record"
	case ownerAlive:
		return "alive"
	case ownerDead:
		return "dead"
	}
	return "undecided"
}

// ownerVerdict is the liveness decision for one owner record, and while it is open, the
// record's lock where this process took it.
type ownerVerdict struct {
	state ownerState
	// why is the reason, for every state but ownerDead.
	why string
	// record is the owner record's path; rec its content where it could be read.
	record string
	rec    tempOwnerRecord
	parsed bool
	// held is the record's lock, taken by THIS decision. While it is held no owner can take
	// the record over, so what the decision found stays true until close.
	held *os.File
}

// temp is the temp the record names, or "" where it could not be read.
func (v *ownerVerdict) temp() string {
	if v.parsed {
		return v.rec.Temp
	}
	return ""
}

// close drops the lock this decision took. clear also removes the record first, which is
// for a record whose temp the sweep has just removed: it no longer describes anything.
func (v *ownerVerdict) close(clear bool) {
	if v.held == nil {
		return
	}
	if clear && stillLinked(v.held, v.record) {
		_ = os.Remove(v.record)
	}
	_ = v.held.Close()
	v.held = nil
}

// ownerOf decides the owner of path's temp (ownerKey).
func (e *Engine) ownerOf(path string) *ownerVerdict {
	if e.owners == nil {
		return &ownerVerdict{state: ownerNoRecord, why: "this engine keeps no owner records"}
	}
	return e.owners.inspect(e.owners.recordPath(ownerKey(path)), ownerKey(path))
}

// inspect is THE liveness decision, for the record at path. want is the temp the caller
// found the record through, or "" when it found the record by listing the records.
func (o *tempOwners) inspect(path, want string) *ownerVerdict {
	v := &ownerVerdict{record: path}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if errors.Is(err, fs.ErrNotExist) {
		v.state, v.why = ownerNoRecord, "no owner record names this temp"
		return v
	}
	if err != nil {
		v.state, v.why = ownerUndecided, "the owner record could not be opened: "+err.Error()
		return v
	}
	switch lerr := lockRecord(f); {
	case errors.Is(lerr, errRecordBusy):
		// Read for the operator's sake only: what the record says does not change the
		// answer, which the lock already gave.
		v.rec, v.parsed = readRecord(f)
		_ = f.Close()
		v.state, v.why = ownerAlive, "the owner record is locked by a live process"
		return v
	case lerr != nil:
		_ = f.Close()
		v.state, v.why = ownerUndecided, "the owner record's lock could not be asked about: "+lerr.Error()
		return v
	}
	v.held = f
	if !stillLinked(f, path) {
		v.close(false)
		v.state, v.why = ownerUndecided, "the owner record was replaced or cleared while it was being checked"
		return v
	}
	rec, err := decodeRecord(f)
	if err != nil {
		v.state, v.why = ownerUndecided, err.Error()
		return v
	}
	if o.recordPath(rec.Temp) != path || (want != "" && filepath.Clean(want) != rec.Temp) {
		v.state, v.why = ownerUndecided, "the owner record names a temp its own name was not derived from"
		return v
	}
	v.rec, v.parsed = rec, true
	if class := fsclass.Of(o.lookup, o.dir); !class.IsLocal() {
		v.state = ownerUndecided
		v.why = "the owner record is on storage classified " + class.String()
		if class.Reason != "" {
			v.why += " (" + class.Reason + ")"
		}
		v.why += ", where a lock this process can take is not evidence that no other process holds one"
		return v
	}
	v.state = ownerDead
	return v
}

// readRecord is decodeRecord where only the answer matters and not why it failed.
func readRecord(f *os.File) (tempOwnerRecord, bool) {
	rec, err := decodeRecord(f)
	return rec, err == nil
}

// decodeRecord reads and checks one record, branching on its format version.
func decodeRecord(f *os.File) (tempOwnerRecord, error) {
	raw, err := io.ReadAll(io.NewSectionReader(f, 0, 1<<16))
	if err != nil {
		return tempOwnerRecord{}, errors.New("the owner record could not be read: " + err.Error())
	}
	var probe struct {
		Format  string `json:"format"`
		Version int    `json:"version"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil || probe.Format != tempOwnerFormat {
		return tempOwnerRecord{}, errors.New("the owner record is malformed")
	}
	switch probe.Version {
	case tempOwnerVersion:
		var rec tempOwnerRecord
		if err := json.Unmarshal(raw, &rec); err != nil || rec.Temp == "" || !filepath.IsAbs(rec.Temp) {
			return tempOwnerRecord{}, errors.New("the owner record is malformed")
		}
		if !isTempName(filepath.Base(rec.Temp)) {
			// A record may only ever license removing a work-in-progress temp. One naming
			// anything else is not read, whatever else it says.
			return tempOwnerRecord{}, errors.New("the owner record names a file that is not a temp")
		}
		return rec, nil
	default:
		return tempOwnerRecord{}, fmt.Errorf("the owner record is at format version %d, which this build does not read", probe.Version)
	}
}

// ownerRecords lists the owner records, by path. A directory that does not exist yet holds
// none.
func (o *tempOwners) ownerRecords() ([]string, error) {
	ents, err := os.ReadDir(o.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, ent := range ents {
		if strings.HasSuffix(ent.Name(), tempOwnerExt) {
			out = append(out, filepath.Join(o.dir, ent.Name()))
		}
	}
	return out, nil
}
