// Package docscheck grades what this build PUBLISHES against what it DOCUMENTS.
//
// A published Prometheus metric name is permanent in a way most identifiers are not: it
// cannot be renamed without breaking every dashboard and alert an operator has already
// built on it, and it reaches them through a scrape rather than through anything they read
// here. So the name is the commitment, and a name nobody can look up is a commitment made
// to somebody who was never told what it means.
//
// The check is deliberately PRESENCE and nothing more. It answers "is this name written
// down somewhere in the shipped documents", which is mechanical and cannot be argued with.
// Whether the prose around it is TRUE, or useful, is not a question any mechanical check
// can answer, and one that pretended to would be a check nobody could trust either way.
package docscheck

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// Undocumented returns the names that appear in NONE of the documents at docPaths, sorted.
//
// An unreadable document is an ERROR and never a pass: a check that skipped the file it
// could not open would report "everything is documented" the moment the corpus moved, which
// is the one answer it must never give by accident.
func Undocumented(names, docPaths []string) ([]string, error) {
	if len(docPaths) == 0 {
		return nil, fmt.Errorf("docscheck: no documents to search - a check with no corpus passes everything")
	}
	corpus := make([]string, 0, len(docPaths))
	for _, p := range docPaths {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("docscheck: read %s: %w", p, err)
		}
		corpus = append(corpus, string(b))
	}

	var missing []string
	for _, name := range names {
		found := false
		for _, doc := range corpus {
			if strings.Contains(doc, name) {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	return missing, nil
}
