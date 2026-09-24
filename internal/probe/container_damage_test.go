package probe

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestVideoProps_ContainerDamage grades S0156's [AC-13] and [AC-14] where the attribution is
// made: which of the lines the snapshot probe wrote to stderr are the DEMUXER's, and that a
// probe which did not run to completion reports none. The engine suite grades the same two
// criteria through the guard chain against the pinned ffprobe; this one names each way a line
// can be misattributed, against a scripted ffprobe, so each rule is held by a case of its own.
func TestVideoProps_ContainerDamage(t *testing.T) {
	const (
		mkv       = "codec_name=h264\nformat_name=matroska,webm\n"
		demuxLine = "[matroska,webm @ 0x55d5c3c0a2c0] 0x00 at pos 63542 (0xf836) invalid as first byte of an EBML number"
		demuxMsg  = "0x00 at pos 63542 (0xf836) invalid as first byte of an EBML number"
		decoder   = "[h264 @ 0x55d5c3c0b100] error while decoding MB 15 12"
	)
	long := strings.Repeat("x", 5000)
	cases := []struct {
		name, stdout, stderr, then string
		cancelAfter                time.Duration
		want                       ContainerDiagnostic
	}{
		{name: "the demuxer's lines among a decoder's", stdout: mkv,
			stderr: decoder + "\n" + demuxLine + "\n[matroska,webm @ 0x55d5c3c0a2c0] a second one\n",
			then:   "exit 0",
			want:   ContainerDiagnostic{Demuxer: "matroska,webm", First: demuxMsg, Count: 2}},
		{name: "a decoder's line alone", stdout: mkv, stderr: decoder + "\n", then: "exit 0"},
		{name: "a decoder logging with the demuxer as its parent", stdout: mkv,
			stderr: "[matroska,webm @ 0x55d5c3c0a2c0] " + decoder + "\n", then: "exit 0"},
		{name: "a raw stream whose demuxer and decoder share a name",
			stdout: "codec_name=h264\nformat_name=h264\n", stderr: decoder + "\n", then: "exit 0"},
		{name: "FLV1, decoded by the decoder named for the demuxer",
			stdout: "codec_name=flv1\nformat_name=flv\n", stderr: "[flv @ 0x1] illegal ac vlc code\n", then: "exit 0"},
		{name: "a last line with no newline", stdout: mkv, stderr: demuxLine, then: "exit 0",
			want: ContainerDiagnostic{Demuxer: "matroska,webm", First: demuxMsg, Count: 1}},
		{name: "a line longer than the bound", stdout: mkv,
			stderr: "[matroska,webm @ 0x1] " + long + "\n", then: "exit 0",
			want: ContainerDiagnostic{Demuxer: "matroska,webm",
				First: long[:maxDiagnosticLine-len("[matroska,webm @ 0x1] ")], Count: 1}},
		{name: "killed by a signal after writing the demuxer's line", stdout: mkv,
			stderr: demuxLine + "\n", then: "kill -9 $$"},
		{name: "its context cancelled after writing the demuxer's line", stdout: mkv,
			stderr: demuxLine + "\n", then: "exec sleep 30", cancelAfter: 300 * time.Millisecond},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := filepath.Join(t.TempDir(), "ffprobe.sh")
			script := "#!/bin/sh\nprintf '%s' '" + tc.stdout + "'\nprintf '%s' '" + tc.stderr + "' >&2\n" + tc.then + "\n"
			if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if tc.cancelAfter > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tc.cancelAfter)
				defer cancel()
			}
			vp := (&Prober{FFprobe: fake}).VideoProps(ctx, "/lib/source.mkv")
			brief := func(d ContainerDiagnostic) string {
				first := d.First
				if len(first) > 80 {
					first = first[:80] + "..."
				}
				return fmt.Sprintf("{Demuxer:%q First:%q (%d bytes) Count:%d}", d.Demuxer, first, len(d.First), d.Count)
			}
			got, damaged := vp.ContainerDamage()
			if got != tc.want || damaged != (tc.want.Count > 0) {
				t.Errorf("ContainerDamage() = %s, %v; want %s, %v", brief(got), damaged, brief(tc.want), tc.want.Count > 0)
			}
		})
	}
}
