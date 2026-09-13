package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// How the census is published, in the two forms AC1c pins: a human-readable table by
// default, and exactly one JSON document behind --json.
//
// Both are rendered from the SAME census value, so "the JSON carries every figure the
// table carries" is a property of there being one source rather than a promise to keep
// two renderers in step. The table deliberately does not parse as JSON - it opens with
// a sentence - so a script cannot mistake the default output for the machine-readable
// form and read half a census as a whole one.

// writeJSON writes the census as one JSON document. One document, one value: a reader
// may `json.Unmarshal` the whole of stdout.
func (c *census) writeJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(c)
}

func (c *census) writeTable(w io.Writer) {
	fmt.Fprintf(w, "holdfast analyze - a census of the configured library roots.\n")
	fmt.Fprintf(w, "Nothing below was read from the ledger: every figure is the filesystem as it is now.\n")

	fmt.Fprintf(w, "\nstorage: %s\n", wrapAt(c.Storage.Verdict, 92, "  "))
	for _, cause := range c.Storage.Causes {
		fmt.Fprintf(w, "  %s\n      %s: %s\n", cause.Path, cause.Kind, cause.Detail)
		if cause.Declaration != "" {
			fmt.Fprintf(w, "      a mutating run would be permitted there by adding to your configuration:\n"+
				"        allow_non_local:\n        - %s\n", cause.Declaration)
		}
	}
	for _, r := range c.Storage.Reduced {
		fmt.Fprintf(w, "  reduced no-loss guarantee: %s\n", r)
	}

	fmt.Fprintf(w, "\nledger: %s\n", wrapAt(c.Ledger, 92, "  "))

	fmt.Fprintf(w, "\nresolution bands, by %s:\n  %s\n  %s\n",
		c.Bands.Basis, strings.Join(c.Bands.Bands, " | "), wrapAt(c.Bands.Note, 92, "  "))

	for _, rc := range c.Roots {
		rc.writeTable(w, "library root "+rc.Root)
	}
	if c.Total != nil {
		c.Total.writeTable(w, "all configured library roots, combined")
	}
}

func (rc *rootCensus) writeTable(w io.Writer, heading string) {
	fmt.Fprintf(w, "\n%s\n", heading)
	if rc.Note != "" {
		fmt.Fprintf(w, "  note: %s\n", rc.Note)
	}
	fmt.Fprintf(w, "  found     %8d file(s)  %16d byte(s)\n", rc.Found.Files, rc.Found.Bytes)
	fmt.Fprintf(w, "            set: %s\n", rc.Found.Set)
	fmt.Fprintf(w, "  sources   %8d file(s)  %16d byte(s)\n", rc.Sources.Files, rc.Sources.Bytes)
	fmt.Fprintf(w, "            set: %s\n", rc.Sources.Set)
	fmt.Fprintf(w, "  withheld  %8d file(s)  %16d byte(s), by these mechanisms and no others:\n",
		rc.WithheldTotal.Files, rc.WithheldTotal.Bytes)
	for _, m := range rc.Withheld {
		fmt.Fprintf(w, "    %-26s %6d file(s)  %14d byte(s)  %s\n", m.Name, m.Files, m.Bytes, m.Detail)
	}
	fmt.Fprintf(w, "  coverage  %8d directory(ies) read, %d not read. The coverage boundary is the LAST\n"+
		"            mechanism between the two figures above, and the only one with no file count:\n"+
		"            what is inside a directory nobody listed is unknown, never reported as absent.\n",
		rc.Coverage.DirectoriesRead, rc.Coverage.DirectoriesNotRead)
	for _, b := range rc.Coverage.NotReadWhy {
		fmt.Fprintf(w, "    %-44s %6d\n", b.Key, b.Count)
	}
	fmt.Fprintf(w, "  files the census could not inspect: %d\n", rc.Coverage.FilesNotInspected)

	for _, d := range rc.Distributions {
		fmt.Fprintf(w, "\n  %s\n    set: %s\n", d.Name, d.Set)
		if d.Note != "" {
			fmt.Fprintf(w, "    %s\n", wrapAt(d.Note, 88, "    "))
		}
		if d.Unavailable != "" {
			fmt.Fprintf(w, "    UNAVAILABLE: %s\n", wrapAt(d.Unavailable, 88, "    "))
		}
		for _, b := range d.Buckets {
			fmt.Fprintf(w, "    %-44s %6d\n", b.Key, b.Count)
		}
		fmt.Fprintf(w, "    %-44s %6d\n", "counted", d.Counted)
		fmt.Fprintf(w, "    %-44s %6d\n", "excluded", d.Excluded)
		for _, b := range d.ExcludedWhy {
			fmt.Fprintf(w, "      %-42s %6d\n", b.Key, b.Count)
		}
	}
}

func (c *census) writeHealthTable(w io.Writer) {
	h := c.Health
	if h == nil {
		return
	}
	fmt.Fprintf(w, "\ndecode-integrity\n  set: %s\n", h.Set)
	if h.Unavailable != "" {
		fmt.Fprintf(w, "  UNAVAILABLE: %s\n", wrapAt(h.Unavailable, 88, "  "))
		return
	}
	fmt.Fprintf(w, "  checked %d file(s); %d did not decode cleanly.\n", h.Checked, len(h.Failed))
	for _, p := range h.Failed {
		fmt.Fprintf(w, "    FAILED %s\n", p)
	}
	fmt.Fprintf(w, "  Reported only: every file above is byte-for-byte where it was, no ledger row was "+
		"written about it,\n  and the next `holdfast run` will treat it exactly as it would have had this "+
		"pass never happened.\n")
}

// wrapAt breaks a sentence onto lines of at most width runes, indenting every line
// after the first. A census is read by a person in a terminal, and a 400-column
// paragraph is a paragraph they will not read.
func wrapAt(s string, width int, indent string) string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return ""
	}
	var b strings.Builder
	line := 0
	for i, word := range words {
		switch {
		case i == 0:
			b.WriteString(word)
			line = len(word)
		case line+1+len(word) > width:
			b.WriteString("\n" + indent + word)
			line = len(indent) + len(word)
		default:
			b.WriteString(" " + word)
			line += 1 + len(word)
		}
	}
	return b.String()
}
