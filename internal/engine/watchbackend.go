package engine

// The EVENT SOURCE the filesystem watch runs on, and the one call site of
// github.com/fsnotify/fsnotify in this repository.
//
// It is its own file, and its own interface, for two reasons. The standard library has no
// filesystem-event API at all, so the alternative to this module is per-platform syscall
// code here - which is what earns the dependency. And the watch has to be gradeable
// without one: a test drives the event source it stands in for, never the watch itself,
// so the settle rule and the fallback decision are exercised for real on a gate that
// cannot make a kernel drop an event queue.
//
// fsnotify exposes no way to ask which backend it obtained. What this build CAN say
// honestly is which backend fsnotify carries for this platform, and that a watcher was
// obtained at all - so the mechanism is derived from the platform and is only ever
// reported once the constructor has returned one. A platform this build has no name for
// is treated as having no mechanism, which is the fail-safe direction: a watch that
// reported a mechanism it did not get would be exactly the false report AC-2 forbids.

import (
	"runtime"

	"github.com/fsnotify/fsnotify"
)

// watchEvent is one filesystem event reduced to the two facts the watch decides on: the
// path it names, and whether it says that path is no longer there under that name. A
// rename is GONE for the old name; the new name arrives as its own event.
type watchEvent struct {
	Path string
	Gone bool
}

// watchBackend is the event source. Add registers one directory - fsnotify is not
// recursive, so the descriptor count is one per directory and grows with the tree, which
// is the cost AC-7 bounds. Errors carries what the platform reported about the watch
// itself (a queue overflow, a watch limit reached), never a per-file error.
type watchBackend interface {
	Add(dir string) error
	Events() <-chan watchEvent
	Errors() <-chan error
	Close() error
}

// fsnotifyBackend adapts fsnotify to the interface above. The translation runs on its own
// goroutine so that a slow consumer cannot back up into the platform's own event queue,
// and it stops on Close rather than on a full channel: an event dropped silently here
// would be indistinguishable from a file nobody touched.
type fsnotifyBackend struct {
	w      *fsnotify.Watcher
	events chan watchEvent
	done   chan struct{}
}

// watchEventBuffer is how many translated events may wait for the watch's own loop. It is
// a buffer and never a guarantee: events are lossy by construction (the platform's queue
// overflows, a restart misses everything), which is why the periodic scan remains the
// source of truth.
const watchEventBuffer = 1024

// newFSNotifyBackend obtains a watcher from the platform. The error it returns is the
// whole of "this build has no filesystem-event backend here", and it is reported as the
// dependency that failed rather than swallowed.
func newFSNotifyBackend() (watchBackend, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	b := &fsnotifyBackend{w: w, events: make(chan watchEvent, watchEventBuffer), done: make(chan struct{})}
	go b.translate()
	return b, nil
}

// translate maps the platform's events onto the two facts above until Close is called.
func (b *fsnotifyBackend) translate() {
	defer close(b.events)
	for ev := range b.w.Events {
		e := watchEvent{Path: ev.Name, Gone: ev.Has(fsnotify.Remove) || ev.Has(fsnotify.Rename)}
		select {
		case b.events <- e:
		case <-b.done:
			return
		}
	}
}

func (b *fsnotifyBackend) Add(dir string) error { return b.w.Add(dir) }

func (b *fsnotifyBackend) Events() <-chan watchEvent { return b.events }

func (b *fsnotifyBackend) Errors() <-chan error { return b.w.Errors }

// Close releases every descriptor this backend holds. The translation goroutine is
// released FIRST so it cannot be parked on a send nobody will receive.
func (b *fsnotifyBackend) Close() error {
	select {
	case <-b.done:
	default:
		close(b.done)
	}
	return b.w.Close()
}

// watchMechanism names the event mechanism fsnotify's backend uses on a platform, and ""
// where this build carries none. The table is fsnotify's own supported-platform table:
// inotify on Linux, kqueue on the BSDs and macOS, ReadDirectoryChangesW on Windows, FEN
// on illumos. There is no polling backend to degrade into, which is what makes "the
// mechanism it actually obtained" a name rather than a guess.
func watchMechanism(goos string) string {
	switch goos {
	case "linux":
		return "inotify"
	case "darwin", "dragonfly", "freebsd", "netbsd", "openbsd":
		return "kqueue"
	case "windows":
		return "ReadDirectoryChangesW"
	case "illumos", "solaris":
		return "FEN"
	}
	return ""
}

// thisMechanism is the mechanism name for the platform this binary was built for.
func thisMechanism() string { return watchMechanism(runtime.GOOS) }
