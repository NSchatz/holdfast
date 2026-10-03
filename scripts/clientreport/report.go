package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/NSchatz/holdfast/internal/mediaclient"
)

// Schema is the report's schema number. A change to any field below moves it.
const Schema = 1

// The service kinds.
const (
	servicePlex   = "plex"
	serviceSonarr = "sonarr"
	serviceRadarr = "radarr"
)

// The request labels: the method and path of each request a report can name. They are this
// build's constants; a report never carries a request line a service or a configuration
// supplied (a refresh's `path` query and a base path are not part of a label).
const (
	reqPlexIdentity = "GET /identity"
	reqPlexSections = "GET /library/sections/all"
	reqPlexSessions = "GET /status/sessions"
	reqPlexRefresh  = "POST /library/sections/{section}/refresh?path={directory}"
	reqArrStatus    = "GET /api/v3/system/status"
	reqSonarrList   = "GET /api/v3/series"
	reqRadarrList   = "GET /api/v3/movie"
	reqArrCommand   = "POST /api/v3/command"
)

var requestLabels = []string{reqPlexIdentity, reqPlexSections, reqPlexSessions, reqPlexRefresh,
	reqArrStatus, reqSonarrList, reqRadarrList, reqArrCommand}

const (
	dateLayout   = "2006-01-02"
	unrecognized = mediaclient.UnrecognizedVersion
	unknown      = "unknown"
)

var (
	// buildVersionShape is this build's own version: `0.0.0-dev`, `v0.3.1`.
	buildVersionShape = regexp.MustCompile(`^v?[0-9]{1,5}(\.[0-9]{1,6}){1,3}([-+][0-9A-Za-z.+-]{1,64})?$`)
	// commitShape is a git revision, or the `unknown` of a build nobody stamped.
	commitShape = regexp.MustCompile(`^([0-9a-f]{7,40}|unknown)$`)
	dateShape   = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)
)

// shaped answers s when it has the shape, and `unrecognized` otherwise.
func shaped(shape *regexp.Regexp, s string) string {
	if shape.MatchString(s) {
		return s
	}
	return unrecognized
}

// Report is the whole of a client report. It is CLOSED: every field is a number, a boolean,
// or a string that is one of this build's own words or shaped like a version, a commit or a
// date. There is no map, no raw JSON and no field a service's own text could pass through.
type Report struct {
	Schema   int         `json:"schema"`
	Service  string      `json:"service"`
	Date     string      `json:"date"`
	Holdfast Build       `json:"holdfast"`
	Config   ConfigFacts `json:"config"`
	// CredentialAccepted is false when a request that needs the credential was answered 401
	// or 403, true when one succeeded, and null when the service was never reached.
	CredentialAccepted *bool       `json:"credential_accepted"`
	Plex               *PlexReport `json:"plex,omitempty"`
	Arr                *ArrReport  `json:"arr,omitempty"`
}

// Build is the holdfast build that ran the check.
type Build struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

// ConfigFacts is what the configuration said about the target, as facts: never the address.
type ConfigFacts struct {
	URLScheme      string `json:"url_scheme"`
	URLHasBasePath bool   `json:"url_has_base_path"`
	PathMapEntries int    `json:"path_map_entries"`
	LibraryRoots   int    `json:"library_roots"`
}

// Request is one request and what it came to.
type Request struct {
	Request      string `json:"request"`
	OK           bool   `json:"ok"`
	Status       int    `json:"status"`
	FailureClass string `json:"failure_class"`
}

// PlexReport is the Plex check.
type PlexReport struct {
	Identity PlexIdentity `json:"identity"`
	Sections PlexSections `json:"sections"`
	Sessions PlexSessions `json:"sessions"`
	Refresh  *WriteReport `json:"refresh,omitempty"`
}

// PlexIdentity is the server's version.
type PlexIdentity struct {
	Request
	Version string `json:"version"`
}

// PlexSections is the library sections, as counts and booleans.
type PlexSections struct {
	Request
	Count                        int           `json:"count"`
	Sections                     []PlexSection `json:"sections"`
	EveryLocationCarriesAPath    bool          `json:"every_location_carries_a_path"`
	EverySectionKeyIsANumber     bool          `json:"every_section_key_is_a_number"`
	LibraryRootsInsideALocation  int           `json:"library_roots_inside_a_location"`
	LibraryRootsContainingOne    int           `json:"library_roots_containing_a_location"`
	ALibraryRootMapsIntoASection bool          `json:"a_library_root_maps_into_a_section"`
}

// PlexSection is one section.
type PlexSection struct {
	Type                      string `json:"type"`
	Locations                 int    `json:"locations"`
	EveryLocationCarriesAPath bool   `json:"every_location_carries_a_path"`
	KeyIsANumber              bool   `json:"key_is_a_number"`
}

// PlexSessions is the current playbacks, as counts: whether a session carries
// `Metadata[].Media[].Part[].file`, which the play hold reads.
type PlexSessions struct {
	Request
	Count                    int `json:"count"`
	SessionsWithMedia        int `json:"sessions_with_media"`
	SessionsWithPart         int `json:"sessions_with_part"`
	SessionsEveryPartHasFile int `json:"sessions_every_part_has_file"`
	Parts                    int `json:"parts"`
	PartsWithFile            int `json:"parts_with_file"`
	// EverySessionCarriesPartFile is null when no session was playing: nothing was observed.
	EverySessionCarriesPartFile *bool `json:"every_session_carries_part_file"`
}

// ArrReport is the Sonarr or Radarr check.
type ArrReport struct {
	Status  ArrStatus    `json:"status"`
	Library ArrLibrary   `json:"library"`
	Rescan  *WriteReport `json:"rescan,omitempty"`
}

// ArrStatus is the application's version and name.
type ArrStatus struct {
	Request
	Version string `json:"version"`
	App     string `json:"app"`
}

// ArrLibrary is the series or movie list, as counts and booleans.
type ArrLibrary struct {
	Request
	Count                         int  `json:"count"`
	ItemsWithIntegerID            int  `json:"items_with_integer_id"`
	ItemsWithStringPath           int  `json:"items_with_string_path"`
	ItemsUsable                   int  `json:"items_with_positive_id_and_absolute_path"`
	ItemsTheClientReads           int  `json:"items_the_client_reads"`
	EveryItemHasIntegerIDAndPath  bool `json:"every_item_has_integer_id_and_string_path"`
	LibraryRootsContainingAnItem  int  `json:"library_roots_containing_an_item"`
	LibraryRootsInsideAnItem      int  `json:"library_roots_inside_an_item"`
	ALibraryRootMapsOntoTheirPath bool `json:"a_library_root_maps_onto_an_item_path"`
}

// WriteReport is the one write check, present only when the owner asked for it.
type WriteReport struct {
	Lookup     Request `json:"lookup"`
	OwnerFound bool    `json:"owner_found"`
	Sent       bool    `json:"sent"`
	// Request is null when no owner was found, so nothing was built or sent.
	Request       *Request `json:"request"`
	Accepted      bool     `json:"accepted"`
	AnswerParsed  bool     `json:"answer_parsed"`
	CommandName   string   `json:"command_name"`
	CommandStatus string   `json:"command_status"`
}

// encode is the one way a report becomes bytes.
func encode(r *Report) ([]byte, error) {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// ownWords is every string this build itself can put into a report: the vocabulary of the
// clients' facts, the service kinds, the request labels and the two URL schemes.
func ownWords() map[string]bool {
	words := map[string]bool{"": true, servicePlex: true, serviceSonarr: true, serviceRadarr: true,
		"http": true, "https": true, unknown: true}
	for _, w := range mediaclient.Vocabulary() {
		words[w] = true
	}
	for _, w := range requestLabels {
		words[w] = true
	}
	return words
}

// foreignStrings is every string VALUE in an encoded report that is not one of this build's
// own words: the versions, the commit and the date.
func foreignStrings(encoded []byte) ([]string, error) {
	var doc any
	if err := json.Unmarshal(encoded, &doc); err != nil {
		return nil, errors.New("it is not JSON")
	}
	own := ownWords()
	var out []string
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			if !own[x] {
				out = append(out, x)
			}
		case []any:
			for _, e := range x {
				walk(e)
			}
		case map[string]any:
			for _, e := range x {
				walk(e)
			}
		}
	}
	walk(doc)
	return out, nil
}

// Verify is the identity check every report passes before it is written, and the one
// `make check` runs over every report committed under testdata/client-reports/. It holds a
// report to the closed struct: no field the struct does not have, every string one of this
// build's own words in the field it belongs to or shaped like a version, a commit or a date,
// and the bytes exactly what encode writes, so a report edited by hand does not pass.
func Verify(encoded []byte) error {
	dec := json.NewDecoder(bytes.NewReader(encoded))
	dec.DisallowUnknownFields()
	var r Report
	if err := dec.Decode(&r); err != nil {
		return errors.New("it is not a report this build knows: a field is unknown or has the wrong type")
	}
	if dec.More() {
		return errors.New("it carries more than one JSON document")
	}
	if again, err := encode(&r); err != nil || !bytes.Equal(again, encoded) {
		return errors.New("its bytes are not what this build writes for its content (a report is never edited by hand)")
	}
	if r.Schema != Schema {
		return fmt.Errorf("its schema is not %d", Schema)
	}
	if _, err := time.Parse(dateLayout, r.Date); err != nil || !dateShape.MatchString(r.Date) {
		return errors.New("its date is not a calendar date")
	}
	if r.Holdfast.Version != unrecognized && !buildVersionShape.MatchString(r.Holdfast.Version) {
		return errors.New("its holdfast version is not shaped like one")
	}
	if r.Holdfast.Commit != unrecognized && !commitShape.MatchString(r.Holdfast.Commit) {
		return errors.New("its holdfast commit is not shaped like one")
	}
	if !in(r.Config.URLScheme, "", "http", "https") {
		return errors.New("its url scheme is not http or https")
	}

	var requests []Request
	var versions []string
	var writes []*WriteReport
	switch {
	case r.Service == servicePlex && r.Plex != nil && r.Arr == nil:
		requests = append(requests, r.Plex.Identity.Request, r.Plex.Sections.Request, r.Plex.Sessions.Request)
		versions = append(versions, r.Plex.Identity.Version)
		writes = append(writes, r.Plex.Refresh)
		if r.Plex.Sections.Sections == nil {
			return errors.New("its section list is missing")
		}
		for _, s := range r.Plex.Sections.Sections {
			if !in(s.Type, "", mediaclient.OtherValue, "movie", "show", "artist", "photo") {
				return errors.New("a section type is not one a report may carry")
			}
		}
	case (r.Service == serviceSonarr || r.Service == serviceRadarr) && r.Arr != nil && r.Plex == nil:
		requests = append(requests, r.Arr.Status.Request, r.Arr.Library.Request)
		versions = append(versions, r.Arr.Status.Version)
		writes = append(writes, r.Arr.Rescan)
		if !in(r.Arr.Status.App, "", mediaclient.OtherValue, serviceSonarr, serviceRadarr) {
			return errors.New("its application name is not one a report may carry")
		}
	default:
		return errors.New("its service and the check it carries do not agree")
	}
	for _, w := range writes {
		if w == nil {
			continue
		}
		requests = append(requests, w.Lookup)
		if w.Request != nil {
			requests = append(requests, *w.Request)
		}
		if !in(w.CommandName, "", mediaclient.OtherValue, "RescanSeries", "RescanMovie") ||
			!in(w.CommandStatus, "", mediaclient.OtherValue, "queued", "started", "completed", "failed", "aborted", "cancelled", "orphaned") {
			return errors.New("a command name or status is not one a report may carry")
		}
	}
	for _, q := range requests {
		if !in(q.Request, requestLabels...) {
			return errors.New("a request is not one this build sends")
		}
		if !in(q.FailureClass, "", mediaclient.ClassTimeout, mediaclient.ClassUnreachable, mediaclient.ClassUnauthorized,
			mediaclient.ClassStatus, mediaclient.ClassUnparseable, mediaclient.ClassRefused) {
			return errors.New("a failure class is not one this build reports")
		}
		if q.OK != (q.FailureClass == "") || (q.Status != 0 && (q.Status < 100 || q.Status > 599)) {
			return errors.New("a request's outcome does not agree with itself")
		}
	}
	for _, v := range versions {
		if !in(v, "", unrecognized) && !mediaclient.IsVersion(v) {
			return errors.New("a version is not shaped like one")
		}
	}

	// The backstop, which knows nothing about fields: every string value anywhere in the
	// document is one of this build's own words or has one of the four shapes.
	foreign, err := foreignStrings(encoded)
	if err != nil {
		return err
	}
	for _, s := range foreign {
		if !mediaclient.IsVersion(s) && !buildVersionShape.MatchString(s) && !commitShape.MatchString(s) && !dateShape.MatchString(s) {
			return errors.New("it carries a string that is neither one of this build's words nor a version, a commit or a date")
		}
	}
	return nil
}

func in(s string, set ...string) bool {
	for _, v := range set {
		if s == v {
			return true
		}
	}
	return false
}
