package audio

import (
	"io"
	"os"
	"sync"
)

// Reports are the channels an encode's second-pass loudness reports come back on: one pipe
// per normalised track, the write end inherited by ffmpeg at StatsFD(k), the read end drained
// here. A report is read back matched to its track by the descriptor it arrived on, never by
// its position in a log.
type Reports struct {
	r, w []*os.File
	wg   sync.WaitGroup
	got  [][]byte
}

// OpenReports opens n report channels. Zero opens none, and returns a Reports with nothing to
// hand over.
func OpenReports(n int) (*Reports, error) {
	rep := &Reports{got: make([][]byte, n)}
	for i := 0; i < n; i++ {
		r, w, err := os.Pipe()
		if err != nil {
			rep.Close()
			return nil, err
		}
		rep.r, rep.w = append(rep.r, r), append(rep.w, w)
	}
	return rep, nil
}

// ExtraFiles places the write ends after the descriptors before FirstStatsFD: progress is
// the encode's descriptor 3 where it has one (nil leaves descriptor 3 closed in the child).
func (rep *Reports) ExtraFiles(progress *os.File) []*os.File {
	if rep == nil || len(rep.w) == 0 {
		if progress == nil {
			return nil
		}
		return []*os.File{progress}
	}
	return append([]*os.File{progress}, rep.w...)
}

// Started closes this process's copies of the write ends, now that the child holds them, and
// starts draining each read end, so a report is never blocked on a full pipe.
func (rep *Reports) Started() {
	if rep == nil {
		return
	}
	for _, w := range rep.w {
		_ = w.Close()
	}
	rep.w = nil
	for i, r := range rep.r {
		rep.wg.Add(1)
		go func(i int, r *os.File) {
			defer rep.wg.Done()
			b, _ := io.ReadAll(r)
			rep.got[i] = b
		}(i, r)
	}
}

// Collect waits for every channel to close - the child has exited - and returns what came
// back on each, by k.
func (rep *Reports) Collect() [][]byte {
	if rep == nil {
		return nil
	}
	rep.wg.Wait()
	return rep.got
}

// Close releases every descriptor still open.
func (rep *Reports) Close() {
	if rep == nil {
		return
	}
	for _, f := range append(rep.r, rep.w...) {
		_ = f.Close()
	}
	rep.r, rep.w = nil, nil
}

// Mode is the loudness mode one normalised track's second pass reported, read off its own
// report: "linear" or "dynamic", and ModeNotRecorded where no readable report came back. It is
// never inferred from the first pass's figures.
func Mode(report []byte) string {
	st, err := ParseStats(report)
	if err != nil {
		return ModeNotRecorded
	}
	return st.Mode
}
