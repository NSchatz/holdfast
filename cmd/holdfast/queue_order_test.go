package main

// Where the resolved QUEUE ORDER is STATED (S0095): the startup records of both daemons,
// and `validate`'s stdout.
//
// An operator watching a queue cannot tell a configured order from an accident of the
// traversal by looking at what is running, so the value has to be said out loud in the two
// places they look - before the pass starts, and before they point the tool at a library at
// all. Each case names the criterion it grades (testing T1).

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/logging"
)

// emptyLibraryConfig writes a configuration over an EMPTY library root and returns its
// path. Empty on purpose: these cases grade what the daemon SAYS at startup, so the fixture
// deliberately gives it nothing to encode.
func emptyLibraryConfig(t *testing.T, extra string) string {
	t.Helper()
	dir := t.TempDir()
	lib := filepath.Join(dir, "media")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.yaml")
	body := "library_roots:\n  - " + lib + "\nstate_dir: " + filepath.Join(dir, "state") +
		"\nvmaf_enable: false\n" + extra
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath
}

// lockedBuffer is a log sink the daemon's goroutines may write to while the case reads it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestRun_StatesTheResolvedQueueOrderAtStartup is [AC-7] for `holdfast run`. The value is a
// FIELD of a structured record rather than a sentence in one (observability O1), so a
// reader joining a pass to its plan can select on it.
func TestRun_StatesTheResolvedQueueOrderAtStartup(t *testing.T) {
	for _, tc := range []struct{ name, extra, want string }{
		{"an absent key states the resolved default", "", config.QueueOrderPath},
		{"a configured order states itself", "queue_order: largest\n", config.QueueOrderLargest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfgPath := emptyLibraryConfig(t, tc.extra)
			code := -1
			got := captureStderr(t, func() {
				var out, errOut bytes.Buffer
				code = dispatch([]string{"run", "--config", cfgPath}, &out, &errOut)
			})
			if code != 0 {
				t.Fatalf("run exited %d:\n%s", code, got)
			}
			if !strings.Contains(got, "queue_order="+tc.want) {
				t.Errorf("the startup records carry no queue_order=%s field:\n%s", tc.want, got)
			}
		})
	}
}

// TestServe_StatesTheResolvedQueueOrderAtStartup is [AC-7] for `holdfast serve`. The daemon
// that runs for weeks is the one an operator is most likely to be reading the records of,
// so the same field is on its startup record too.
func TestServe_StatesTheResolvedQueueOrderAtStartup(t *testing.T) {
	// Grab a free localhost port, then release it for the server to bind.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	cfgPath := emptyLibraryConfig(t, "queue_order: newest\nserver_addr: "+addr+"\n")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	logs := &lockedBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- runServer(ctx, cfg, logging.To(logs, "info"), &bytes.Buffer{}) }()

	waitHTTP(t, "http://"+addr+"/api/summary", 10*time.Second)
	if !strings.Contains(logs.String(), "queue_order="+config.QueueOrderNewest) {
		t.Errorf("serve's startup records carry no queue_order=%s field:\n%s",
			config.QueueOrderNewest, logs.String())
	}

	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("runServer exited %d", code)
		}
	case <-time.After(12 * time.Second):
		t.Fatal("runServer did not shut down after the context was cancelled")
	}
}

// TestValidate_PrintsTheResolvedQueueOrder is [AC-8]. `validate` is where an operator
// checks a configuration BEFORE pointing it at a library, so it prints the resolved value
// on STDOUT (cli L2 - it is the report the command exists to print, not narration), says
// what the value MEANS, and exits zero.
func TestValidate_PrintsTheResolvedQueueOrder(t *testing.T) {
	for _, tc := range []struct{ name, extra, want string }{
		{"an absent key prints the resolved default", "", config.QueueOrderPath},
		{"a configured order prints itself", "queue_order: smallest\n", config.QueueOrderSmallest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfgPath := emptyLibraryConfig(t, tc.extra)
			var out, errOut bytes.Buffer
			if code := dispatch([]string{"validate", "--config", cfgPath}, &out, &errOut); code != 0 {
				t.Fatalf("validate exited nonzero: %s", errOut.String())
			}
			line := queueOrderLine(t, out.String())
			if !strings.Contains(line, tc.want) {
				t.Errorf("validate's queue-order line does not name %q:\n%s", tc.want, line)
			}
			// It says what the value IS ABOUT. A bare token is a value an operator has to
			// go and look up, which is the report not being a report.
			for _, phrase := range []string{"order", "offers", "workers"} {
				if !strings.Contains(line, phrase) {
					t.Errorf("validate's queue-order line never says %q, so it names a value without "+
						"saying it is the order files are offered in:\n%s", phrase, line)
				}
			}
		})
	}
}

// queueOrderLine returns the one stdout line naming the queue order, failing the case when
// there is none. It reads STDOUT and nothing else: a report on stderr is narration a caller
// parsing this command would never see.
func queueOrderLine(t *testing.T, stdout string) string {
	t.Helper()
	for _, line := range strings.Split(stdout, "\n") {
		if strings.Contains(line, "queue order") {
			return line
		}
	}
	t.Fatalf("no line of validate's stdout names the queue order:\n%s", stdout)
	return ""
}

// TestValidate_TheShippedExampleDeclaresTheQueueOrderAndPasses is [AC-12]. Both halves are
// graded here and neither is graded by `make check` alone: the documentation gate enforces
// four fixed anchors and says nothing about configuration-key coverage, so the only thing
// that can tell an operator's copied example from one that never mentioned the key is a
// case that reads the file out of the tree.
func TestValidate_TheShippedExampleDeclaresTheQueueOrderAndPasses(t *testing.T) {
	path := filepath.Join("..", "..", "config.example.yaml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the shipped example: %v", err)
	}
	text := string(body)

	// The key, with its DEFAULT value beside it: an example that showed a non-default
	// would be an example whose behaviour differs from the file an operator copies it into
	// and then edits.
	if !strings.Contains(text, "queue_order: "+config.QueueOrderPath) {
		t.Errorf("config.example.yaml does not declare %q with its default value %q",
			"queue_order", config.QueueOrderPath)
	}
	// And every value it accepts, so the refusal is not the first place an operator learns
	// the spelling.
	for _, order := range config.QueueOrders {
		if !strings.Contains(text, order) {
			t.Errorf("config.example.yaml never names the accepted value %q beside the key", order)
		}
	}

	var out, errOut bytes.Buffer
	if code := dispatch([]string{"validate", "--config", path}, &out, &errOut); code != 0 {
		t.Fatalf("`holdfast validate` over the SHIPPED example exited %d: %s", code, errOut.String())
	}
	if line := queueOrderLine(t, out.String()); !strings.Contains(line, config.QueueOrderPath) {
		t.Errorf("validate over the shipped example does not report the default order:\n%s", line)
	}
}
