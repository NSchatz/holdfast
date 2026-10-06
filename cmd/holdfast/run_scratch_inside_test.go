package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// A scratch_dir inside a library root that the root's exclude_paths prune
// (docs/scratch.md#inside-a-root), through the whole of `holdfast run`: the startup walk
// accepts it and prunes it, the encode works there and swaps beside the source, a video in
// the working directory is never offered, and the scratch sweep clears an orphan there.
func TestRun_AScratchDirInsideAnExcludedDirectoryOfTheRoot(t *testing.T) {
	ffmpeg := envOr("HOLDFAST_FFMPEG", "ffmpeg")
	if _, err := exec.LookPath(ffmpeg); err != nil {
		t.Fatalf("::error:: ffmpeg is required here: a run that never ran proves nothing: %v", err)
	}
	dir := t.TempDir()
	lib := filepath.Join(dir, "disk1")
	work := filepath.Join(lib, ".holdfast", "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(lib, "movies"), 0o755); err != nil {
		t.Fatal(err)
	}
	film := h264Fixture(t, ffmpeg, filepath.Join(lib, "movies", "film.mkv"))
	decoy := h264Fixture(t, ffmpeg, filepath.Join(work, "decoy.mkv"))
	orphan := filepath.Join(work, "gone.0123456789ab.__transcoding__.mkv")
	if err := os.WriteFile(orphan, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.yaml")
	body := "exclude_paths: [\"**/.holdfast/**\"]\n" +
		"library_roots:\n  - path: " + lib + "\n    scratch_dir: " + work + "\n" +
		"state_dir: " + filepath.Join(dir, "state") + "\nscratch_min_free_gb: 0\n" +
		"min_bitrate_kbps: 0\nvmaf_enable: false\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	if code := dispatch([]string{"run", "--config", cfgPath}, &out, &errOut); code != exitOK {
		t.Fatalf("run exited %d, want %d (stderr: %s)", code, exitOK, errOut.String())
	}
	if codec := videoCodec(t, film); codec == "h264" {
		t.Errorf("the film was not re-encoded: %s is still %s", film, codec)
	}
	if codec := videoCodec(t, decoy); codec != "h264" {
		t.Errorf("a video inside the pruned working directory was processed: %s is now %s", decoy, codec)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Errorf("the scratch sweep left the orphaned working file %s (stat err %v)", orphan, err)
	}
	ents, err := os.ReadDir(filepath.Dir(film))
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || ents[0].Name() != "film.mkv" {
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Errorf("the film's directory holds %v after the run, want just film.mkv", names)
	}
}
