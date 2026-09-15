package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
)

// The PER-FILE surface: a ledger-wide path search, and the withheld paths an operator
// records, reads and removes.
//
// Every route here is inside the token-gated group, and the reason the SEARCH is there
// too is not symmetry. The capped read endpoints ship at most a few hundred rows; a
// ledger-wide search returns rows those have never served, so serving it without a token
// would be a new unauthenticated read of per-file data. It is a CONTROL-gated read
// instead, which adds no unauthenticated one.
//
// What this surface can do is bounded by construction, and the bound is the whole of its
// safety argument: it READS the ledger, and it WITHHOLDS a path. It creates, modifies and
// deletes no media file, it offers nothing to the encoder that was not already eligible -
// withholding only ever takes a file OUT - and it never writes the configuration file, on
// any path, for any request. `requeue`, `restore` and `resolve` stay local commands for
// the reason they always have: each changes what the engine will DO to a media file.

// searchLimit caps what one ledger search ships. It is larger than the history view's cap
// because the whole point of the search is to reach rows that view cannot, and small
// enough that no request can ask the daemon for an unbounded payload off its one
// serialized connection. The MATCH COUNT is over the whole ledger and is reported beside
// the rows, so a search that hit the cap says what it capped against.
const searchLimit = 200

// maxPathBytes is the longest path this surface will accept. Linux's own PATH_MAX is 4096
// including the terminator, so anything past it cannot name a file that exists and is
// refused before it reaches the store rather than being recorded as a withholding nothing
// will ever match.
const maxPathBytes = 4095

// searchResponse is what a ledger search returns: the matching rows in the same projection
// the history view uses, plus how many matched over the WHOLE ledger.
//
// The rows are jobDTO, deliberately: a search result and a history row are the same fact
// about the same ledger, and a second projection of one row is a second thing to keep in
// step. The total carries the aggregates' envelope for the reason every total here does -
// one that could not be read is STATED as unreadable beside rows that still ship.
type searchResponse struct {
	Term    string      `json:"term"`
	Results []jobDTO    `json:"results"`
	Total   rowTotalDTO `json:"total"`
}

// exclusionDTO is one withheld path on the wire.
type exclusionDTO struct {
	Path      string `json:"path"`
	CreatedAt int64  `json:"created_at"`
}

// exclusionsResponse is every withholding in force, with the one sentence the surface owes
// about what they ARE. The sentence is on the wire rather than only in the page so that a
// client of this API cannot render the list without it: a withholding an operator reads as
// configuration is one they will go looking for in a file that does not mention it.
type exclusionsResponse struct {
	Exclusions   []exclusionDTO `json:"exclusions"`
	RuntimeState string         `json:"runtime_state"`
}

// runtimeStateNotice is that sentence, in one place, so the API and the page cannot drift
// into two different accounts of where a withholding lives.
const runtimeStateNotice = "Withheld paths are runtime state this daemon holds. " +
	"Nothing here is written to the configuration file, and no configuration key holds them."

// excludeResponse is the answer to recording or removing one withholding. `changed` is
// what ACTUALLY moved rather than what was asked for, so a caller counts the former: an
// operator told a path was newly withheld when it already was learns nothing false, but a
// caller that reported success against a store that refused would.
type excludeResponse struct {
	Path         string `json:"path"`
	Changed      bool   `json:"changed"`
	RuntimeState string `json:"runtime_state"`
}

// handleLedgerSearch answers a path search over every terminal row in the ledger.
//
// It is a pure read. The term is taken as TEXT by the store, so a `%` or a `_` in it
// matches those characters and never a wildcard.
func (s *Server) handleLedgerSearch(w http.ResponseWriter, r *http.Request) {
	term := strings.TrimSpace(r.URL.Query().Get("path"))
	if term == "" {
		http.Error(w, "the search needs a path term: ?path=<substring of the path you are looking for>. "+
			"An empty term is not a search over everything, it is a question nobody asked", http.StatusBadRequest)
		return
	}
	if len(term) > maxPathBytes {
		http.Error(w, "the search term is longer than any path this system can hold", http.StatusBadRequest)
		return
	}
	limit := searchLimit
	if q := r.URL.Query().Get("limit"); q != "" {
		// Clamped into (0, searchLimit] exactly as the history view clamps its own: a
		// caller may ask for fewer and never for more.
		if n, err := strconv.Atoi(q); err == nil && n > 0 && n <= limit {
			limit = n
		}
	}

	rows, total, err := s.store.SearchPath(r.Context(), terminal, term, limit)
	if err != nil {
		s.fail(w, "ledger search", err)
		return
	}
	out := searchResponse{
		Term:    term,
		Results: toDTOs(rows),
		Total: rowTotalDTO{
			Available: total.Err == nil,
			Covers:    total.Coverage.Set,
			Cap:       limit,
		},
	}
	if total.Err != nil {
		s.log.Warn("ledger search total unavailable (the rows still ship)", "err", total.Err)
		out.Total.Unavailable = aggregateUnavailable
	} else {
		n := total.Count
		out.Total.Count = &n
	}
	writeJSON(w, http.StatusOK, out)
}

// handleExclusionsList renders every withholding in force.
func (s *Server) handleExclusionsList(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ExcludedPaths(r.Context())
	if err != nil {
		s.fail(w, "withheld paths", err)
		return
	}
	out := exclusionsResponse{Exclusions: make([]exclusionDTO, 0, len(list)), RuntimeState: runtimeStateNotice}
	for _, e := range list {
		out.Exclusions = append(out.Exclusions, exclusionDTO{Path: e.Path, CreatedAt: e.CreatedAt})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleExcludeAdd records a withholding. It is the ONE thing on this surface that writes
// anything, and what it writes is a path this daemon will not offer to the pipeline.
func (s *Server) handleExcludeAdd(w http.ResponseWriter, r *http.Request) {
	path, err := s.acceptPath(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	changed, err := s.store.ExcludePath(r.Context(), path)
	if err != nil {
		// NOTHING CHANGED, and the response says exactly that. A refusal a reader could
		// take for success is the failure mode this whole surface is built against: the
		// operator would believe a file is held out while the next scan picks it up.
		s.log.Warn("could not record a withheld path", "path", path, "err", err)
		http.Error(w, "nothing changed: the withheld paths could not be written", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, excludeResponse{Path: path, Changed: changed, RuntimeState: runtimeStateNotice})
}

// handleExcludeRemove removes a withholding, after which the path is eligible again on the
// next scan. It exists for the same reason the record does: a withholding nobody can
// remove from the surface that created it is a file that silently stopped being worked on.
func (s *Server) handleExcludeRemove(w http.ResponseWriter, r *http.Request) {
	path, err := s.acceptPath(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	changed, err := s.store.UnexcludePath(r.Context(), path)
	if err != nil {
		s.log.Warn("could not remove a withheld path", "path", path, "err", err)
		http.Error(w, "nothing changed: the withheld paths could not be written", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, excludeResponse{Path: path, Changed: changed, RuntimeState: runtimeStateNotice})
}

// maxPerFileBody bounds what this surface will read off a request. The body carries one
// path and nothing else.
const maxPerFileBody = 1 << 16

// acceptPath reads the path a per-file action names and REFUSES every one this build
// cannot accept, naming the reason.
//
// The refusals are not hygiene. A withholding is keyed on the path text, so a path that
// names no file this daemon could ever enumerate is a record that will never match
// anything and will never be re-derived away either: it would sit in the operator's own
// list for ever, looking like a file being held out, holding nothing out.
//
// A path outside every configured library root is refused for the same reason and one
// more: this surface must not become a way to write arbitrary strings into the daemon's
// state keyed on paths it has no business knowing about.
func (s *Server) acceptPath(r *http.Request) (string, error) {
	var body struct {
		Path string `json:"path"`
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxPerFileBody))
	if err != nil {
		return "", errors.New("the request body could not be read")
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return "", errors.New(`the request body must be JSON of the form {"path": "/library/root/file.mkv"}`)
	}
	return s.acceptPathValue(body.Path)
}

// acceptPathValue is the rule itself, taken apart from the transport so it can be exercised
// one refusal at a time.
func (s *Server) acceptPathValue(p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", errors.New("nothing changed: no path was named")
	}
	if len(p) > maxPathBytes {
		return "", errors.New("nothing changed: the path is longer than any path this system can hold")
	}
	if strings.ContainsRune(p, 0) {
		return "", errors.New("nothing changed: the path contains a NUL byte, so it names no file")
	}
	// A tab or a newline in a path is pathological and the engine already refuses to
	// process one: a withholding keyed on such a path could never match a file the scan
	// offers, so recording it would be a record that holds nothing out.
	if strings.ContainsAny(p, "\t\n\r") {
		return "", errors.New("nothing changed: the path contains a tab or a newline, which this build does not process")
	}
	if !filepath.IsAbs(p) {
		return "", errors.New("nothing changed: the path is not absolute, so it names no file on this machine")
	}
	clean := filepath.Clean(p)
	roots := s.cfg.LibraryRoots
	if len(roots) == 0 {
		return "", errors.New("nothing changed: this daemon has no library root configured, so no path is inside one")
	}
	for _, root := range roots {
		if withinRoot(clean, filepath.Clean(root)) {
			return clean, nil
		}
	}
	return "", fmt.Errorf("nothing changed: %q is outside every configured library root (%s), "+
		"so nothing this daemon scans could ever match it", clean, strings.Join(roots, ", "))
}

// withinRoot reports whether clean is root itself or something under it. The comparison is
// on cleaned path ELEMENTS and never on a string prefix: "/media/films-old" begins with
// "/media/films" and is a different directory.
func withinRoot(clean, root string) bool {
	if clean == root {
		return true
	}
	rel, err := filepath.Rel(root, clean)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
