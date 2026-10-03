package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/NSchatz/holdfast/internal/store"
)

// GET /api/history: the status filter and the cursor (S0170).
//
// The endpoint used to ship the newest 200 terminal rows and nothing else, so any fact
// about an older row meant copying the ledger out. It now reads the whole ledger one
// bounded page at a time, filtered to the verdicts asked about.
//
// It stays a pure READ. What it can get wrong is the audit trail itself - a row dropped or
// repeated between two pages, or a traversal that ends early and reads as complete - and
// three rules here exist for that and nothing else:
//
//   - the page order is TOTAL (store.ListPage), so a boundary falls between two rows and
//     never through a group of rows the order cannot tell apart;
//   - `next_cursor` is non-null exactly when a further row was READ, in the same statement
//     as the page, so the last page says it is the last;
//   - a parameter this endpoint cannot read is REFUSED, never guessed at. An empty page for
//     a mistyped status reads as "no such rows", and a cursor continued under a different
//     filter reads as one traversal while being two.

// The refusal tokens. They are a CLOSED vocabulary, stated in docs/api-reference.md, so a
// caller branches on a token and never on prose.
const (
	// ruleInvalidQuery is the top-level rule of every refusal of a query parameter.
	ruleInvalidQuery = "invalid-query"

	// ruleStatusNotTerminal is a `status` value outside the terminal vocabulary: an unknown
	// word, a status the queue serves, or an empty element.
	ruleStatusNotTerminal = "status-not-terminal"

	// ruleCursorUndecodable is a `cursor` this build's encoding cannot read: empty,
	// truncated, altered, given twice, or not one of its tokens at all.
	ruleCursorUndecodable = "cursor-undecodable"

	// ruleCursorFilterMismatch is a readable cursor presented with a status set other than
	// the one it was minted under.
	ruleCursorFilterMismatch = "cursor-filter-mismatch"
)

// The query parameters this file reads, named once.
const (
	paramStatus = "status"
	paramCursor = "cursor"
)

// parameterRefusal is what was wrong with ONE query parameter.
type parameterRefusal struct {
	Parameter string `json:"parameter"`
	Rule      string `json:"rule"`
	Error     string `json:"error"`
}

// queryRefusal is the body of a 400 from a read endpoint that takes query parameters. Every
// parameter that was wrong is named in the ONE response, so a caller fixes its request once
// rather than once per parameter. Retryable is false always: a query that is malformed is
// malformed again on the next send.
type queryRefusal struct {
	Rule       string             `json:"rule"`
	Error      string             `json:"error"`
	Retryable  bool               `json:"retryable"`
	Parameters []parameterRefusal `json:"parameters"`
}

// refuseQuery writes the 400.
func refuseQuery(w http.ResponseWriter, problems []parameterRefusal) {
	names := make([]string, 0, len(problems))
	for _, p := range problems {
		names = append(names, p.Parameter)
	}
	writeJSON(w, http.StatusBadRequest, queryRefusal{
		Rule: ruleInvalidQuery,
		Error: "nothing was read: the request's " + strings.Join(names, " and ") +
			" could not be accepted, and a page served under a parameter this endpoint could not read " +
			"would be an answer to a question nobody asked",
		Retryable:  false,
		Parameters: problems,
	})
}

// terminalNames is the terminal vocabulary in words, for a refusal to state.
func terminalNames() string {
	names := make([]string, len(terminal))
	for i, st := range terminal {
		names[i] = string(st)
	}
	return strings.Join(names, ", ")
}

// maxEchoedValue bounds how much of a refused value a refusal quotes back.
const maxEchoedValue = 64

func echoed(v string) string {
	if len(v) > maxEchoedValue {
		return strconv.Quote(v[:maxEchoedValue]) + "..."
	}
	return strconv.Quote(v)
}

// statusFilter reads the `status` parameter: repeated, comma-separated, or both, the filter
// being their union.
//
// It returns the set in the TERMINAL VOCABULARY'S OWN ORDER with duplicates folded, which is
// what makes two spellings of one set the same request: the rows, the total's `covers` and
// the cursor's binding are all computed from this one canonical list. With no `status`
// parameter at all it returns nil, which is the unfiltered request and not a filter naming
// everything.
//
// Every element is held to the vocabulary, the empty one included. `status=` and
// `status=done,` are requests whose author meant something this endpoint cannot recover,
// and reading them as "no filter" or as "done" would be answering a different question.
func statusFilter(q url.Values) ([]store.Status, *parameterRefusal) {
	raw, given := q[paramStatus]
	if !given {
		return nil, nil
	}
	asked := map[store.Status]bool{}
	var bad []string
	for _, value := range raw {
		for _, element := range strings.Split(value, ",") {
			st, ok := terminalStatus(element)
			if !ok {
				bad = append(bad, echoed(element))
				continue
			}
			asked[st] = true
		}
	}
	if len(bad) > 0 {
		return nil, &parameterRefusal{
			Parameter: paramStatus,
			Rule:      ruleStatusNotTerminal,
			Error: "status takes the terminal statuses only (" + terminalNames() + "), repeated or " +
				"comma-separated, and " + strings.Join(bad, ", ") + " is not one of them",
		}
	}
	set := make([]store.Status, 0, len(asked))
	for _, st := range terminal {
		if asked[st] {
			set = append(set, st)
		}
	}
	return set, nil
}

// terminalStatus is the terminal status a word spells, exactly.
func terminalStatus(word string) (store.Status, bool) {
	for _, st := range terminal {
		if word == string(st) {
			return st, true
		}
	}
	return "", false
}

// filterKey is the status set as a cursor binds it: the canonical list (statusFilter's)
// joined, and "" for the unfiltered request.
func filterKey(set []store.Status) string {
	names := make([]string, len(set))
	for i, st := range set {
		names[i] = string(st)
	}
	return strings.Join(names, ",")
}

// cursorVersion is the encoding's version. A token of any other is not this build's.
const cursorVersion = 1

// historyCursor is what a `next_cursor` token carries: the position the next page starts
// strictly after, and the status set the traversal is over.
//
// The token is OPAQUE to a caller and is not a secret: it is base64url over this JSON. It
// carries the row's fingerprint, which the row's wire shape deliberately omits, because the
// fingerprint is the final tie-break of the order and a position without it is not a
// position. Tamper detection is not attempted: a readable token that was altered is a
// read-only request for another position, behind the same gate.
type historyCursor struct {
	Version     int    `json:"v"`
	UpdatedAt   int64  `json:"u"`
	Path        string `json:"p"`
	Fingerprint string `json:"f"`
	Filter      string `json:"s"`
}

// mintCursor is the token that continues a traversal after pos under set.
func mintCursor(pos store.PagePosition, set []store.Status) string {
	raw, err := json.Marshal(historyCursor{
		Version: cursorVersion, UpdatedAt: pos.UpdatedAt, Path: pos.Path,
		Fingerprint: pos.Fingerprint, Filter: filterKey(set),
	})
	if err != nil {
		// Unreachable: the struct holds an int, an int64 and three strings.
		panic(fmt.Sprintf("server: history cursor: %v", err))
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// decodeCursor reads a token back, or reports that it is not one of this build's. It is
// strict on purpose: exactly one JSON object with exactly the fields above, at this
// version, naming a path. A token that reads as anything looser is refused rather than
// continued from whatever position its remains happen to spell.
func decodeCursor(token string) (historyCursor, bool) {
	raw, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil {
		return historyCursor{}, false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var c historyCursor
	if err := dec.Decode(&c); err != nil {
		return historyCursor{}, false
	}
	if dec.More() {
		return historyCursor{}, false
	}
	if c.Version != cursorVersion || c.Path == "" || c.UpdatedAt < 0 {
		return historyCursor{}, false
	}
	return c, true
}

// cursorParam reads the `cursor` parameter. It returns nil for a request that carries none -
// the first page of a traversal - and a refusal for one that carries a token this build
// cannot read. A parameter that is PRESENT and empty is the second, not the first: a client
// that sent `cursor=` meant to continue, and serving it the newest page would restart its
// traversal while it believed it was finishing one.
func cursorParam(q url.Values) (*historyCursor, *parameterRefusal) {
	raw, given := q[paramCursor]
	if !given {
		return nil, nil
	}
	undecodable := &parameterRefusal{
		Parameter: paramCursor,
		Rule:      ruleCursorUndecodable,
		Error: "cursor is not a token this server issued: send the next_cursor of the previous page " +
			"exactly as it was served, once, or leave the parameter out to start from the newest row",
	}
	if len(raw) != 1 {
		return nil, undecodable
	}
	c, ok := decodeCursor(raw[0])
	if !ok {
		return nil, undecodable
	}
	return &c, nil
}

// historyLimitOf reads `limit`: clamped into (0, historyLimit], anything larger keeping the
// cap and anything else the default. `<=` lets a caller ask for exactly the cap.
func historyLimitOf(q url.Values) int {
	limit := historyLimit
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= limit {
			limit = n
		}
	}
	return limit
}

// historyQuery is one accepted request.
type historyQuery struct {
	limit int
	// filter is the canonical status set, nil for the unfiltered request.
	filter []store.Status
	// after is the position the page starts strictly after, nil for the first page.
	after *store.PagePosition
}

// statuses is the set the page is read over: the filter, or every terminal status.
func (q historyQuery) statuses() []store.Status {
	if q.filter == nil {
		return terminal
	}
	return q.filter
}

// parseHistoryQuery reads every parameter and collects EVERY failure before answering, so
// one response names both parameters when both are wrong.
//
// A cursor is checked against the filter only when the filter itself was readable: with
// the status unreadable there is no set to compare the cursor's with, and the status's own
// refusal is the whole of what can honestly be said.
func parseHistoryQuery(q url.Values) (historyQuery, []parameterRefusal) {
	out := historyQuery{limit: historyLimitOf(q)}
	var problems []parameterRefusal

	filter, statusProblem := statusFilter(q)
	if statusProblem != nil {
		problems = append(problems, *statusProblem)
	}
	out.filter = filter

	cur, cursorProblem := cursorParam(q)
	switch {
	case cursorProblem != nil:
		problems = append(problems, *cursorProblem)
	case cur != nil && statusProblem == nil && cur.Filter != filterKey(filter):
		problems = append(problems, parameterRefusal{
			Parameter: paramCursor,
			Rule:      ruleCursorFilterMismatch,
			Error: "cursor was issued under " + describeFilter(cur.Filter) + " and this request asks for " +
				describeFilter(filterKey(filter)) + ": a traversal keeps one status set from its first page " +
				"to its last, so send the set the cursor was issued under or start again without a cursor",
		})
	case cur != nil:
		out.after = &store.PagePosition{UpdatedAt: cur.UpdatedAt, Path: cur.Path, Fingerprint: cur.Fingerprint}
	}
	return out, problems
}

// describeFilter is a cursor's binding in words.
func describeFilter(key string) string {
	if key == "" {
		return "no status filter"
	}
	return "the status filter " + echoed(key)
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	query, problems := parseHistoryQuery(r.URL.Query())
	if len(problems) > 0 {
		refuseQuery(w, problems)
		return
	}
	jobs, more, err := s.reads().ListPage(r.Context(), query.statuses(), query.after, query.limit)
	if err != nil {
		s.fail(w, "history", err)
		return
	}
	out := historyResponse{
		History:      toDTOs(jobs),
		HistoryTotal: s.historyTotal(r, query),
	}
	if more {
		// More is true only where the page is full, so there is a last row to continue from.
		next := mintCursor(store.PositionOf(jobs[len(jobs)-1]), query.filter)
		out.NextCursor = &next
	}
	writeJSON(w, http.StatusOK, out)
}

// historyTotal is the total the page was capped against.
//
// history_total counts the matching rows in the LEDGER, so it is the same figure whether
// the caller took the cap or asked for fewer: `cap` moves with the request, `count` does
// not. Unfiltered it is the cached whole-ledger figure the stream publishes. Under a filter
// it is read for this response, over the filtered set, the way the ledger search reads its
// own - and like every total here a read that fails is STATED as unavailable beside rows
// that still ship, never reported as a count of zero.
func (s *Server) historyTotal(r *http.Request, query historyQuery) rowTotalDTO {
	if query.filter == nil {
		return rowTotalOf(s.hub.ledgerFigures(r.Context(), true).HistoryTotal, query.limit)
	}
	total := s.reads().CountRows(r.Context(), query.filter)
	out := rowTotalDTO{Available: total.Err == nil, Covers: total.Coverage.Set, Cap: query.limit}
	if total.Err != nil {
		s.log.Warn("filtered history total unavailable (the rows still ship)", "err", total.Err)
		out.Unavailable = aggregateUnavailable
		return out
	}
	n, age := total.Count, int64(0)
	out.Count, out.AgeSeconds = &n, &age
	return out
}
