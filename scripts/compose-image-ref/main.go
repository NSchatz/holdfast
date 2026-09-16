// Command compose-image-ref prints the one image reference the example deployment names.
//
// docker-compose.yml is not documentation, it is the stack definition a stranger copies
// and runs, so the reference it carries has to be READ rather than guessed at. This is the
// only reader of it, and scripts/resolve-compose-image.sh - which resolves that reference
// against the registry after a release - asks this program instead of parsing the file a
// second time. A second reader written in sed would agree with this one on today's file and
// disagree on a quoted scalar, a folded one, a second service carrying an `image:`, or an
// `image:` key nested outside `services:`: the "one value, two readers held in step by
// hope" shape this repository refuses for the ffmpeg pin, which is parsed out of the
// Dockerfile rather than restated.
//
// It refuses rather than guesses. No reference, or more than one, is a non-zero exit and a
// message naming what it saw, because a guessed reference is one a release would then
// publish against.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	yaml "go.yaml.in/yaml/v3"
)

const composeFile = "docker-compose.yml"

func main() {
	root := flag.String("root", ".", "repository root holding the example deployment")
	flag.Parse()

	ref, err := composeImageRef(filepath.Join(*root, composeFile))
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	fmt.Println(ref)
}

func composeImageRef(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("the example deployment %s CANNOT BE READ (%v). An image reference that is not there cannot be resolved", filepath.Base(path), err)
	}
	var doc struct {
		Services map[string]struct {
			Image string `yaml:"image"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return "", fmt.Errorf("the example deployment %s CANNOT BE PARSED (%v), so the image reference it names is unknown", filepath.Base(path), err)
	}
	var refs []string
	for _, name := range sortedServiceNames(doc.Services) {
		if img := strings.TrimSpace(doc.Services[name].Image); img != "" {
			refs = append(refs, img)
		}
	}
	switch len(refs) {
	case 1:
		return refs[0], nil
	case 0:
		return "", fmt.Errorf("the example deployment %s NAMES NO IMAGE REFERENCE. A user copying it would build rather than pull, and the reference a release publishes would be checked against nothing", filepath.Base(path))
	default:
		return "", fmt.Errorf("the example deployment %s names %d image references (%s); this reader models one service and refuses to guess which one a release is supposed to publish", filepath.Base(path), len(refs), strings.Join(refs, ", "))
	}
}

func sortedServiceNames[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
