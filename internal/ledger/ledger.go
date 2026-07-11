// Package ledger is the resumable processed-file record for the transcoder. It is
// an append-only TSV (`<status>\t<key>\t<path>`, status ∈ done|skipped|failed)
// keyed by path + a size:mtime fingerprint, mirroring the bash transcoder's ledger.
// A `done`/`skipped` row is permanent; a `failed` row is retryable up to a bound
// (a transient ENOSPC/OOM must not exclude a file forever). TRANSCODE-5 replaces
// this flat file with a SQLite/WAL jobs table; the semantics are preserved.
package ledger

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Status values recorded in the ledger.
const (
	Done    = "done"
	Skipped = "skipped"
	Failed  = "failed"
)

// Ledger is a handle to the TSV file at Path.
type Ledger struct {
	Path string
}

// New returns a Ledger backed by the file at path.
func New(path string) *Ledger { return &Ledger{Path: path} }

// scan reads the ledger and calls fn for each well-formed row (status, key, path).
// A missing file is not an error (no rows). Malformed lines are skipped.
func (l *Ledger) scan(fn func(status, key, path string)) error {
	f, err := os.Open(l.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024) // tolerate long paths
	for sc.Scan() {
		parts := strings.SplitN(sc.Text(), "\t", 3)
		if len(parts) != 3 {
			continue
		}
		fn(parts[0], parts[1], parts[2])
	}
	return sc.Err()
}

// Status returns the LAST recorded status for the given path+key, or "" if none.
// Last-wins mirrors the bash `awk … {s=$1} END{print s}` so a later row overrides
// an earlier one (e.g. a file that failed then later succeeded).
func (l *Ledger) Status(path, key string) (string, error) {
	var status string
	err := l.scan(func(s, k, p string) {
		if p == path && k == key {
			status = s
		}
	})
	return status, err
}

// FailCount returns how many times path+key has been recorded `failed`.
func (l *Ledger) FailCount(path, key string) (int, error) {
	n := 0
	err := l.scan(func(s, k, p string) {
		if s == Failed && p == path && k == key {
			n++
		}
	})
	return n, err
}

// Record appends a `<status>\t<key>\t<path>` row, creating the parent dir if
// needed. Append is O_APPEND so concurrent single-line writes cannot interleave.
func (l *Ledger) Record(status, key, path string) error {
	if err := os.MkdirAll(filepath.Dir(l.Path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(l.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "%s\t%s\t%s\n", status, key, path)
	return err
}
