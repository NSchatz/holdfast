package version

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// The container image's binary is the only one built with the holdfast_image tag, and the
// only one whose Packaging is "image". The gate cannot build the image, so it checks the
// wiring: the Dockerfile's go build passes the tag, and the Makefile's does not (a host build
// must keep `encoder: amf`).
func TestPackaging_OnlyTheImageBuildPassesTheTag(t *testing.T) {
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join("..", "..", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	tagged := regexp.MustCompile(`go build[^\n]*-tags[ =]holdfast_image\b`)
	if !tagged.MatchString(read("Dockerfile")) {
		t.Error("the Dockerfile's go build does not pass -tags holdfast_image: the image's binary would " +
			"not know it is the image's, and would probe amf rather than refuse it with its reason")
	}
	if regexp.MustCompile(`holdfast_image`).MatchString(read("Makefile")) {
		t.Error("the Makefile builds with holdfast_image: a host build would refuse amf")
	}
	if Packaging != "" {
		t.Errorf("an untagged build's Packaging = %q, want \"\"", Packaging)
	}
}
