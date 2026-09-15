// Command comment-density-gate prints this repository's Go comment-density table and
// refuses a file over the ceiling. Part of `make check`, via `make comment-density`.
// Everything it decides lives in internal/commentdensity.
package main

import (
	"fmt"
	"os"

	"github.com/NSchatz/holdfast/internal/commentdensity"
)

func main() {
	root, err := commentdensity.RepoRoot(".")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := commentdensity.Run(root, os.Stdout, commentdensity.Exemptions); err != nil {
		os.Exit(1)
	}
}
