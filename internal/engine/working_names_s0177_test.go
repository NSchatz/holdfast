package engine

// S0177: the working file beside a source ends in TempSuffix and the retained original in
// UndoSuffix, so neither is a playable-looking film in a library folder, and every rule
// that recognises, protects, sweeps, restores or releases one of them still works on the
// names earlier builds left on disk.
//
// Graded here against real ffmpeg, real files and the real pipeline, like the rest of this
// package's safety proofs. The criteria graded through the `holdfast restore` command
// (AC-7, AC-8, AC-10) and through the census (AC-16) are in cmd/holdfast.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
)

// ---- helpers ----------------------------------------------------------------

// s0177Names is dir's entry names, sorted. It reports rather than fails, so it can be
// called from an engine seam, which runs on a worker goroutine.
func s0177Names(dir string) ([]string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(ents))
	for _, e := range ents {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out, nil
}

// s0177VideoNamed is every name that ends in one of exts, which is what a media server
// scanning a folder by extension picks up. It is written out rather than borrowed from the
// engine's own rule, because it is the observer of that rule.
func s0177VideoNamed(names, exts []string) []string {
	var out []string
	for _, n := range names {
		for _, ext := range exts {
			if strings.HasSuffix(strings.ToLower(n), "."+strings.ToLower(ext)) {
				out = append(out, n)
				break
			}
		}
	}
	return out
}

// s0177Suffixed is every name ending in suffix.
func s0177Suffixed(names []string, suffix string) []string {
	var out []string
	for _, n := range names {
		if strings.HasSuffix(n, suffix) {
			out = append(out, n)
		}
	}
	return out
}

// s0177FormatName is ffprobe's format_name for path: the demuxer that reads it back.
func s0177FormatName(ffprobe, path string) (string, error) {
	out, err := exec.Command(ffprobe, "-v", "error", "-show_entries", "format=format_name",
		"-of", "default=nw=1:nk=1", "--", path).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("ffprobe %s: %v: %s", path, err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

// s0177TSPacket is the transport-stream packet size a file is written in: 192 for m2ts
// mode (BDAV, a 4-byte timestamp ahead of each 188-byte packet), 188 for a plain stream,
// 0 for neither. format_name reads both as "mpegts", so this is the only way to tell them
// apart.
func s0177TSPacket(b []byte) int {
	aligned := func(stride, at int) bool {
		return len(b) >= 2*stride && len(b)%stride == 0 && b[at] == 0x47 && b[stride+at] == 0x47
	}
	switch {
	case aligned(192, 4):
		return 192
	case aligned(188, 0):
		return 188
	}
	return 0
}

// s0177CopyFile copies from to to.
func s0177CopyFile(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// s0177DirectArgv is a job's own encode argv as ffmpeg runs it to write out BY ITS OWN
// CHOICE of container: the same arguments, reading in, with the container this build
// names (the -f and the option m2ts adds) taken out, so the muxer is the one ffmpeg
// selects from out's name.
func s0177DirectArgv(jobArgv []string, in, out string) []string {
	var args []string
	for i := 0; i < len(jobArgv); i++ {
		switch jobArgv[i] {
		case "-f", "-mpegts_m2ts_mode":
			i++
			continue
		case "-i":
			args = append(args, "-i", in)
			i++
			continue
		}
		args = append(args, jobArgv[i])
	}
	args[len(args)-1] = out
	return args
}

// s0177Seen is what one job handed its encoder and what the encode left.
type s0177Seen struct {
	in, out string
	err     error  // what the encode returned
	format  string // the working file's format_name, where the encode succeeded
	bytes   []byte // the working file's bytes, where the encode succeeded
	probe   error  // why format could not be read
}

// s0177Recorder collects s0177Seen across the jobs of a run.
type s0177Recorder struct {
	ffprobe string
	mu      sync.Mutex
	seen    []s0177Seen
}

func (r *s0177Recorder) add(s s0177Seen) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, s)
}

func (r *s0177Recorder) forSource(t *testing.T, in string) s0177Seen {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.seen {
		if s.in == in {
			return s
		}
	}
	t.Fatalf("the encoder was never handed %s (it was handed %+v)", in, r.seen)
	return s0177Seen{}
}

func (r *s0177Recorder) inputs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, s := range r.seen {
		out = append(out, s.in)
	}
	return out
}

// s0177Observed is the production encoder with the working file it wrote inspected the
// moment the encode returns, before any gate has run. It forwards the profile and the
// stream plan, so the argv is the job's own and not the plan-less one a bare wrapper gets.
type s0177Observed struct {
	inner FFmpegEncoder
	rec   *s0177Recorder
}

func (o s0177Observed) ForProfile(prof config.Profile) Encoder {
	o.inner = o.inner.ForProfile(prof).(FFmpegEncoder)
	return o
}

func (o s0177Observed) ForStreamPlan(plan *StreamPlan) Encoder {
	o.inner = o.inner.ForStreamPlan(plan).(FFmpegEncoder)
	return o
}

func (o s0177Observed) Encode(ctx context.Context, in, out string, props *probe.VideoProps) error {
	err := o.inner.Encode(ctx, in, out, props)
	s := s0177Seen{in: in, out: out, err: err}
	if err == nil {
		s.format, s.probe = s0177FormatName(o.rec.ffprobe, out)
		s.bytes, _ = os.ReadFile(out)
	}
	o.rec.add(s)
	return err
}

// s0177Observe builds the production encoder for cfg, observed by a fresh recorder, and an
// argv log beside it.
func s0177Observe(ffmpeg, ffprobe string, cfg config.Config) (s0177Observed, *s0177Recorder, *argvLog) {
	rec := &s0177Recorder{ffprobe: ffprobe}
	argv := newArgvLog()
	enc := FFmpegEncoder{FFmpeg: ffmpeg, Cfg: cfg, Probe: probe.New(ffmpeg, ffprobe), argvObserver: argv.record}
	return s0177Observed{inner: enc, rec: rec}, rec, argv
}

// s0177ListingFFmpeg writes an ffmpeg stand-in that runs the real binary and, for the one
// invocation whose output is a working file beside a source (its last argument ends in
// TempSuffix), lists that output's directory WHILE THE ENCODER PROCESS IS RUNNING: once
// the working file has bytes in it the encoder is stopped, the directory is listed into
// listing, and the encoder is continued. An encode that ends before its working file could
// be seen leaves no listing, and the case fails rather than passing on nothing.
func s0177ListingFFmpeg(t *testing.T, real, listing string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "listing-ffmpeg.sh")
	script := "#!/bin/sh\n" +
		"for a in \"$@\"; do out=\"$a\"; done\n" +
		"case \"$out\" in *" + TempSuffix + ") ;; *) exec \"" + real + "\" \"$@\" ;; esac\n" +
		"\"" + real + "\" \"$@\" &\n" +
		"pid=$!\n" +
		"while kill -0 \"$pid\" 2>/dev/null; do\n" +
		"  if [ -s \"$out\" ]; then\n" +
		"    kill -STOP \"$pid\"\n" +
		"    ls -1A \"$(dirname -- \"$out\")\" > \"" + listing + ".part\"\n" +
		"    mv \"" + listing + ".part\" \"" + listing + "\"\n" +
		"    kill -CONT \"$pid\"\n" +
		"    break\n" +
		"  fi\n" +
		"  sleep 0.005\n" +
		"done\n" +
		"wait \"$pid\"\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write the listing ffmpeg: %v", err)
	}
	return path
}

// s0177Listing reads a listing s0177ListingFFmpeg took.
func s0177Listing(t *testing.T, listing string) []string {
	t.Helper()
	b, err := os.ReadFile(listing)
	if err != nil {
		t.Fatalf("no listing was taken while the encoder ran (the encode ended before its working file "+
			"could be seen, or never started): %v", err)
	}
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	sort.Strings(out)
	return out
}

// s0177Hevc writes an HEVC clip of the given length at a small size: a source already at
// the target codec, so a pass has nothing of its own to encode.
func s0177Hevc(t *testing.T, ffmpeg, path, seconds string) {
	t.Helper()
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration="+seconds+":size=160x120:rate=10",
		"-c:v", "libx265", "-preset", "ultrafast", "-x265-params", "log-level=error",
		"-pix_fmt", "yuv420p", "-f", "matroska", "--", path)
}

// s0177FailingFFmpeg writes an ffmpeg stand-in that runs the real binary to completion and
// then exits 1 for the invocation whose output is a working file beside a source, recording
// the size of what it left there into left. Every other invocation is the real binary.
func s0177FailingFFmpeg(t *testing.T, real, left string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "failing-ffmpeg.sh")
	script := "#!/bin/sh\n" +
		"for a in \"$@\"; do out=\"$a\"; done\n" +
		"case \"$out\" in *" + TempSuffix + ") ;; *) exec \"" + real + "\" \"$@\" ;; esac\n" +
		"\"" + real + "\" \"$@\"\n" +
		"wc -c < \"$out\" > \"" + left + "\"\n" +
		"exit 1\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write the failing ffmpeg: %v", err)
	}
	return path
}

// ---- AC-1: the working file beside a source ----------------------------------

// TestS0177AC1_TheWorkingFileBesideTheSourceEndsInHoldfastPart grades AC-1: the encode is
// written beside the source under `<stem>.__transcoding__.<ext>.holdfast-part`, where
// <ext> is the OUTPUT container's, and while the encoder is running and after the job
// ends, no file of that job but the source (and, after the swap, the final path) ends in
// a configured video extension.
func TestS0177AC1_TheWorkingFileBesideTheSourceEndsInHoldfastPart(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	for _, tc := range []struct {
		name, src, containerExt, final string
	}{
		{"the output keeps the source's container", "movie.mkv", "source", "movie.mkv"},
		{"container_ext names another container", "movie.mp4", "mkv", "movie.mkv"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := t.TempDir()
			src := filepath.Join(d, tc.src)
			// Ten seconds, so the encode is still running when its working file is listed.
			mkH264Long(t, ffmpeg, src, "8M")
			listing := filepath.Join(t.TempDir(), "listing")
			wrapper := s0177ListingFFmpeg(t, ffmpeg, listing)
			exts := baseCfg(d).VideoExts

			ts := run(t, wrapper, ffprobe, d, nil, func(c *config.Config) { c.ContainerExt = tc.containerExt })

			during := s0177Listing(t, listing)
			work := "movie." + TempMarker + ".mkv" + TempSuffix
			if !slices.Contains(during, work) {
				t.Fatalf("while the encoder ran the directory held %v, and no working file named %s", during, work)
			}
			if got := s0177VideoNamed(during, exts); !equalStrings(got, []string{tc.src}) {
				t.Errorf("while the encoder ran, %v ended in a configured video extension; want only the source %s",
					got, tc.src)
			}
			if !ledgerHas(t, ts, store.Done, tc.final) {
				t.Fatalf("the job did not swap, so the listing after it proves nothing: %v", lsDir(t, d))
			}
			after, err := s0177Names(d)
			if err != nil {
				t.Fatal(err)
			}
			if !equalStrings(after, []string{tc.final}) {
				t.Errorf("after the job the directory holds %v, want only the final path %s", after, tc.final)
			}
		})
	}
}

// ---- AC-2: the accepted copy beside the source, with a scratch directory ------

// TestS0177AC2_TheAcceptedCopyBesideTheSourceEndsInHoldfastPart grades AC-2: with
// scratch_dir set, the gates' accepted bytes are placed beside the source under the AC-1
// shape, and at that moment - before the swap - and after the job, nothing in the source's
// directory but the source and the final path ends in a configured video extension.
func TestS0177AC2_TheAcceptedCopyBesideTheSourceEndsInHoldfastPart(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root, scratch := scratchDirs(t)
	src := filepath.Join(root, "film.mkv")
	mkH264(t, ffmpeg, src, "8M")
	eng, ts := buildEngineAndStore(t, ffmpeg, ffprobe, root, nil, func(c *config.Config) { c.ScratchDir = scratch })
	exts := eng.Cfg.VideoExts

	var copied string
	var atCopy []string
	var listErr error
	eng.afterCopyBack = func(p string) error {
		copied = p
		atCopy, listErr = s0177Names(root)
		return nil
	}
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}
	if listErr != nil {
		t.Fatal(listErr)
	}
	want := filepath.Join(root, "film."+TempMarker+".mkv"+TempSuffix)
	if copied != want {
		t.Fatalf("the accepted bytes were placed at %q beside the source, want %q", copied, want)
	}
	if !equalStrings(atCopy, []string{"film." + TempMarker + ".mkv" + TempSuffix, "film.mkv"}) {
		t.Errorf("once the copy was placed, before the swap, the source's directory held %v", atCopy)
	}
	if got := s0177VideoNamed(atCopy, exts); !equalStrings(got, []string{"film.mkv"}) {
		t.Errorf("before the swap %v ended in a configured video extension; want only the source", got)
	}
	if !ledgerHas(t, ts, store.Done, "film.mkv") {
		t.Fatalf("the job did not swap: %v", lsDir(t, root))
	}
	if got := listDir(t, root); !equalStrings(got, []string{"film.mkv"}) {
		t.Errorf("after the job the source's directory holds %v, want only the final path", got)
	}
	if got := listDir(t, scratch); len(got) != 0 {
		t.Errorf("the scratch directory holds %v after the job", got)
	}
}

// ---- AC-3: the swapped replacement --------------------------------------------

// TestS0177AC3_TheReplacementIsSwappedToItsOwnNameInItsExtensionsContainer grades AC-3: a
// job that passes every gate swaps to `<stem>.<ext>`, with no suffix, in the container
// ffmpeg itself writes for that extension from the job's own arguments, and leaves no
// `.holdfast-part` file - for an mkv source, an mp4 source, and a container-changing job.
func TestS0177AC3_TheReplacementIsSwappedToItsOwnNameInItsExtensionsContainer(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	for _, tc := range []struct {
		name, src, containerExt, final string
	}{
		{"an mkv source", "movie.mkv", "source", "movie.mkv"},
		{"an mp4 source", "movie.mp4", "source", "movie.mp4"},
		{"container_ext mkv on an mp4 source", "movie.mp4", "mkv", "movie.mkv"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, side := t.TempDir(), t.TempDir()
			src := filepath.Join(d, tc.src)
			mkH264(t, ffmpeg, src, "8M")
			orig := filepath.Join(side, "orig"+filepath.Ext(tc.src))
			s0177CopyFile(t, src, orig)
			mutate := func(c *config.Config) {
				c.ContainerExt = tc.containerExt
				c.VmafEnable = boolPtr(true) // every gate, the perceptual one included
				c.MinVmaf = 95
				c.VmafMinPool = 60
			}
			cfg := baseCfg(d)
			mutate(&cfg)
			enc, _, argv := s0177Observe(ffmpeg, ffprobe, cfg)

			ts := run(t, ffmpeg, ffprobe, d, enc, mutate)

			final := filepath.Join(d, tc.final)
			if !ledgerHas(t, ts, store.Done, tc.final) {
				t.Fatalf("the job did not pass every gate: %q", skipReason(t, ts, tc.final))
			}
			if got := codecOf(t, ffprobe, final); got != "hevc" {
				t.Fatalf("%s is %q after the swap, want the hevc replacement", tc.final, got)
			}
			if got := listDir(t, d); !equalStrings(got, []string{tc.final}) {
				t.Errorf("after the swap the directory holds %v, want only %s", got, tc.final)
			}
			direct := filepath.Join(side, "x"+filepath.Ext(tc.final))
			args := s0177DirectArgv(argv.forSource(t, src), orig, direct)
			if out, err := exec.Command(ffmpeg, args...).CombinedOutput(); err != nil {
				t.Fatalf("ffmpeg could not write %s from the job's own arguments %v: %v\n%s", direct, args, err, out)
			}
			want, err := s0177FormatName(ffprobe, direct)
			if err != nil {
				t.Fatal(err)
			}
			got, err := s0177FormatName(ffprobe, final)
			if err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Errorf("the replacement reads back as %q; ffmpeg writes %q for a name ending %s",
					got, want, filepath.Ext(tc.final))
			}
		})
	}
}

// ---- AC-4: the container, for every default extension ------------------------

// TestS0177AC4_TheWorkingFileIsInTheContainerFFmpegChoosesForItsExtension grades AC-4. For
// each extension of the shipped default video_exts, a job whose output container is that
// extension writes its working file in the container ffmpeg itself writes for `x.<ext>`
// given the job's own encode arguments - and where ffmpeg refuses those arguments for that
// name, the job fails with the source byte-identical and no working file left, rather than
// succeeding in some other container.
//
// The second half proves the whole table the encode names its container from, entry by
// entry, byte for byte: a stream written by ffmpeg's own choice for `x.<ext>` and the same
// stream written with the container this build names for `y.<ext>.holdfast-part` must be
// the same bytes, which is what catches a muxer that reads more than its muxer choice off
// the name (m2ts).
func TestS0177AC4_TheWorkingFileIsInTheContainerFFmpegChoosesForItsExtension(t *testing.T) {
	ffmpeg, ffprobe := tools(t)

	t.Run("each default video extension, through a job's own encode", func(t *testing.T) {
		exts := profileCfg(t, "library_roots:\n  - "+t.TempDir()+"\n").VideoExts
		if !equalStrings(exts, []string{"mkv", "mp4", "avi", "mov", "m4v", "ts", "m2ts", "wmv", "flv"}) {
			t.Fatalf("the shipped default video_exts is %v, not the list this criterion names", exts)
		}
		clip := filepath.Join(t.TempDir(), "clip.mkv")
		ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
			"-i", "testsrc2=duration=1:size=128x96:rate=10",
			"-c:v", "libx264", "-preset", "ultrafast", "-b:v", "4M", "-pix_fmt", "yuv420p", "--", clip)
		failedDirectly := 0
		for _, ext := range exts {
			t.Run(ext, func(t *testing.T) {
				d := t.TempDir()
				src := filepath.Join(d, "clip.mkv")
				s0177CopyFile(t, clip, src)
				before := sha256f(t, src)
				mutate := func(c *config.Config) { c.ContainerExt = ext }
				cfg := baseCfg(d)
				mutate(&cfg)
				enc, rec, argv := s0177Observe(ffmpeg, ffprobe, cfg)

				ts := run(t, ffmpeg, ffprobe, d, enc, mutate)

				seen := rec.forSource(t, src)
				if want := filepath.Join(d, "clip."+TempMarker+"."+ext+TempSuffix); seen.out != want {
					t.Fatalf("the job's working file was %s, want %s", seen.out, want)
				}
				direct := filepath.Join(t.TempDir(), "x."+ext)
				args := s0177DirectArgv(argv.forSource(t, src), clip, direct)
				out, derr := exec.Command(ffmpeg, args...).CombinedOutput()
				if derr != nil {
					failedDirectly++
					// ffmpeg refuses the job's own arguments for x.<ext>: the job must fail, and
					// must not have succeeded in a container of some other extension.
					if seen.err == nil {
						t.Fatalf("ffmpeg refuses x.%s with the job's arguments (%v: %s), but the job's encode "+
							"succeeded - in a container ffmpeg would not have chosen", ext, derr, out)
					}
					if !ledgerHas(t, ts, store.Failed, "clip.mkv") {
						t.Errorf("the job whose encode ffmpeg refuses did not fail")
					}
					if sha256f(t, src) != before {
						t.Error("the source changed under a job that failed")
					}
				} else {
					if seen.err != nil {
						t.Fatalf("ffmpeg writes x.%s with the job's arguments, but the job's encode failed: %v", ext, seen.err)
					}
					if seen.probe != nil {
						t.Fatal(seen.probe)
					}
					want, err := s0177FormatName(ffprobe, direct)
					if err != nil {
						t.Fatal(err)
					}
					if seen.format != want {
						t.Errorf("the working file is %q; ffmpeg writes %q for x.%s", seen.format, want, ext)
					}
					if want == "mpegts" {
						b, err := os.ReadFile(direct)
						if err != nil {
							t.Fatal(err)
						}
						if got, w := s0177TSPacket(seen.bytes), s0177TSPacket(b); got != w || w == 0 {
							t.Errorf("the working file is written in %d-byte packets; ffmpeg writes x.%s in %d-byte ones",
								got, ext, w)
						}
					}
				}
				names := listDir(t, d)
				if left := s0177Suffixed(names, TempSuffix); len(left) != 0 {
					t.Errorf("the job left %v", left)
				}
			})
		}
		// ipod (m4v) and asf (wmv) take no hevc in the pinned ffmpeg; the failing half of this
		// criterion is only graded if at least one extension exercises it.
		if failedDirectly == 0 {
			t.Error("ffmpeg accepted the job's arguments for every default extension, so the failing half of " +
				"this criterion was not exercised")
		}
	})

	t.Run("every extension the container table names, byte for byte", func(t *testing.T) {
		side := t.TempDir()
		// Three short streams between them fit every container in the table.
		sources := []string{
			filepath.Join(side, "h264.mkv"),
			filepath.Join(side, "mpeg4.avi"),
			filepath.Join(side, "vp8.webm"),
		}
		for i, codec := range []string{"libx264", "mpeg4", "libvpx"} {
			ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
				"-i", "testsrc2=duration=0.5:size=64x48:rate=10", "-c:v", codec, "-pix_fmt", "yuv420p", "--", sources[i])
		}
		exts := knownContainerExts()
		exts = append(exts, "MKV", "M2TS") // ffmpeg matches an extension whatever its case, and so does the table
		for _, ext := range exts {
			t.Run(ext, func(t *testing.T) {
				d := t.TempDir()
				proved := false
				for n, source := range sources {
					direct := filepath.Join(d, fmt.Sprintf("x%d.%s", n, ext))
					named := filepath.Join(d, fmt.Sprintf("y%d.%s%s", n, ext, TempSuffix))
					container, err := outputContainerFor(named)
					if err != nil {
						t.Fatal(err)
					}
					base := []string{"-hide_banner", "-nostdin", "-loglevel", "error", "-y", "-i", source,
						"-c", "copy", "-fflags", "+bitexact"}
					_, derr := exec.Command(ffmpeg, append(append([]string(nil), base...), "--", direct)...).CombinedOutput()
					namedArgs := append(append(append([]string(nil), base...), container.args()...), "--", named)
					out, nerr := exec.Command(ffmpeg, namedArgs...).CombinedOutput()
					if (derr == nil) != (nerr == nil) {
						t.Fatalf("from %s ffmpeg's own choice for x.%s exits %v and the named container %v exits %v: %s",
							filepath.Base(source), ext, derr, container.args(), nerr, out)
					}
					if derr != nil {
						continue
					}
					if !bytes.Equal(readAll(t, direct), readAll(t, named)) {
						t.Errorf("from %s, %v wrote different bytes from ffmpeg's own choice for x.%s",
							filepath.Base(source), container.args(), ext)
					}
					proved = true
				}
				if !proved {
					t.Errorf("no stream this case writes fits x.%s, so the entry is unproven", ext)
				}
			})
		}
	})
}

// readAll reads path, failing the test when it cannot.
func readAll(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// ---- AC-5: an extension with no known container -------------------------------

// TestS0177AC5_AnOutputExtensionWithNoKnownContainerFailsTheJobNamingIt grades AC-5: a job
// whose output extension - from video_exts with container_ext: source, or from
// container_ext itself - is one ffmpeg has no muxer for ends failed with a reason naming
// the extension, the source byte-identical with its mtime, and no `.holdfast-part` file.
func TestS0177AC5_AnOutputExtensionWithNoKnownContainerFailsTheJobNamingIt(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	for _, tc := range []struct {
		name, src, ext string
		mutate         func(*config.Config)
	}{
		{"a video_exts entry", "clip.divx", "divx", func(c *config.Config) {
			c.VideoExts = append(c.VideoExts, "divx")
			c.ContainerExt = "source"
		}},
		{"a container_ext", "clip.mkv", "xyz", func(c *config.Config) { c.ContainerExt = "xyz" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The precondition: ffmpeg itself has no muxer for a name ending in this extension.
			if out, err := exec.Command(ffmpeg, "-hide_banner", "-nostdin", "-f", "lavfi", "-i", "nullsrc=d=0.1",
				"--", filepath.Join(t.TempDir(), "x."+tc.ext)).CombinedOutput(); err == nil ||
				!strings.Contains(string(out), "Unable to choose an output format") {
				t.Fatalf("ffmpeg has a muxer for .%s (%v: %s), so this case is not the one AC-5 names", tc.ext, err, out)
			}
			d := t.TempDir()
			src := filepath.Join(d, tc.src)
			mkH264(t, ffmpeg, filepath.Join(d, "clip.mkv"), "8M")
			if tc.src != "clip.mkv" {
				if err := os.Rename(filepath.Join(d, "clip.mkv"), src); err != nil {
					t.Fatal(err)
				}
			}
			before := sha256f(t, src)
			fi, err := os.Stat(src)
			if err != nil {
				t.Fatal(err)
			}

			ts := run(t, ffmpeg, ffprobe, d, nil, tc.mutate)

			out, status, ok := outcomeFor(t, ts, src)
			if !ok || (status != store.Failed && status != store.Skipped) {
				t.Fatalf("the job ended %q (recorded %v), want a failure or a skip", status, ok)
			}
			if !strings.Contains(out.Reason, `"`+tc.ext+`"`) {
				t.Errorf("the recorded reason does not name the extension %q: %s", tc.ext, out.Reason)
			}
			if sha256f(t, src) != before {
				t.Error("the source's bytes changed")
			}
			if after, err := os.Stat(src); err != nil || !after.ModTime().Equal(fi.ModTime()) {
				t.Errorf("the source's mtime changed (%v)", err)
			}
			if got := listDir(t, d); !equalStrings(got, []string{tc.src}) {
				t.Errorf("the job left %v in the source's directory, want only the source", got)
			}
		})
	}
}

// ---- AC-6: an encode that fails or is refused ---------------------------------

// TestS0177AC6_AFailedOrRefusedEncodeLeavesNoHoldfastPartFile grades AC-6: whether the
// encoder fails after writing its working file or a gate refuses what it wrote, no
// `.holdfast-part` file of that job is left beside the source and the source is
// byte-identical.
func TestS0177AC6_AFailedOrRefusedEncodeLeavesNoHoldfastPartFile(t *testing.T) {
	ffmpeg, ffprobe := tools(t)

	t.Run("the encoder fails after writing its working file", func(t *testing.T) {
		d := t.TempDir()
		src := filepath.Join(d, "movie.mkv")
		mkH264(t, ffmpeg, src, "8M")
		before := sha256f(t, src)
		left := filepath.Join(t.TempDir(), "left")
		ts := run(t, s0177FailingFFmpeg(t, ffmpeg, left), ffprobe, d, nil, nil)

		b, err := os.ReadFile(left)
		if err != nil || strings.TrimSpace(string(b)) == "0" {
			t.Fatalf("the failing encoder left no working file to clean up (%v, %q), so this proves nothing", err, b)
		}
		if !ledgerHas(t, ts, store.Failed, "movie.mkv") {
			t.Errorf("the job did not fail: %q", skipReason(t, ts, "movie.mkv"))
		}
		if sha256f(t, src) != before {
			t.Error("the source changed")
		}
		if got := listDir(t, d); !equalStrings(got, []string{"movie.mkv"}) {
			t.Errorf("the failed encode left %v, want only the source", got)
		}
	})

	t.Run("a gate refuses the working file", func(t *testing.T) {
		d := t.TempDir()
		src := filepath.Join(d, "movie.mkv")
		mkH264(t, ffmpeg, src, "300k")
		before := sha256f(t, src)
		var wrote int64
		// A LOSSLESS re-encode of an already small clip: valid, the right codec and length,
		// and larger than its source, so the size gate is the one that refuses it.
		larger := EncoderFunc(func(ctx context.Context, in, out string, _ *probe.VideoProps) error {
			if err := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-i", in,
				"-c:v", "libx265", "-preset", "ultrafast", "-x265-params", "log-level=error:lossless=1",
				"-pix_fmt", "yuv420p", "-f", "matroska", "--", out).Run(); err != nil {
				return err
			}
			wrote = probe.FileSize(out)
			return nil
		})
		ts := run(t, ffmpeg, ffprobe, d, larger, nil)

		if wrote <= 0 {
			t.Fatal("the encoder wrote no working file, so this proves nothing")
		}
		out, status, ok := outcomeFor(t, ts, src)
		if !ok || status != store.Failed || !strings.Contains(out.Reason, "size") {
			t.Errorf("the job ended %q (%s), want a failure the size gate reports", status, out.Reason)
		}
		if sha256f(t, src) != before {
			t.Error("the source changed")
		}
		if got := listDir(t, d); !equalStrings(got, []string{"movie.mkv"}) {
			t.Errorf("the refused encode left %v, want only the source", got)
		}
	})
}

// ---- AC-9: a new-form retention's window closes -------------------------------

// TestS0177AC9_AClosedWindowReleasesANewFormRetentionAndReportsIt grades AC-9: a retention
// under this build's name is removed at the start of the first scan pass after its window
// closes and reported in that pass's release line, and the held-bytes figure carries its
// source bytes while it is held and not after.
func TestS0177AC9_AClosedWindowReleasesANewFormRetentionAndReportsIt(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	ctx := context.Background()
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264(t, ffmpeg, src, "8M")
	size := probe.FileSize(src)
	want := retainedPathFor(src, probe.Fingerprint(src))
	logs := &capturedLog{}
	eng, ts := undoEngineWithLog(t, ffmpeg, ffprobe, d, 24, nil, logs.logger())

	if err := eng.RunOneshot(ctx); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	r := onlyRetained(t, ts)
	if r.RetainedPath != want || !strings.HasSuffix(r.RetainedPath, "."+UndoMarker+".mkv"+UndoSuffix) {
		t.Fatalf("the retention is at %s, want this build's name %s", r.RetainedPath, want)
	}
	if !exists(r.RetainedPath) {
		t.Fatalf("the retained original is not on disk at %s", r.RetainedPath)
	}
	if held, err := ts.HeldByUndoWindow(ctx); err != nil || held != size {
		t.Fatalf("while it is held the undo window holds %d byte(s) (%v), want the source's %d", held, err, size)
	}

	// The window closes, and a new file arrives for the next pass to encode, so where in
	// that pass the release happened is observable.
	late := filepath.Join(d, "late.mkv")
	mkH264(t, ffmpeg, late, "8M")
	eng.undoNow = func() time.Time { return time.Now().Add(25 * time.Hour) }
	if err := eng.RunOneshot(ctx); err != nil {
		t.Fatalf("pass 2: %v", err)
	}

	if exists(r.RetainedPath) {
		t.Errorf("the retained original is still at %s after its window closed", r.RetainedPath)
	}
	// What the window holds now is the retention the late file's swap took in this same
	// pass, and nothing of the released one.
	rows := retainedRows(t, ts)
	if len(rows) != 1 || rows[0].SourcePath != late {
		t.Fatalf("after the release the live retentions are %+v, want only the late file's", rows)
	}
	if held, err := ts.HeldByUndoWindow(ctx); err != nil || held != rows[0].SourceBytes {
		t.Errorf("after the release the undo window holds %d byte(s) (%v), want only the late file's %d",
			held, err, rows[0].SourceBytes)
	}
	released, transcoded := -1, -1
	for i, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, `msg="undo window: released retained original(s)"`) {
			if !hasField(line, "released", "1") || !hasField(line, "bytes_returned", fmt.Sprint(size)) {
				t.Errorf("the release line does not report the release: %s", line)
			}
			released = i
		}
		if strings.Contains(line, "msg=transcode") && hasField(line, "file", late) {
			transcoded = i
		}
	}
	if released < 0 {
		t.Fatalf("no release line was logged:\n%s", logs.String())
	}
	if transcoded < 0 || released > transcoded {
		t.Errorf("the release (line %d) did not come at the start of the pass, before its encode (line %d)",
			released, transcoded)
	}
}

// ---- AC-11: the hardlink guard -------------------------------------------------

// TestS0177AC11_AnUnrecordedLinkAtEitherRetainedNameIsNotReadAsForeign grades AC-11: a
// source whose second link is a same-inode file at this build's retained name or at the
// earlier build's, with no record behind it, is not skipped as hardlinked; a second link
// at any other name still is.
func TestS0177AC11_AnUnrecordedLinkAtEitherRetainedNameIsNotReadAsForeign(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	for _, tc := range []struct {
		name    string
		link    func(src, fingerprint string) string
		foreign bool
	}{
		{"this build's retained name", retainedPathFor, false},
		{"the earlier build's retained name", legacyRetainedPathFor, false},
		{"a retained name for a different fingerprint", func(src, _ string) string {
			return retainedPathFor(src, "1:2")
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := t.TempDir()
			src := filepath.Join(d, "movie.mkv")
			mkH264(t, ffmpeg, src, "8M")
			link := tc.link(src, probe.Fingerprint(src))
			if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(src, link); err != nil {
				t.Fatal(err)
			}
			if n := nlinkOf(t, src); n != 2 {
				t.Fatalf("the fixture has %d links, want 2", n)
			}
			// A dry run decides the file - every guard - and writes nothing.
			cfg := undoCfg(d, 24)
			cfg.DryRun = true
			ts := newTestStore(t, d)
			prober := probe.New(ffmpeg, ffprobe)
			eng := New(cfg, prober, FFmpegEncoder{FFmpeg: ffmpeg, Cfg: cfg, Probe: prober}, ts, discardLogger())
			if err := eng.RunOneshot(context.Background()); err != nil {
				t.Fatalf("RunOneshot: %v", err)
			}

			reason := skipReason(t, ts, "movie.mkv")
			if tc.foreign && reason != SkipHardlinked {
				t.Errorf("a second link at %s was not skipped as hardlinked (reason %q)", filepath.Base(link), reason)
			}
			if !tc.foreign {
				if reason == SkipHardlinked {
					t.Errorf("holdfast's own link at %s was read as a foreign one", filepath.Base(link))
				}
				if !ledgerHas(t, ts, store.WouldTranscode, "movie.mkv") {
					t.Errorf("the source was not decided as one a run would transcode (reason %q)", reason)
				}
			}
			if !sameFile(src, link) {
				t.Errorf("the link at %s is no longer the source's", link)
			}
		})
	}
}

// ---- AC-12: a retention meets the earlier build's link --------------------------

// TestS0177AC12_ARetentionCarriesAnUnrecordedEarlierLinkToTheNewName grades AC-12: a
// retention taken for a source with an unrecorded same-inode link at the earlier build's
// retained name finishes the swap with that original under exactly one retained name, this
// build's, recorded; a DIFFERENT file at the earlier name is left byte-identical and in
// place. A link at the earlier name that a live record names is an earlier build's
// retention: it keeps its name and its expiry, and this retention waits for it.
func TestS0177AC12_ARetentionCarriesAnUnrecordedEarlierLinkToTheNewName(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	ctx := context.Background()

	type fixture struct {
		d, src, earlier, want string
		ino                   uint64
		sum                   string
	}
	setup := func(t *testing.T) fixture {
		d := t.TempDir()
		src := filepath.Join(d, "movie.mkv")
		mkH264(t, ffmpeg, src, "8M")
		fp := probe.Fingerprint(src)
		earlier := legacyRetainedPathFor(src, fp)
		if err := os.MkdirAll(filepath.Dir(earlier), 0o755); err != nil {
			t.Fatal(err)
		}
		return fixture{d: d, src: src, earlier: earlier, want: retainedPathFor(src, fp),
			ino: inodeOf(t, src), sum: sha256f(t, src)}
	}

	t.Run("an unrecorded link at the earlier name ends as the one retained name, this build's", func(t *testing.T) {
		f := setup(t)
		if err := os.Link(f.src, f.earlier); err != nil {
			t.Fatal(err)
		}
		eng, ts := undoEngine(t, ffmpeg, ffprobe, f.d, 24, nil)
		if err := eng.RunOneshot(ctx); err != nil {
			t.Fatalf("RunOneshot: %v", err)
		}
		if got := codecOf(t, ffprobe, f.src); got != "hevc" {
			t.Fatalf("the swap did not happen (%s is %q): %q", f.src, got, skipReason(t, ts, "movie.mkv"))
		}
		if got := listDir(t, filepath.Dir(f.want)); !equalStrings(got, []string{filepath.Base(f.want)}) {
			t.Errorf("the retention area holds %v, want only %s", got, filepath.Base(f.want))
		}
		if inodeOf(t, f.want) != f.ino || sha256f(t, f.want) != f.sum {
			t.Error("the file at this build's retained name is not the pre-swap original")
		}
		if r := onlyRetained(t, ts); r.RetainedPath != f.want {
			t.Errorf("the record names %s, want %s", r.RetainedPath, f.want)
		}
	})

	t.Run("a different file at the earlier name is left as it is", func(t *testing.T) {
		f := setup(t)
		if err := os.WriteFile(f.earlier, []byte("somebody else's bytes"), 0o644); err != nil {
			t.Fatal(err)
		}
		otherIno, otherSum := inodeOf(t, f.earlier), sha256f(t, f.earlier)
		eng, ts := undoEngine(t, ffmpeg, ffprobe, f.d, 24, nil)
		if err := eng.RunOneshot(ctx); err != nil {
			t.Fatalf("RunOneshot: %v", err)
		}
		if got := codecOf(t, ffprobe, f.src); got != "hevc" {
			t.Fatalf("the swap did not happen (%s is %q): %q", f.src, got, skipReason(t, ts, "movie.mkv"))
		}
		if !exists(f.earlier) || inodeOf(t, f.earlier) != otherIno || sha256f(t, f.earlier) != otherSum {
			t.Error("the different file at the earlier name was moved, replaced or changed")
		}
		if inodeOf(t, f.want) != f.ino || sha256f(t, f.want) != f.sum {
			t.Error("the original is not retained under this build's name")
		}
		if r := onlyRetained(t, ts); r.RetainedPath != f.want {
			t.Errorf("the record names %s, want %s", r.RetainedPath, f.want)
		}
	})

	t.Run("a recorded link at the earlier name keeps its name and the retention waits", func(t *testing.T) {
		f := setup(t)
		if err := os.Link(f.src, f.earlier); err != nil {
			t.Fatal(err)
		}
		eng, ts := undoEngine(t, ffmpeg, ffprobe, f.d, 24, nil)
		if err := eng.undo().record(ctx, f.src, f.src, f.earlier, probe.FileSize(f.src)); err != nil {
			t.Fatal(err)
		}
		rec := onlyRetained(t, ts)
		if err := eng.RunOneshot(ctx); err != nil {
			t.Fatalf("RunOneshot: %v", err)
		}
		if got := skipReason(t, ts, "movie.mkv"); got != SkipUndoRetentionFailed {
			t.Errorf("the job ended %q, want %q", got, SkipUndoRetentionFailed)
		}
		if sha256f(t, f.src) != f.sum {
			t.Error("the source changed")
		}
		if got := listDir(t, filepath.Dir(f.earlier)); !equalStrings(got, []string{filepath.Base(f.earlier)}) {
			t.Errorf("the retention area holds %v, want only the recorded %s", got, filepath.Base(f.earlier))
		}
		if !sameFile(f.src, f.earlier) {
			t.Error("the recorded retention is no longer the original")
		}
		if r := onlyRetained(t, ts); r.RetainedPath != rec.RetainedPath || r.ExpiresAt != rec.ExpiresAt {
			t.Errorf("the record changed: %+v, was %+v", r, rec)
		}
	})

	// The rest of the states an earlier build's name can be in, taken at the retention
	// itself: no encode is needed to ask what retain does with them.
	t.Run("the retention alone, over the other states of the earlier name", func(t *testing.T) {
		for _, tc := range []struct {
			name    string
			arrange func(t *testing.T, src, earlier, dst string)
			noStore bool
			refused bool
			kept    bool // the earlier name is still there afterwards
		}{
			{"both names already hold the original", func(t *testing.T, src, earlier, dst string) {
				for _, p := range []string{earlier, dst} {
					if err := os.Link(src, p); err != nil {
						t.Fatal(err)
					}
				}
			}, false, false, false},
			{"a symbolic link at the earlier name", func(t *testing.T, src, earlier, _ string) {
				if err := os.Symlink(src, earlier); err != nil {
					t.Fatal(err)
				}
			}, false, false, true},
			{"no ledger to ask whether a record names the link", func(t *testing.T, src, earlier, _ string) {
				if err := os.Link(src, earlier); err != nil {
					t.Fatal(err)
				}
			}, true, true, true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				d := t.TempDir()
				src := filepath.Join(d, "movie.mkv")
				if err := os.WriteFile(src, []byte("the original's bytes"), 0o644); err != nil {
					t.Fatal(err)
				}
				fp := probe.Fingerprint(src)
				earlier, dst := legacyRetainedPathFor(src, fp), retainedPathFor(src, fp)
				if err := os.MkdirAll(filepath.Dir(earlier), 0o755); err != nil {
					t.Fatal(err)
				}
				tc.arrange(t, src, earlier, dst)
				before, _ := os.Lstat(earlier)
				var st store.Store = newTestStore(t, d)
				if tc.noStore {
					st = nil
				}

				got, err := NewUndoWindow(undoCfg(d, 24), st, discardLogger()).retain(ctx, src, fp)

				if tc.refused {
					if err == nil {
						t.Errorf("the retention was taken (%s) though nothing could say whether a record names %s", got, earlier)
					}
				} else if err != nil || got != dst || !sameRegularFile(src, dst) {
					t.Errorf("retain = %q, %v; want this build's name %s, holding the original", got, err, dst)
				}
				after, lerr := os.Lstat(earlier)
				switch {
				case tc.kept && (lerr != nil || !os.SameFile(before, after) || after.Mode() != before.Mode()):
					t.Errorf("the file at the earlier name was moved, removed or replaced (%v)", lerr)
				case !tc.kept && lerr == nil:
					t.Errorf("the earlier name still holds the original beside this build's: two retained names")
				}
				if !exists(src) || sha256f(t, src) != sha256Of([]byte("the original's bytes")) {
					t.Error("the source changed")
				}
			})
		}
	})
}

// sha256Of is the digest sha256f reports for b.
func sha256Of(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// ---- AC-13: the sweep, both generations ------------------------------------------

// TestS0177AC13_AScanPassSweepsAPartialEncodeAtEitherWorkingName grades AC-13: a scan pass
// removes an orphaned partial encode at this build's working name and one at the earlier
// build's, each with its source beside it, and counts both in the "discarded orphaned temp
// file(s)" line. It holds with owner records kept, and the bounded run's owner-checked
// sweep removes both too when their recorded owners are provably dead.
func TestS0177AC13_AScanPassSweepsAPartialEncodeAtEitherWorkingName(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	for _, bounded := range []bool{false, true} {
		name := "a whole-library pass"
		if bounded {
			name = "a bounded run whose owner records show the owners dead"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			// Sources already at the target codec: the pass has nothing of its own to encode,
			// so what happens to the directory is the sweep's doing alone.
			fresh, older := filepath.Join(root, "new.mkv"), filepath.Join(root, "old.mkv")
			s0177Hevc(t, ffmpeg, fresh, "10")
			s0177Hevc(t, ffmpeg, older, "10")
			freshTemp := tempPath(root, "new", "mkv", 0)
			olderTemp := filepath.Join(root, "old."+TempMarker+".mkv")
			// What a killed run leaves: a real encode of the first two seconds.
			mkHevcFrom(t, ffmpeg, fresh, freshTemp, "2")
			mkHevcFrom(t, ffmpeg, older, olderTemp, "2")
			sums := map[string]string{fresh: md5f(t, fresh), older: md5f(t, older)}

			eng, _ := ownedEngine(t, ffmpeg, ffprobe, root, "ext4")
			var buf bytes.Buffer
			eng.Log = captureLogger(&buf)
			var err error
			if bounded {
				killedOwner(t, eng, freshTemp, fresh)
				killedOwner(t, eng, olderTemp, older)
				err = eng.RunBounded(context.Background(), Bound{Limit: 1})
			} else {
				err = eng.RunOneshot(context.Background())
			}
			if err != nil {
				t.Fatalf("pass: %v", err)
			}

			logged := buf.String()
			for _, p := range []string{freshTemp, olderTemp} {
				if exists(p) {
					t.Errorf("the sweep left the partial encode %s", p)
				}
				if n := removalsOf(logged, p); n != 1 {
					t.Errorf("%d removal(s) of %s were recorded, want 1:\n%s", n, p, logged)
				}
			}
			for p, sum := range sums {
				if md5f(t, p) != sum {
					t.Errorf("the source %s changed", p)
				}
			}
			counted := false
			for _, line := range strings.Split(logged, "\n") {
				if bounded && strings.Contains(line, `msg="bounded run: the owner-checked stale-temp sweep is done"`) {
					counted = hasField(line, "temps_removed", "2")
				}
				if !bounded && strings.Contains(line, `msg="discarded orphaned temp file(s) from a prior run"`) {
					counted = hasField(line, "count", "2")
				}
			}
			if !counted {
				t.Errorf("the sweep's summary line does not count both removals:\n%s", logged)
			}
		})
	}
}

// ---- AC-14: a stranded replacement, both generations -------------------------------

// TestS0177AC14_AStrandedReplacementAtEitherWorkingNameSurvivesTwoPasses grades AC-14: a
// finished, gate-passing encode at this build's working name or at the earlier build's,
// with no record naming it, with and without its source beside it, is not deleted by a
// pass's sweep, not deleted or reused when the next job for that source picks its working
// path, and not enumerated as a source - byte-identical after two consecutive passes.
//
// Owner records are kept, and each stranded file's recorded owner is provably dead: the
// strongest licence to remove a temp either sweep has, which the hold-back must still
// refuse. A partial encode beside a source is swept in the same pass, so "left alone" is
// not a sweep that did nothing.
//
// This is the test that reds when the record-free matcher stops recognising this build's
// working name (splitTempConstruction): such a name reaches strayReplacementHold as not a
// construction name, is answered "", and os.Remove takes the replacement.
func TestS0177AC14_AStrandedReplacementAtEitherWorkingNameSurvivesTwoPasses(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	for _, tc := range []struct {
		name         string
		working      func(dir string) string
		sourceBeside bool
	}{
		{"this build's name, its source beside it", func(dir string) string { return tempPath(dir, "movie", "mkv", 0) }, true},
		{"this build's name, no source beside it", func(dir string) string { return tempPath(dir, "movie", "mkv", 0) }, false},
		{"the earlier build's name, its source beside it", func(dir string) string {
			return filepath.Join(dir, "movie."+TempMarker+".mkv")
		}, true},
		{"the earlier build's name, no source beside it", func(dir string) string {
			return filepath.Join(dir, "movie."+TempMarker+".mkv")
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			root, side := t.TempDir(), t.TempDir()
			dir := filepath.Join(root, "film")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			src := filepath.Join(dir, "movie.mkv")
			orig := filepath.Join(side, "movie.mkv")
			mkH264(t, ffmpeg, orig, "8M")
			stranded := tc.working(dir)
			mkHevcFrom(t, ffmpeg, orig, stranded, "") // the whole source: a finished replacement
			if tc.sourceBeside {
				s0177CopyFile(t, orig, src)
			}
			want := md5f(t, stranded)
			// The control: a partial encode beside a source, which the same sweep must take.
			control := filepath.Join(root, "ctl", "ep.mkv")
			controlTemp := tempPath(filepath.Dir(control), "ep", "mkv", 0)
			if err := os.MkdirAll(filepath.Dir(control), 0o755); err != nil {
				t.Fatal(err)
			}
			s0177Hevc(t, ffmpeg, control, "10")
			mkHevcFrom(t, ffmpeg, control, controlTemp, "2")

			eng, _ := ownedEngine(t, ffmpeg, ffprobe, root, "ext4")
			enc, rec, _ := s0177Observe(ffmpeg, ffprobe, eng.Cfg)
			eng.Enc = enc
			ts := eng.Store.(*testStore)
			killedOwner(t, eng, stranded, src)

			for pass := 1; pass <= 2; pass++ {
				if err := eng.RunOneshot(ctx); err != nil {
					t.Fatalf("pass %d: %v", pass, err)
				}
				if !exists(stranded) {
					t.Fatalf("pass %d deleted the stranded replacement %s", pass, stranded)
				}
				if md5f(t, stranded) != want {
					t.Fatalf("pass %d changed the stranded replacement %s", pass, stranded)
				}
			}
			if exists(controlTemp) {
				t.Errorf("the sweep did not take the partial encode %s, so it did nothing this case can rely on", controlTemp)
			}
			if slices.Contains(rec.inputs(), stranded) {
				t.Errorf("the stranded replacement was handed to the encoder as a source")
			}
			if _, _, found, err := ts.Get(ctx, stranded, probe.Fingerprint(stranded)); err != nil || found {
				t.Errorf("a job row exists for the stranded replacement (found %v, %v)", found, err)
			}
			if tc.sourceBeside {
				seen := rec.forSource(t, src)
				if seen.out == stranded {
					t.Errorf("the next job for the source was handed the stranded replacement's path to write")
				}
				if !ledgerHas(t, ts, store.Done, "movie.mkv") {
					t.Errorf("the source beside it was not encoded around it: %q", skipReason(t, ts, "movie.mkv"))
				}
			}
		})
	}
}

// ---- AC-15: never a source ---------------------------------------------------------

// TestS0177AC15_NewFormNamesAreNeverSourcesWhateverVideoExtsSays grades AC-15: a working
// file and a retained original under this build's names are never enumerated as sources,
// even with `holdfast-part` and `holdfast-undo` configured as video extensions, on both
// enumeration paths; and a targeted submission of the one is refused as a holdfast working
// file and of the other as the retention area.
func TestS0177AC15_NewFormNamesAreNeverSourcesWhateverVideoExtsSays(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	for _, withCoverage := range []bool{false, true} {
		name := "walking the roots directly"
		if withCoverage {
			name = "bounded by the startup walk's coverage (the production path)"
		}
		t.Run(name, func(t *testing.T) {
			d, side := t.TempDir(), t.TempDir()
			orig := filepath.Join(side, "film.mkv")
			mkH264(t, ffmpeg, orig, "8M")
			// Both are real encodes of a film: media by content, and by name only through the
			// two suffixes configured below.
			working := tempPath(d, "film", "mkv", 0)
			mkHevcFrom(t, ffmpeg, orig, working, "")
			retained := retainedPathFor(filepath.Join(d, "film.mkv"), "100:200")
			if err := os.MkdirAll(filepath.Dir(retained), 0o755); err != nil {
				t.Fatal(err)
			}
			s0177CopyFile(t, orig, retained)
			// An ordinary source, so the scan is seen to enumerate something.
			other := filepath.Join(d, "other.mkv")
			mkH264(t, ffmpeg, other, "8M")
			sums := map[string]string{working: md5f(t, working), retained: md5f(t, retained)}

			cfg := baseCfg(d)
			cfg.VideoExts = append(cfg.VideoExts, "holdfast-part", "holdfast-undo")
			for _, p := range []string{working, retained} {
				if !matchesVideoExt(filepath.Base(p), cfg.VideoExts) {
					t.Fatalf("%s does not end in a configured video extension, so this case is not the hazard", p)
				}
				if IsSourceName(filepath.Base(p), cfg.VideoExts) {
					t.Errorf("IsSourceName(%s) = true", filepath.Base(p))
				}
			}
			var mu sync.Mutex
			var handed []string
			spy := EncoderFunc(func(_ context.Context, in, _ string, _ *probe.VideoProps) error {
				mu.Lock()
				handed = append(handed, in)
				mu.Unlock()
				return errFake
			})
			ts := newTestStore(t, d)
			eng := New(cfg, probe.New(ffmpeg, ffprobe), spy, ts, discardLogger())
			if withCoverage {
				eng.Coverage = []string{d, filepath.Dir(retained)}
			}
			if err := eng.RunOneshot(context.Background()); err != nil {
				t.Fatalf("RunOneshot: %v", err)
			}

			if !slices.Contains(handed, other) {
				t.Fatalf("the scan never offered the ordinary source %s (it offered %v)", other, handed)
			}
			for p, sum := range sums {
				if slices.Contains(handed, p) {
					t.Errorf("%s was handed to the encoder as a source", p)
				}
				if _, _, found, err := ts.Get(context.Background(), p, probe.Fingerprint(p)); err != nil || found {
					t.Errorf("a job row exists for %s (found %v, %v)", p, found, err)
				}
				if !exists(p) || md5f(t, p) != sum {
					t.Errorf("%s was removed or changed", p)
				}
			}

			el := eng.Eligibility()
			for p, rule := range map[string]string{working: RuleWorkingFile, retained: RuleRetentionArea} {
				if _, bad := el.Judge(p); bad == nil || bad.Rule != rule {
					t.Errorf("a submission of %s was answered %+v, want rule %q", p, bad, rule)
				}
			}
		})
	}
}
