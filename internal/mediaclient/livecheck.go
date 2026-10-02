package mediaclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path"
	"regexp"
	"strings"

	"github.com/NSchatz/holdfast/internal/config"
)

// This file is the READ side of the owner's live check (scripts/client-report.sh,
// docs/client-reports.md, decision T49): what a real Plex, Sonarr or Radarr answers to the
// requests this package ships, reduced to facts that identify nobody.
//
// Every Check method sends its request through the same caller the post-swap hook and the
// play hold use - the same address, headers, credential header, timeout and failure
// classes - so a report proves the shipped path. And every one answers a CLOSED set of
// facts: an HTTP status, a failure class, counts, booleans, and strings drawn from a fixed
// vocabulary or shaped like a version number. No title, path, address, identifier or name
// a service answered is ever copied into a fact; a string outside the vocabulary becomes
// OtherValue, and a version that is not shaped like one becomes UnrecognizedVersion.
//
// What the extra requests rest on, each read 2026-10-02:
//   - Plex `GET /identity` ("Get PMS identity"): `MediaContainer.version`, "The full version
//     string of the PMS", beside a `machineIdentifier` this file never reads; the operation
//     declares `security: [{}]`, so it is answered without a token and says nothing about
//     one (https://developer.plex.tv/pms/, the OpenAPI document that page embeds).
//   - Plex section `type`: the same document's `GET /library/sections/all` example carries
//     `"type":"movie"` and `"type":"show"`. ASSUMED: `artist` and `photo` are the other two
//     values a server answers; any other value is recorded as OtherValue.
//   - Sonarr and Radarr `GET /api/v3/system/status` answer a `SystemResource` with `appName`,
//     `instanceName` and `version`, each a nullable string; `POST /api/v3/command` answers a
//     `CommandResource` whose `name` is a nullable string and whose `status` is a
//     `CommandStatus`: queued, started, completed, failed, aborted, cancelled, orphaned
//     (https://raw.githubusercontent.com/Sonarr/Sonarr/develop/src/Sonarr.Api.V3/openapi.json,
//     https://raw.githubusercontent.com/Radarr/Radarr/develop/src/Radarr.Api.V3/openapi.json).

const (
	// OtherValue stands in for a string a service answered that is not in the vocabulary a
	// fact is drawn from. The string itself is dropped.
	OtherValue = "other"
	// UnrecognizedVersion stands in for a version that is not shaped like a version number.
	UnrecognizedVersion = "unrecognized"
)

// Probe is what one request came to: the HTTP status the target answered (0 when it never
// answered) and the failure class, "" for a success.
type Probe struct {
	Status       int
	FailureClass string
}

// OK reports whether the request succeeded.
func (p Probe) OK() bool { return p.FailureClass == "" }

func probeOf(status int, f *failure) Probe {
	if f == nil {
		return Probe{Status: status}
	}
	if f.status != 0 {
		status = f.status
	}
	return Probe{Status: status, FailureClass: f.class}
}

// commandOutcome is what the two requests of a rescan came to: the lookup that finds the
// owner, whether one was found, whether the write request was sent, and the status it was
// answered with.
type commandOutcome struct {
	lookup     Probe
	ownerFound bool
	requested  bool
	status     int
}

// versionShape is the only form a version is copied in: dotted decimal groups with at most
// one short alphanumeric suffix ("1.40.2.8395-c67dce28e", "4.0.20.3014"). A host name, a
// path, an address with a port and an identifier all fail it.
var versionShape = regexp.MustCompile(`^[0-9]{1,5}(\.[0-9]{1,6}){1,4}([-+][0-9A-Za-z]{1,16})?$`)

// cleanVersion answers a version only when it is shaped like one.
func cleanVersion(s string) string {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return ""
	case versionShape.MatchString(s):
		return s
	}
	return UnrecognizedVersion
}

// oneOf answers s lowercased when it is in the vocabulary, "" when it is empty, and
// OtherValue otherwise.
func oneOf(s string, vocabulary ...string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return ""
	}
	for _, v := range vocabulary {
		if s == strings.ToLower(v) {
			return v
		}
	}
	return OtherValue
}

// The vocabularies. Vocabulary returns their union, which is every string a fact can carry
// that is not a version.
var (
	sectionTypes    = []string{"movie", "show", "artist", "photo"}
	appNames        = []string{"sonarr", "radarr"}
	commandNames    = []string{radarrKind.command, sonarrKind.command}
	commandStatuses = []string{"queued", "started", "completed", "failed", "aborted", "cancelled", "orphaned"}
	failureClasses  = []string{ClassTimeout, ClassUnreachable, ClassUnauthorized, ClassStatus, ClassUnparseable, ClassRefused}
)

// Vocabulary is every string a fact of this file can carry besides a version: the failure
// classes, the section types, the application names, the command names and statuses, and the
// two stand-ins. The report's final scan reads it to tell this build's own words from
// anything a service said.
func Vocabulary() []string {
	out := []string{OtherValue, UnrecognizedVersion}
	for _, set := range [][]string{sectionTypes, appNames, commandNames, commandStatuses, failureClasses} {
		out = append(out, set...)
	}
	return out
}

// PlexIdentity is what `GET /identity` answered: the server's version and nothing else.
type PlexIdentity struct {
	Probe
	Version string
}

// CheckIdentity asks the server its version. The request needs no token, so its success
// says nothing about the token.
func (p *Plex) CheckIdentity(ctx context.Context) PlexIdentity {
	var body struct {
		MediaContainer *struct {
			Version string `json:"version"`
		} `json:"MediaContainer"`
	}
	status, f := p.call.send(ctx, "GET", "/identity", nil, func(r io.Reader) error {
		if err := decodeObject(r, &body); err != nil {
			return err
		}
		if body.MediaContainer == nil {
			return errNoContainer
		}
		return nil
	})
	out := PlexIdentity{Probe: probeOf(status, f)}
	if f == nil {
		out.Version = cleanVersion(body.MediaContainer.Version)
	}
	return out
}

// PlexSection is one library section, as facts.
type PlexSection struct {
	// Type is movie, show, artist, photo, OtherValue or "".
	Type string
	// Locations is how many Location entries the section carries.
	Locations int
	// EveryLocationHasPath is true when each of them carries a non-empty `path`.
	EveryLocationHasPath bool
	// KeyIsSectionNumber is true when the section's key is one this client would put into a
	// refresh request.
	KeyIsSectionNumber bool
}

// PlexSections is what `GET /library/sections/all` answered, as facts.
type PlexSections struct {
	Probe
	Sections []PlexSection
	// Roots is how many library roots were asked about; RootsInsideALocation is how many of
	// them, through the path map, are a section location or lie inside one;
	// RootsContainingALocation is how many are a section location or have one beneath them.
	Roots, RootsInsideALocation, RootsContainingALocation int
}

// CheckSections lists the library sections and says how the configured library roots (as
// holdfast sees them) relate to their locations through the path map.
func (p *Plex) CheckSections(ctx context.Context, roots []string) PlexSections {
	var sections plexSections
	status, f := p.call.send(ctx, "GET", "/library/sections/all", nil, func(r io.Reader) error {
		if err := decodeObject(r, &sections); err != nil {
			return err
		}
		if sections.MediaContainer == nil {
			return errNoContainer
		}
		return nil
	})
	out := PlexSections{Probe: probeOf(status, f), Roots: len(roots)}
	if f != nil {
		return out
	}
	var locations []string
	for _, d := range sections.MediaContainer.Directory {
		s := PlexSection{
			Type:                 oneOf(d.Type, sectionTypes...),
			Locations:            len(d.Location),
			EveryLocationHasPath: true,
			KeyIsSectionNumber:   isSectionNumber(sectionKey(d.Key)),
		}
		for _, loc := range d.Location {
			if loc.Path == "" {
				s.EveryLocationHasPath = false
				continue
			}
			locations = append(locations, loc.Path)
		}
		out.Sections = append(out.Sections, s)
	}
	out.RootsInsideALocation, out.RootsContainingALocation = relate(p.paths, roots, locations)
	return out
}

// relate counts the roots (holdfast's view) that, mapped, are one of the target's paths or
// lie inside one, and the roots that are one of them or contain one. Only absolute paths
// take part, on whole path components.
func relate(paths config.PathMap, roots, targetPaths []string) (inside, containing int) {
	for _, root := range roots {
		if root == "" || root[0] != '/' {
			continue
		}
		mapped := paths.Map(root)
		in, has := false, false
		for _, tp := range targetPaths {
			if tp == "" || tp[0] != '/' {
				continue
			}
			tp = path.Clean(tp)
			if _, ok := config.UnderPrefix(mapped, tp); ok {
				in = true
			}
			if _, ok := config.UnderPrefix(tp, mapped); ok {
				has = true
			}
		}
		if in {
			inside++
		}
		if has {
			containing++
		}
	}
	return inside, containing
}

// PlexSessionsShape is what `GET /status/sessions` answered, as counts. It settles what the
// play hold assumes: that a session's parts carry `file`.
type PlexSessionsShape struct {
	Probe
	// Sessions is how many playbacks the server listed.
	Sessions int
	// SessionsWithMedia and SessionsWithPart count the sessions carrying at least one Media
	// entry, and at least one Part.
	SessionsWithMedia, SessionsWithPart int
	// SessionsEveryPartHasFile counts the sessions with at least one part, every one of which
	// carries an absolute `file`: the sessions the play hold can act on in full.
	SessionsEveryPartHasFile int
	// Parts and PartsWithFile count the parts across every session, and those carrying an
	// absolute `file`.
	Parts, PartsWithFile int
}

// CheckSessions lists the current playbacks and counts what each one carries.
func (p *Plex) CheckSessions(ctx context.Context) PlexSessionsShape {
	var sessions plexSessions
	status, f := p.call.send(ctx, "GET", "/status/sessions", nil, func(r io.Reader) error {
		if err := decodeObject(r, &sessions); err != nil {
			return err
		}
		if sessions.MediaContainer == nil {
			return errNoContainer
		}
		return nil
	})
	out := PlexSessionsShape{Probe: probeOf(status, f)}
	if f != nil {
		return out
	}
	for _, item := range sessions.MediaContainer.Metadata {
		out.Sessions++
		parts, withFile := 0, 0
		for _, media := range item.Media {
			for _, part := range media.Part {
				parts++
				if part.File != "" && part.File[0] == '/' {
					withFile++
				}
			}
		}
		if len(item.Media) > 0 {
			out.SessionsWithMedia++
		}
		if parts > 0 {
			out.SessionsWithPart++
			if withFile == parts {
				out.SessionsEveryPartHasFile++
			}
		}
		out.Parts += parts
		out.PartsWithFile += withFile
	}
	return out
}

// WriteCheck is what one deliberate rescan request came to.
type WriteCheck struct {
	// Lookup is the read that finds the owner of the directory.
	Lookup Probe
	// OwnerFound is true when exactly one owner was found, so a request could be made.
	OwnerFound bool
	// Requested is true when the write request was sent; Request is what it came to.
	Requested bool
	Request   Probe
	// Accepted is true when the target answered the write request 2xx.
	Accepted bool
	// CommandName and CommandStatus are the `name` and `status` of the command Radarr or
	// Sonarr answered with, each from its vocabulary; AnswerParsed is false when the answer
	// was not a JSON object. Plex answers a refresh with no body, so all three stay zero.
	CommandName, CommandStatus string
	AnswerParsed               bool
}

// writeCheck turns a rescan's result into its facts.
func writeCheck(res Result, cmd commandOutcome) WriteCheck {
	out := WriteCheck{Lookup: cmd.lookup, OwnerFound: cmd.ownerFound, Requested: cmd.requested, Accepted: res.Sent}
	if cmd.ownerFound {
		// Past the lookup, a failure is the write request's: one that was sent and failed, or
		// one this build refused to build.
		out.Request = probeOf(cmd.status, res.Failure)
	}
	return out
}

// CheckRefresh sends the ONE partial refresh Rescan would send for dir (a directory as
// holdfast sees it) and says what it came to. It is Rescan: the same lookup, the same
// refusal of anything that is not one section and one directory.
func (p *Plex) CheckRefresh(ctx context.Context, dir string) WriteCheck {
	return writeCheck(p.rescan(ctx, dir))
}

// ArrStatus is what `GET /api/v3/system/status` answered: the version and which of the two
// applications answered.
type ArrStatus struct {
	Probe
	Version string
	// App is sonarr, radarr, OtherValue or "".
	App string
}

// CheckStatus asks the application its version and name.
func (a *Arr) CheckStatus(ctx context.Context) ArrStatus {
	var body struct {
		AppName string `json:"appName"`
		Version string `json:"version"`
	}
	status, f := a.call.send(ctx, "GET", "/api/v3/system/status", nil, func(r io.Reader) error {
		return decodeObject(r, &body)
	})
	out := ArrStatus{Probe: probeOf(status, f)}
	if f == nil {
		out.Version = cleanVersion(body.Version)
		out.App = oneOf(body.AppName, appNames...)
	}
	return out
}

// ArrLibrary is what the list endpoint answered, as counts.
type ArrLibrary struct {
	Probe
	// Items is how many movies or series were listed.
	Items int
	// ItemsWithIntegerID and ItemsWithStringPath count the items whose `id` is a JSON
	// integer and whose `path` is a JSON string; ItemsUsable counts the items this client
	// can own a directory with: a positive id and an absolute path. ItemsTheClientReads
	// counts the items the shipped decoding accepts; one it does not makes the whole list
	// unparseable to the post-swap rescan.
	ItemsWithIntegerID, ItemsWithStringPath, ItemsUsable, ItemsTheClientReads int
	// Roots is how many library roots were asked about; RootsContainingAnItem is how many of
	// them, through the path map, are an item's path or have one beneath them;
	// RootsInsideAnItem is how many are an item's path or lie inside one.
	Roots, RootsContainingAnItem, RootsInsideAnItem int
}

// CheckLibrary lists every movie or series and says what each carries and how the configured
// library roots (as holdfast sees them) relate to the items' paths through the path map.
func (a *Arr) CheckLibrary(ctx context.Context, roots []string) ArrLibrary {
	out := ArrLibrary{Roots: len(roots)}
	var itemPaths []string
	status, f := a.call.send(ctx, "GET", a.kind.listPath, nil, func(r io.Reader) error {
		return eachElement(r, func(raw json.RawMessage) {
			out.Items++
			if json.Unmarshal(raw, new(arrOwner)) == nil {
				out.ItemsTheClientReads++
			}
			var item struct {
				ID   json.RawMessage `json:"id"`
				Path json.RawMessage `json:"path"`
			}
			if json.Unmarshal(raw, &item) != nil {
				return
			}
			var id int
			hasID := len(item.ID) > 0 && json.Unmarshal(item.ID, &id) == nil && string(item.ID) != "null"
			var p string
			hasPath := len(item.Path) > 0 && json.Unmarshal(item.Path, &p) == nil && string(item.Path) != "null"
			if hasID {
				out.ItemsWithIntegerID++
			}
			if hasPath {
				out.ItemsWithStringPath++
			}
			if hasID && id > 0 && hasPath && p != "" && p[0] == '/' {
				out.ItemsUsable++
				itemPaths = append(itemPaths, p)
			}
		})
	})
	out.Probe = probeOf(status, f)
	if f != nil {
		return ArrLibrary{Probe: out.Probe, Roots: len(roots)}
	}
	out.RootsInsideAnItem, out.RootsContainingAnItem = relate(a.paths, roots, itemPaths)
	return out
}

// eachElement reads a JSON array one element at a time and hands each to fn, so a library of
// many thousand items is never held as one decoded document.
func eachElement(r io.Reader, fn func(json.RawMessage)) error {
	dec := json.NewDecoder(r)
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return errors.New("the list is not a JSON array")
	}
	for dec.More() {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return err
		}
		fn(raw)
	}
	_, err = dec.Token()
	return err
}

// CheckRescan sends the ONE rescan command Rescan would send for dir (a directory as holdfast
// sees it) and says what it came to. It is Rescan: the same owner lookup, the same refusal
// to build a command with no id.
func (a *Arr) CheckRescan(ctx context.Context, dir string) WriteCheck {
	var answer struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	parsed := false
	res, cmd := a.rescan(ctx, dir, func(r io.Reader) error {
		// An answer this build cannot read is a fact about the answer, not a failed command:
		// the command was accepted the moment the target answered 2xx.
		parsed = decodeObject(r, &answer) == nil
		return nil
	})
	out := writeCheck(res, cmd)
	if out.Accepted && parsed {
		out.AnswerParsed = true
		out.CommandName = oneOf(answer.Name, commandNames...)
		out.CommandStatus = oneOf(answer.Status, commandStatuses...)
	}
	return out
}

// IsVersion reports whether s is shaped like a version number, the only form a version is
// copied into a fact in.
func IsVersion(s string) bool { return versionShape.MatchString(s) }
