package mediaclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"path"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/secret"
)

// Plex is the client for one Plex Media Server.
//
// What it rests on is the server's own reference, https://developer.plex.tv/pms/ (the
// OpenAPI document that page embeds, "Plex Media Server" API version 1.2.3, read
// 2026-10-02):
//
//   - The token travels in the `X-Plex-Token` HEADER: `securitySchemes.user_token` is
//     `{type: apiKey, in: header, name: X-Plex-Token}`. It is never put in a URL here.
//     `X-Plex-Client-Identifier` ("An opaque identifier unique to the client") is sent beside
//     it, and `Accept: application/json` selects the JSON the reference documents.
//   - `POST /library/sections/{sectionId}/refresh` ("Refresh section") takes the query
//     parameter `path`, "Restrict refresh to the specified path", and declares
//     `security: [{user_token: [admin]}]`. The path has a `post` and a `delete` and no `get`.
//   - `GET /library/sections/all` ("Get library sections") answers
//     `MediaContainer.Directory[]`, each a section with a `key` (a string; "1" in the
//     reference's example) and `Location[]` entries whose `path` is "The path of where this
//     directory exists on disk". The reference lists no bare `/library/sections`, which is
//     the spelling python-plexapi uses, so the documented one is the one requested.
//   - `GET /status/sessions` ("List all current playbacks on this server") answers
//     `MediaContainer.Metadata[]`, each the `metadata` schema with `Media[]`, each with
//     `Part[]`, whose `file` is "The local file path at which the part is stored on the
//     server". It declares the same admin scope.
//
// ASSUMED, because the reference does not settle them and no live server was asked (this
// program never contacts one):
//   - that a real session item actually CARRIES `Media[].Part[].file`. The schema gives a
//     part that property and says no attribute of a media element is guaranteed; the
//     reference's own session example elides the part's details. A session that carries no
//     file names no path, so it holds nothing.
//   - that `sectionId` in the refresh path is the section's `key`. The reference types the
//     parameter as an integer and the key as a string; a key that is not a positive decimal
//     integer is refused rather than put into a URL.
//   - XML. A server answers XML when no JSON is asked for; this client asks for JSON and
//     reads only JSON, because the XML shape is not in the reference.
//
// Both operations need an ADMIN token (the server owner's): each declares the admin scope,
// and a token without it is answered 401 or 403, which is reported as `unauthorized`.
type Plex struct {
	call  *caller
	paths config.PathMap
}

// plexClientIdentifier is the opaque `X-Plex-Client-Identifier` this client sends. ASSUMED
// sufficient: the reference asks for an identifier unique to the client and says nothing
// about its form.
const plexClientIdentifier = "holdfast"

// NewPlex builds the Plex client from its address, its RESOLVED token and its path map.
func NewPlex(baseURL string, token secret.Value, paths config.PathMap) *Plex {
	return &Plex{paths: paths, call: newCaller(baseURL, "X-Plex-Token", token, map[string]string{
		"X-Plex-Client-Identifier": plexClientIdentifier,
		"Accept":                   "application/json",
	})}
}

// Name is plex.
func (p *Plex) Name() string { return "plex" }

// plexSections is the part of `GET /library/sections/all` this client reads.
type plexSections struct {
	MediaContainer *struct {
		Directory []struct {
			Key      json.RawMessage `json:"key"`
			Location []struct {
				Path string `json:"path"`
			} `json:"Location"`
		} `json:"Directory"`
	} `json:"MediaContainer"`
}

// Rescan asks Plex for a PARTIAL scan of dir (a directory as holdfast sees it): a refresh of
// the one section whose location owns the mapped directory, restricted to that directory. It
// never requests a refresh with no path, which would rescan a whole section.
func (p *Plex) Rescan(ctx context.Context, dir string) Result {
	mapped := p.paths.Map(dir)
	res := Result{Dir: mapped, Attempted: "GET /library/sections/all to find the section that owns the directory"}

	var sections plexSections
	if f := p.call.do(ctx, "GET", "/library/sections/all", nil, func(r io.Reader) error {
		if err := decodeObject(r, &sections); err != nil {
			return err
		}
		if sections.MediaContainer == nil {
			return errNoContainer
		}
		return nil
	}); f != nil {
		res.Failure = f
		return res
	}
	key, found := owningSection(sections, mapped)
	if !found {
		res.NoOwner = true
		return res
	}
	res.Attempted = "POST /library/sections/" + key + "/refresh restricted to the directory"
	target, err := refreshTarget(key, mapped)
	if err != nil {
		res.Failure = &failure{class: ClassRefused}
		return res
	}
	if f := p.call.do(ctx, "POST", target, nil, nil); f != nil {
		res.Failure = f
		return res
	}
	res.Sent = true
	return res
}

// errUnspecificRefresh is why refreshTarget refuses: the request would not be a partial scan.
var errUnspecificRefresh = errors.New("a refresh must name one section by number and one absolute directory")

// refreshTarget is the ONE place a refresh request is built, and it cannot build a
// whole-section or all-sections one: the section is a positive decimal number (never `all`)
// and the path restriction is a non-empty absolute directory.
func refreshTarget(sectionKey, dir string) (string, error) {
	if !isSectionNumber(sectionKey) || dir == "" || dir[0] != '/' {
		return "", errUnspecificRefresh
	}
	return "/library/sections/" + sectionKey + "/refresh?path=" + url.QueryEscape(dir), nil
}

// isSectionNumber reports whether s is a positive decimal integer with no sign and no
// leading zero, which is the only section key this client will put into a URL.
func isSectionNumber(s string) bool {
	if s == "" || s[0] == '0' || len(s) > 9 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// owningSection picks the section with the LONGEST location path that owns dir on a
// whole-component boundary. A section whose key is not a section number owns nothing, and
// two different sections tied on the winning location are an answer this build cannot choose
// between, so neither is the owner.
func owningSection(s plexSections, dir string) (key string, found bool) {
	bestLen, tied := -1, false
	for _, d := range s.MediaContainer.Directory {
		k := sectionKey(d.Key)
		if !isSectionNumber(k) {
			continue
		}
		for _, loc := range d.Location {
			if loc.Path == "" || loc.Path[0] != '/' {
				continue
			}
			p := path.Clean(loc.Path)
			if _, ok := config.UnderPrefix(dir, p); !ok {
				continue
			}
			switch {
			case len(p) > bestLen:
				key, bestLen, tied = k, len(p), false
			case len(p) == bestLen && k != key:
				tied = true
			}
		}
	}
	return key, bestLen >= 0 && !tied
}

// sectionKey reads a section's `key`, which the reference types as a string and which is
// accepted as a bare number too. Anything else is "".
func sectionKey(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err == nil {
		return n.String()
	}
	return ""
}

// plexSessions is the part of `GET /status/sessions` this client reads.
type plexSessions struct {
	MediaContainer *struct {
		Metadata []struct {
			Media []struct {
				Part []struct {
					File string `json:"file"`
				} `json:"Part"`
			} `json:"Media"`
		} `json:"Metadata"`
	} `json:"MediaContainer"`
}

// Playing asks Plex which files are being played right now and answers them as holdfast sees
// them: each session part's `file`, mapped back through the path map and cleaned. A session
// that names no absolute file contributes nothing. ok is false when Plex could not be asked
// or its answer could not be read, and class then says why.
func (p *Plex) Playing(ctx context.Context) (files map[string]bool, class string, ok bool) {
	var sessions plexSessions
	if f := p.call.do(ctx, "GET", "/status/sessions", nil, func(r io.Reader) error {
		if err := decodeObject(r, &sessions); err != nil {
			return err
		}
		if sessions.MediaContainer == nil {
			return errNoContainer
		}
		return nil
	}); f != nil {
		return nil, f.String(), false
	}
	files = map[string]bool{}
	for _, item := range sessions.MediaContainer.Metadata {
		for _, media := range item.Media {
			for _, part := range media.Part {
				if part.File == "" || part.File[0] != '/' {
					continue
				}
				files[p.paths.Reverse(part.File)] = true
			}
		}
	}
	return files, "", true
}

// errNoContainer is an answer that parsed as JSON and is not a Plex answer: every documented
// response is one `MediaContainer` object.
var errNoContainer = errors.New("the answer carries no MediaContainer")

// decodeObject reads one JSON document into v and refuses anything after it.
func decodeObject(r io.Reader, v any) error {
	dec := json.NewDecoder(r)
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("the answer carries more than one JSON document")
	}
	return nil
}
