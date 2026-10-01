package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/health"
	"github.com/NSchatz/holdfast/internal/secret"
)

// TestHealthEndpoint_ReportsASweepThroughTheReadAPI runs a real sweep (the pinned ffmpeg)
// over a synthetic library - two good clips, a truncated copy and a text file under a video
// name - and reads GET /api/health. The body is printed: it is the read-API sample the
// goal report and docs/api-reference.md carry.
func TestHealthEndpoint_ReportsASweepThroughTheReadAPI(t *testing.T) {
	bin := os.Getenv("HOLDFAST_FFMPEG")
	if bin == "" {
		bin = "ffmpeg"
	}
	if _, err := exec.LookPath(bin); err != nil {
		t.Fatalf("::error:: %q not found - this fixture requires the pinned ffmpeg (set HOLDFAST_FFMPEG): %v", bin, err)
	}
	lib := filepath.Join(t.TempDir(), "media")
	if err := os.MkdirAll(filepath.Join(lib, "Show"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Show/episode-01.mkv", "Show/episode-02.mkv"} {
		if out, err := exec.Command(bin, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
			"-i", "testsrc2=duration=1:size=160x120:rate=24", "-c:v", "libx264", "-preset", "ultrafast",
			"-pix_fmt", "yuv420p", filepath.Join(lib, name)).CombinedOutput(); err != nil {
			t.Fatalf("ffmpeg: %v\n%s", err, out)
		}
	}
	whole, err := os.ReadFile(filepath.Join(lib, "Show/episode-02.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lib, "Show/episode-03.mkv"), whole[:len(whole)/2], 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lib, "readme.mkv"), []byte("not a video\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	st := newStore(t)
	sw := health.New(168*time.Hour, 1, st, func(stop func() bool, offer func(string) bool) {
		_ = filepath.WalkDir(lib, func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && !stop() && !offer(p) {
				return fs.SkipAll
			}
			return nil
		})
	}, health.FFmpeg{Bin: bin}, discard())
	ctx := context.Background()
	if ran, err := sw.RunDue(ctx); err != nil || !ran {
		t.Fatalf("RunDue: ran=%v err=%v", ran, err)
	}

	ctrl := NewController(ctx, func(context.Context) error { return nil }, discard())
	hub := NewHub(st, ctrl, discard())
	cfg := config.Config{HealthSweepIntervalHours: 168}
	srv := New(ctx, cfg, secret.Value{}, secret.NewValue("read-secret"), st, ctrl, hub, nil, discard())
	srv.SetHealth(sw)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// The read gate covers it like every other read.
	resp, err := http.Get(ts.URL + "/api/health")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET /api/health without the read token: %d, want 401", resp.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/health", nil)
	req.Header.Set("Authorization", "Bearer read-secret")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/health: %d %s", resp.StatusCode, body)
	}
	var pretty bytes.Buffer
	_ = json.Indent(&pretty, body, "", "  ")
	t.Logf("GET /api/health after one sweep:\n%s", pretty.String())

	var rep health.Report
	if err := json.Unmarshal(body, &rep); err != nil {
		t.Fatal(err)
	}
	lc := rep.LastCompleted
	if !rep.Enabled || rep.IntervalHours != 168 || rep.State != "idle" || rep.Current != nil || lc == nil ||
		rep.NextDueAt == nil || lc.Checked != 4 || lc.OK != 2 || lc.Corrupt != 2 || lc.Unreadable != 0 ||
		len(lc.Problems) != 2 || lc.Problems[0].Path != filepath.Join(lib, "Show/episode-03.mkv") ||
		lc.Problems[0].Reason == "" || lc.Problems[1].Path != filepath.Join(lib, "readme.mkv") {
		t.Errorf("the report does not describe the sweep that ran: %+v", rep)
	}
}

// TestHealthEndpoint_ADaemonWithNoSweepSaysOff: the route answers with the sweep off and
// nothing recorded as null.
func TestHealthEndpoint_ADaemonWithNoSweepSaysOff(t *testing.T) {
	h := newHarness(t, "")
	ts := httptest.NewServer(h.srv)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/api/health")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	want := `{"enabled":false,"interval_hours":0,"state":"off","next_due_at":null,"current":null,"last_completed":null}` + "\n"
	if resp.StatusCode != http.StatusOK || string(body) != want {
		t.Errorf("GET /api/health: %d %q, want 200 %q", resp.StatusCode, body, want)
	}
}
