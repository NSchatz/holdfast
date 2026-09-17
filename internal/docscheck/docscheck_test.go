package docscheck_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/corpus"
	"github.com/NSchatz/holdfast/internal/docscheck"
	"github.com/NSchatz/holdfast/internal/metrics"
	"github.com/NSchatz/holdfast/internal/store"
)

// shippedMarkdown is every Markdown file this repository ships, from the one place a check
// that reads the shipped documents gets its file set - a WALK and not a list, so a document
// that did not exist when this was written is still searched.
func shippedMarkdown(t *testing.T) []string {
	t.Helper()
	root, err := corpus.RepoRoot(".")
	if err != nil {
		t.Fatalf("locate the repository root: %v", err)
	}
	docs, err := corpus.Markdown(root)
	if err != nil {
		t.Fatalf("list the shipped Markdown: %v", err)
	}
	if len(docs) < 5 {
		t.Fatalf("only %d Markdown file(s) were found under %s - the corpus is not being read, so "+
			"nothing below is being checked", len(docs), root)
	}
	return docs
}

// publishedNames is what THIS build registers, read from the registry's own descriptors.
func publishedNames(t *testing.T) []string {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return metrics.New(st, nil).PublishedNames()
}

// TestEveryPublishedMetricNameIsDocumented grades [AC-11] of S0104: the gate fails if a
// metric name the registry exposes is absent from the shipped Markdown.
//
// A metric name is published the moment somebody scrapes this build, and from then on it
// cannot be renamed without breaking whatever was built on it. Documentation is therefore
// not a courtesy owed after the fact: it is the only way the name reaches the person who
// has to decide what a movement in it means. The obligation is mechanical here so that it
// is met before the name ships rather than after somebody asks.
func TestEveryPublishedMetricNameIsDocumented(t *testing.T) {
	names := publishedNames(t)
	// Anti-vacuity: an empty name set would make the assertion below true of a build that
	// publishes nothing at all.
	if len(names) < 5 {
		t.Fatalf("only %d holdfast metric name(s) were read off the registry (%v) - the published "+
			"set is not being read, so nothing below is being checked", len(names), names)
	}

	missing, err := docscheck.Undocumented(names, shippedMarkdown(t))
	if err != nil {
		t.Fatalf("read the shipped documents: %v", err)
	}
	if len(missing) > 0 {
		t.Errorf("these metric names are PUBLISHED by this build and named in none of the shipped "+
			"documents: %v.\nA scrape carries the name to an operator whether or not anything here "+
			"says what it counts, and the name cannot be renamed afterwards without breaking every "+
			"dashboard already built on it. Document it beside the others in docs/api-reference.md.",
			missing)
	}
}

// TestUndocumented_BitesOnAMetricNobodyDocumented is the check's own self-test: it proves
// the assertion above can FAIL. A documentation check that cannot fail attests to nothing,
// and this one runs over a corpus it did not choose, so "everything is documented" is
// exactly what it would report if it had quietly stopped looking.
func TestUndocumented_BitesOnAMetricNobodyDocumented(t *testing.T) {
	docs := shippedMarkdown(t)
	const invented = "holdfast_a_series_no_document_mentions"

	missing, err := docscheck.Undocumented([]string{invented, "holdfast_files_total"}, docs)
	if err != nil {
		t.Fatalf("read the shipped documents: %v", err)
	}
	if len(missing) != 1 || missing[0] != invented {
		t.Errorf("Undocumented reported %v, want exactly [%s]. The known-documented name must pass "+
			"and the invented one must not, or the check is answering the same way whatever it is asked",
			missing, invented)
	}

	// And it refuses an empty corpus rather than passing everything through it.
	if _, err := docscheck.Undocumented([]string{invented}, nil); err == nil {
		t.Error("Undocumented accepted an empty corpus, which passes every name there is")
	}
}

// TestPublishedNames_ReadsTheRegistryAndNotAScrape is the other half of AC-11's honesty: the
// name set is read from the registry's DESCRIPTORS, so it includes a series that has no
// samples yet. A check built on a scrape would silently shrink to the metrics that happen
// to have data, and would stop covering the rest without saying so.
func TestPublishedNames_ReadsTheRegistryAndNotAScrape(t *testing.T) {
	names := publishedNames(t)
	got := map[string]bool{}
	for _, n := range names {
		got[n] = true
		if !strings.HasPrefix(n, "holdfast_") {
			t.Errorf("PublishedNames returned %q, which is not one of this build's own series - the "+
				"Go and process collectors are not holdfast's to document", n)
		}
	}
	// The queue-depth gauge emits NOTHING over an empty ledger, so a scrape-based reader
	// would not see it here. It is the case that distinguishes the two instruments.
	if !got["holdfast_queue_depth"] {
		t.Errorf("holdfast_queue_depth is registered but absent from %v - a series with no samples "+
			"yet is still a published name", names)
	}
}
