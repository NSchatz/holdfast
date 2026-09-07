// Command genindex writes internal/webui/index.html from the source modules under
// internal/webui/src. It is the only writer of that file.
//
//	go run ./internal/webui/gen/genindex            # regenerate (make webui-gen)
//	go run ./internal/webui/gen/genindex -check     # fail if the committed file is stale
//
// The whole body lives in internal/webui/gen so the tests drive the real command rather
// than a stand-in.
package main

import (
	"os"

	"github.com/NSchatz/holdfast/internal/webui/gen"
)

func main() { os.Exit(gen.Main(os.Args[1:], os.Stdout, os.Stderr)) }
