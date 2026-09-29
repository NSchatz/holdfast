package startup

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
)

// startupRecords is every structured record Result.Log wrote.
func startupRecords(t *testing.T, res Result) []map[string]any {
	t.Helper()
	var buf bytes.Buffer
	res.Log(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("a log line is not one JSON object (%v): %s", err, line)
		}
		out = append(out, rec)
	}
	return out
}

// TestS0168AC11_TheStartupReportNeverWarnsAboutAPrunedDirectory is S0168 [AC-11] at the
// report that names pruned directories: the walk's own log. Each pruned directory is one
// record at debug, the count of them one record at info, a second spelling of pruned storage
// one record at info - and no warn record names a pruned directory or anything beneath it,
// while a directory the filters do NOT cover that cannot be read still warns (observability
// O3: warn is for a degraded state, and an operator's own filter doing what it says is not
// one). A walk that pruned nothing writes no count at all.
func TestS0168AC11_TheStartupReportNeverWarnsAboutAPrunedDirectory(t *testing.T) {
	build := func() *fakeFS {
		f := newFS().setType("/", "ext4")
		f.mkfile("/srv/media/Film.mkv")
		f.mkfile("/srv/media/.Trash-0/Old.mkv")
		f.failRead("/srv/media/.Trash-0", fs.ErrPermission)
		f.mkfile("/srv/media/film/Extras/Behind.mkv")
		f.mkfile("/srv/media/tv/Extras/Behind.mkv")
		f.bind("/srv/media/zz", "/srv/media/.Trash-0")
		f.mkfile("/srv/media/locked/Hidden.mkv")
		f.failRead("/srv/media/locked", fs.ErrPermission)
		f.mkdir("/var/state")
		return f
	}
	pruned := []string{"/srv/media/.Trash-0", "/srv/media/film/Extras", "/srv/media/tv/Extras"}
	res := pruningCheck(build(), config.Config{LibraryRoots: []string{"/srv/media"},
		ExcludePaths: []string{"**/.Trash-*/**", "**/Extras"}})

	debugs, counted, alias, lockedWarned := map[string]bool{}, false, false, false
	for _, rec := range startupRecords(t, res) {
		level, msg := fmt.Sprint(rec["level"]), fmt.Sprint(rec["msg"])
		path, detail := fmt.Sprint(rec["path"]), fmt.Sprint(rec["detail"])
		for _, dir := range pruned {
			if level == "WARN" && (strings.Contains(path, dir) || strings.Contains(detail, dir)) {
				t.Errorf("a warn record names the pruned %s: %v", dir, rec)
			}
		}
		switch {
		case level == "DEBUG" && rec["path"] != nil && strings.Contains(msg, "path filter"):
			debugs[path] = true
		case level == "INFO" && rec["directories_excluded_by_a_path_filter"] != nil:
			if rec["directories_excluded_by_a_path_filter"] != float64(len(pruned)) {
				t.Errorf("the count record says %v, want %d", rec["directories_excluded_by_a_path_filter"], len(pruned))
			}
			counted = true
		case level == "INFO" && path == "/srv/media/zz":
			alias = true
		case level == "WARN" && path == "/srv/media/locked":
			lockedWarned = true
		}
	}
	for _, dir := range pruned {
		if !debugs[dir] {
			t.Errorf("the pruned %s has no debug record of its own: %v", dir, debugs)
		}
	}
	if !counted {
		t.Error("the walk wrote no count of the directories it pruned")
	}
	if !alias {
		t.Error("the second spelling of pruned storage was not reported at info")
	}
	if !lockedWarned {
		t.Error("a directory no filter covers that cannot be read no longer warns")
	}

	// A walk with no filter prunes nothing and writes no count.
	for _, rec := range startupRecords(t, check(build(), []string{"/srv/media"}, "/var/state")) {
		if rec["directories_excluded_by_a_path_filter"] != nil {
			t.Fatalf("a walk that pruned nothing wrote a count: %v", rec)
		}
	}
}
