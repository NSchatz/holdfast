package server

// POST /api/scan - the targeted scan endpoint (S0093).
//
// It is the network-reachable spelling of "look at THIS file now". An *arr's import
// webhook, a Jellyfin plugin or a shell script sends a path; holdfast reaches the same
// verdict on that one file that a whole-library scan would have reached on the next tick,
// and a steady-state deployment can then run with its periodic scan turned off.
//
// This file adds a CALLER to the pipeline and nothing else. What it may never become is
// the thing that matters, because the pipeline ends in the deletion of an original that no
// re-run undoes:
//
//   - it sits in the token-gated group, so it is disabled outright until an operator
//     configures a control token, exactly as rescan/pause/resume are;
//   - every submitted path is RESOLVED before it is judged, and a path that does not
//     resolve at or beneath a configured library root is refused here - nothing
//     downstream would refuse it, because everything downstream is licensed to rewrite the
//     file it was handed;
//   - an accepted path goes to the SAME pipeline entry point a scan's worker uses. There
//     is no fast path here, no second copy of a guard and no answer this file gives that
//     the engine would have given differently;
//   - it re-opens no row, restores no original and resolves no parked incident. Those stay
//     LOCAL commands by ratified operator decision, and TestScanEndpoint_AddsNoRouteBeyondScan
//     is what holds this file to it.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// Submissions is the targeted-scan queue this endpoint feeds. engine.Submissions is the
// production implementation; the interface is here so this package states exactly what it
// needs from the engine and so a test can drive the endpoint without one.
type Submissions interface {
	// Judge applies the ONE eligibility decision every scan applies, to a path arriving
	// from outside. It returns the RESOLVED path the pipeline would be handed and
	// ok=true, or ok=false with the rule token that refused it and a detail for a human.
	// It enqueues nothing and opens no media file.
	Judge(path string) (resolved, rule, detail string, ok bool)

	// Offer enqueues a resolved path for processing, reporting false when the queue is
	// full. It never blocks - a handler that waited for room would be a handler waiting
	// for an encode.
	Offer(resolved string) bool

	// Cap is the queue's capacity, so a refusal can name the bound it hit.
	Cap() int

	// Wait blocks until in-flight targeted work has returned, so a shutdown joins it
	// before the store handle is closed.
	Wait()
}

// MaxScanPaths is the documented per-request path maximum. A request naming more is
// refused WHOLE rather than partially accepted: a caller that got half its list processed
// and a 4xx would have no way to tell which half.
//
// It is stated in docs/api-reference.md as a number, because the refusal names it and a
// caller sizing a batch has to be able to read it somewhere other than a rejection.
const MaxScanPaths = 256

// MaxScanBodyBytes is the documented maximum body size, enforced before the JSON is
// parsed. Media paths are short and 256 of them do not approach this; the bound exists so
// an unauthenticated-by-accident deployment cannot be made to buffer an arbitrary body.
const MaxScanBodyBytes = 256 * 1024

// scanResult is what happened to ONE submitted path, in the order it was submitted. Every
// path gets one, accepted or not: a report that listed only the failures would leave a
// caller to work out what was taken by subtraction, and a silent no-op is the failure mode
// this endpoint is written to avoid.
type scanResult struct {
	Path     string `json:"path"`
	Accepted bool   `json:"accepted"`
	// Resolved is the path the pipeline will act on - the one every rule was answered
	// against. Present only on an accepted path, and worth carrying because it is not
	// always the path that was sent (a container path reached through a symbolic link).
	Resolved string `json:"resolved,omitempty"`
	// Rule is the token that refused this path, from the engine's closed vocabulary.
	Rule string `json:"rule,omitempty"`
	// Detail is the same refusal in words, naming what was seen.
	Detail string `json:"detail,omitempty"`
	// Retryable says whether sending THIS path again, unchanged, could succeed. It is a
	// machine-readable answer to the only question a caller has after a refusal, and it is
	// false for every eligibility rule: a path outside the library roots does not move into
	// them by being sent twice, and a retry loop around one is a retry loop that never ends.
	Retryable bool `json:"retryable"`
}

// scanResponse is the body of EVERY answer this endpoint gives, whether it reached the
// per-path stage or not. One envelope rather than two: a caller that has to tell a
// whole-request refusal from a per-path one by guessing at the content type has to parse
// prose to find out what happened.
//
// On a whole-request refusal Results is empty and Rule/Error carry what was wrong. On an
// answer that reached the per-path stage the counts are a convenience and Results is the
// record.
type scanResponse struct {
	Accepted int `json:"accepted"`
	Rejected int `json:"rejected"`
	// Rule is the stable token for a WHOLE-request refusal, from the closed vocabulary
	// below. Empty on an answer that reached the per-path stage, where each result carries
	// its own rule instead.
	Rule string `json:"rule,omitempty"`
	// Error is that same refusal in words, naming the bound or the state it hit.
	Error string `json:"error,omitempty"`
	// Retryable says whether sending this request again, unchanged, could succeed: true
	// for a queue that is full and for a pause an operator lifts, false for a body that is
	// malformed or over a bound and for every path rule.
	Retryable bool         `json:"retryable"`
	Results   []scanResult `json:"results"`
}

// The rule tokens this package adds to the engine's, for the refusals that are properties
// of the REQUEST rather than of the path. They are a CLOSED vocabulary, stated in
// docs/api-reference.md, so a caller can branch on a token rather than on prose.
const (
	// ruleDuplicate is a path named more than once in one request. It is accepted at most
	// once; the later mentions say so rather than reporting a phantom second acceptance.
	ruleDuplicate = "duplicate-in-request"

	// ruleQueueFull is a path that passed every rule and could not be taken. It is
	// reported per path, because which paths were not taken is the only thing a caller
	// needs in order to retry correctly, and it is the one per-path refusal that IS
	// retryable: the queue drains.
	ruleQueueFull = "submission-queue-full"

	// ruleNotWired is a server with no queue behind the route. Not reachable in the
	// daemon, which wires one before the listener exists.
	ruleNotWired = "targeted-scanning-not-wired"

	// rulePaused is the controller paused. Retryable after POST /api/resume.
	rulePaused = "paused"

	// ruleMalformedBody is any way the body is not a readable {"paths": [...]} object.
	ruleMalformedBody = "malformed-body"

	// ruleTooManyPaths is more than MaxScanPaths paths in one request; ruleBodyTooLarge is
	// a body over MaxScanBodyBytes. Both refuse the request WHOLE, and neither is
	// retryable as sent - the caller splits it.
	ruleTooManyPaths   = "too-many-paths"
	ruleBodyTooLarge   = "body-too-large"
	ruleUnreadableBody = "unreadable-body"
)

// handleScan is the endpoint. It answers BEFORE anything is processed - an HTTP handler
// must never be held open across an encode - so the body reports what was ACCEPTED, never
// what was decided about the file.
//
// The statuses, all of them:
//
//	202  at least one path was accepted and enqueued
//	400  the body was malformed, or every submitted path was refused
//	401  a control token is configured and the request did not carry it
//	403  no control token is configured, so control is disabled outright
//	409  the controller is paused
//	413  the request named more than MaxScanPaths paths, or carried a body over MaxScanBodyBytes
//	503  the queue could not take the accepted paths
func (s *Server) handleScan(w http.ResponseWriter, r *http.Request) {
	if s.subs == nil {
		// Not reachable in the daemon: runServer wires the queue before the listener
		// exists. It is a refusal rather than a panic because the alternative - accepting
		// work nothing will process - is the silent no-op this endpoint exists to avoid.
		refuse(w, http.StatusServiceUnavailable, scanRefusal{ruleNotWired, false,
			"targeted scanning is not wired on this server; nothing was enqueued"})
		return
	}

	// Paused refuses, consistently with POST /api/rescan refusing while paused. Pause is
	// the operator saying "feed no NEW files", and a submission is new files.
	if s.ctrl.Paused() {
		refuse(w, http.StatusConflict, scanRefusal{rulePaused, true,
			"holdfast is paused, so no new file is fed to the workers; " +
				"POST /api/resume first. Nothing was enqueued."})
		return
	}

	paths, bad := readScanBody(w, r)
	if bad != nil {
		refuse(w, bad.code, bad.refusal)
		return
	}

	results := make([]scanResult, 0, len(paths))
	accept := make([]int, 0, len(paths)) // indexes into results, in submission order
	seen := map[string]bool{}
	for _, p := range paths {
		resolved, rule, detail, ok := s.subs.Judge(p)
		switch {
		case !ok:
			// Not retryable: every eligibility rule is a property of the path and of the
			// configuration, and neither changes because the same path arrives again.
			results = append(results, scanResult{Path: p, Rule: rule, Detail: detail})
		case seen[resolved]:
			results = append(results, scanResult{Path: p, Rule: ruleDuplicate, Detail: fmt.Sprintf(
				"%s was already named in this request and is accepted at most once", resolved)})
		default:
			seen[resolved] = true
			results = append(results, scanResult{Path: p, Accepted: true, Resolved: resolved})
			accept = append(accept, len(results)-1)
		}
	}

	// Offered only now, after every path has been judged, so a queue that fills partway
	// through reports exactly which paths it could not take.
	full := false
	for _, i := range accept {
		if !s.subs.Offer(results[i].Resolved) {
			full = true
			results[i].Accepted = false
			results[i].Resolved = ""
			results[i].Rule = ruleQueueFull
			// The one per-path refusal that IS retryable: the queue drains, and nothing
			// about this path was decided.
			results[i].Retryable = true
			results[i].Detail = fmt.Sprintf(
				"the submission queue is full (capacity %d); this path was NOT taken and nothing was "+
					"recorded for it - retry it", s.subs.Cap())
		}
	}

	body := scanResponse{Results: results}
	for _, res := range results {
		if res.Accepted {
			body.Accepted++
		} else {
			body.Rejected++
		}
	}
	switch {
	case full:
		// Something could not be taken. The per-path report says which, and the status
		// says the request was not fully honoured rather than leaving that to be inferred.
		// Retryable at the request level too: the paths that were not taken are named and
		// the queue drains, so a caller can resend exactly those.
		body.Rule = ruleQueueFull
		body.Retryable = true
		body.Error = fmt.Sprintf("the submission queue is full (capacity %d); the paths marked "+
			"%s were NOT taken and nothing was recorded for them", s.subs.Cap(), ruleQueueFull)
		writeJSON(w, http.StatusServiceUnavailable, body)
	case body.Accepted == 0:
		// Nothing was accepted, so every path broke a rule: a 400-class answer naming the
		// rule each one broke, rather than a 202 that took nothing.
		writeJSON(w, http.StatusBadRequest, body)
	default:
		s.log.Info("targeted scan accepted", "accepted", body.Accepted, "rejected", body.Rejected)
		writeJSON(w, http.StatusAccepted, body)
	}
}

// scanRefusal is a whole-request refusal: the request never reached the per-path stage.
// It carries the same three things every per-path refusal carries - a stable token, whether
// a retry could succeed, and the reason in words - so a caller reads one shape.
type scanRefusal struct {
	rule      string
	retryable bool
	message   string
}

// refuse writes a whole-request refusal in the endpoint's own envelope. It is JSON rather
// than http.Error's plain text for one reason: a caller that got a 400 should be able to
// branch on rule and retryable rather than parse an English sentence for them.
func refuse(w http.ResponseWriter, code int, ref scanRefusal) {
	writeJSON(w, code, scanResponse{
		Rule: ref.rule, Error: ref.message, Retryable: ref.retryable,
		Results: []scanResult{},
	})
}

// bodyRefusal pairs a refusal with the status it is answered with. readScanBody returns one
// rather than writing it, so the handler stays the only thing that answers.
type bodyRefusal struct {
	code    int
	refusal scanRefusal
}

// readScanBody parses and bounds the request body, returning the submitted paths IN
// ORDER. Every way a body can be wrong is a refusal that processes nothing and records
// nothing, and each one says what was wrong:
//
//   - not JSON at all, or not a JSON object
//   - no "paths" key, or a "paths" that is not an array
//   - an empty array, or an entry that is not a string
//   - more than MaxScanPaths entries, or a body over MaxScanBodyBytes
//
// The two maxima are named in the refusal, and stated as numbers in
// docs/api-reference.md, so "the documented maximum" is a thing a caller can read.
func readScanBody(w http.ResponseWriter, r *http.Request) ([]string, *bodyRefusal) {
	malformed := func(msg string) *bodyRefusal {
		return &bodyRefusal{http.StatusBadRequest, scanRefusal{ruleMalformedBody, false, msg}}
	}

	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxScanBodyBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return nil, &bodyRefusal{http.StatusRequestEntityTooLarge, scanRefusal{ruleBodyTooLarge, false, fmt.Sprintf(
				"request body is larger than the maximum this endpoint accepts (%d bytes); "+
					"nothing was enqueued", MaxScanBodyBytes)}}
		}
		return nil, &bodyRefusal{http.StatusBadRequest, scanRefusal{ruleUnreadableBody, false,
			"could not read the request body: " + err.Error()}}
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, malformed(`body must be a JSON object of the form {"paths": ["/library/film.mkv"]}: ` + err.Error())
	}
	list, ok := obj["paths"]
	if !ok {
		return nil, malformed(`body carries no "paths": it must be a JSON object of the form {"paths": ["/library/film.mkv"]}`)
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(list, &entries); err != nil {
		return nil, malformed(`"paths" must be an array of strings: ` + err.Error())
	}
	if len(entries) == 0 {
		return nil, malformed(`"paths" is empty: name at least one file, or do not call this endpoint`)
	}
	if len(entries) > MaxScanPaths {
		return nil, &bodyRefusal{http.StatusRequestEntityTooLarge, scanRefusal{ruleTooManyPaths, false, fmt.Sprintf(
			"request names %d paths, more than the maximum this endpoint accepts in one request (%d); "+
				"the whole request was refused and nothing was enqueued - split it",
			len(entries), MaxScanPaths)}}
	}

	paths := make([]string, 0, len(entries))
	for i, e := range entries {
		var p string
		if err := json.Unmarshal(e, &p); err != nil {
			return nil, malformed(fmt.Sprintf(
				`"paths"[%d] is not a string (%s): every entry must be a path`, i, string(e)))
		}
		paths = append(paths, p)
	}
	return paths, nil
}
