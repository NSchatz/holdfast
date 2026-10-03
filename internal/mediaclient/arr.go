package mediaclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/secret"
)

// Arr is the client for one Radarr or Sonarr: the two share one API shape (v3) and differ in
// three names, which is all an arrKind carries.
//
// What it rests on, each read 2026-10-02:
//   - `X-Api-Key` "Apikey passed as header" is a security scheme of both v3 APIs, beside an
//     `apikey` query parameter this client never uses
//     (https://raw.githubusercontent.com/Sonarr/Sonarr/develop/src/Sonarr.Api.V3/openapi.json,
//     https://raw.githubusercontent.com/Radarr/Radarr/develop/src/Radarr.Api.V3/openapi.json).
//   - `GET /api/v3/series` and `GET /api/v3/movie` answer an array of resources whose `id` is
//     an int32 and whose `path` is a string (same two documents: SeriesResource,
//     MovieResource).
//   - `POST /api/v3/command` takes `{"name": ..., ...}`; `RescanSeriesCommand` has
//     `public int? SeriesId` and `RescanMovieCommand` has `public int? MovieId`
//     (src/NzbDrone.Core/MediaFiles/Commands/ in each repository, checked at the releases
//     v4.0.20.3014 and v6.4.4.10685). The id is NULLABLE there, and a command with none
//     rescans every series or movie, which is why this client cannot build one.
//   - ASSUMED: the JSON spelling of those two properties is camelCase (`seriesId`,
//     `movieId`), as every property in both openapi documents is; no source read here shows
//     the command body itself.
type Arr struct {
	kind  arrKind
	call  *caller
	paths config.PathMap
}

// arrKind is what differs between the two applications.
type arrKind struct {
	name     string // the target's name in a record
	listPath string // the endpoint that lists every owner
	command  string // the rescan command's name
	idField  string // the JSON property the owner's id travels in
	owner    string // what an owner is called in a record
}

var (
	radarrKind = arrKind{name: "radarr", listPath: "/api/v3/movie", command: "RescanMovie", idField: "movieId", owner: "movie"}
	sonarrKind = arrKind{name: "sonarr", listPath: "/api/v3/series", command: "RescanSeries", idField: "seriesId", owner: "series"}
)

// NewRadarr builds the Radarr client from its address, its RESOLVED api key and its path map.
func NewRadarr(baseURL string, key secret.Value, paths config.PathMap) *Arr {
	return newArr(radarrKind, baseURL, key, paths)
}

// NewSonarr builds the Sonarr client from its address, its RESOLVED api key and its path map.
func NewSonarr(baseURL string, key secret.Value, paths config.PathMap) *Arr {
	return newArr(sonarrKind, baseURL, key, paths)
}

func newArr(kind arrKind, baseURL string, key secret.Value, paths config.PathMap) *Arr {
	return &Arr{kind: kind, paths: paths,
		call: newCaller(baseURL, "X-Api-Key", key, map[string]string{"Accept": "application/json"})}
}

// Name is radarr or sonarr.
func (a *Arr) Name() string { return a.kind.name }

// arrOwner is the two properties this client reads of a movie or a series. ID is a pointer
// so an item with no id at all is told apart from one whose id is 0; neither is ever sent.
type arrOwner struct {
	ID   *int   `json:"id"`
	Path string `json:"path"`
}

// Rescan asks the movie or series that owns dir (a directory as holdfast sees it) to rescan.
// The owner is the item whose path is the mapped directory or its NEAREST ancestor, on whole
// path components. With no owner nothing is sent.
func (a *Arr) Rescan(ctx context.Context, dir string) Result {
	mapped := a.paths.Map(dir)
	res := Result{Dir: mapped, Attempted: "GET " + a.kind.listPath + " to find the " + a.kind.owner + " that owns the directory"}

	var owners []arrOwner
	if f := a.call.do(ctx, "GET", a.kind.listPath, nil, func(r io.Reader) error {
		var err error
		owners, err = decodeOwners(r)
		return err
	}); f != nil {
		res.Failure = f
		return res
	}

	id, found := nearestOwner(owners, mapped)
	if !found {
		res.NoOwner = true
		return res
	}
	res.Attempted = fmt.Sprintf("POST /api/v3/command %s for %s %d", a.kind.command, a.kind.owner, id)
	body, err := rescanCommand(a.kind, id)
	if err != nil {
		res.Failure = &failure{class: ClassRefused}
		return res
	}
	if f := a.call.do(ctx, "POST", "/api/v3/command", bytes.NewReader(body), nil); f != nil {
		res.Failure = f
		return res
	}
	res.Sent = true
	return res
}

// errNoOwnerID is why rescanCommand refuses: the command would name no owner.
var errNoOwnerID = errors.New("a rescan command must name one owner by a positive id")

// rescanCommand is the ONE place a rescan command body is built, and it cannot build one
// without an owner: RescanSeries and RescanMovie with no id rescan the whole library, so an
// id that is zero or negative is refused rather than sent or omitted.
func rescanCommand(kind arrKind, id int) ([]byte, error) {
	if id <= 0 {
		return nil, errNoOwnerID
	}
	return json.Marshal(map[string]any{"name": kind.command, kind.idField: id})
}

// decodeOwners reads the list endpoint's array one element at a time, keeping two fields of
// each, so a library of many thousand items is never held as one decoded document.
func decodeOwners(r io.Reader) ([]arrOwner, error) {
	dec := json.NewDecoder(r)
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return nil, errors.New("the list is not a JSON array")
	}
	var out []arrOwner
	for dec.More() {
		var o arrOwner
		if err := dec.Decode(&o); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return out, nil
}

// nearestOwner picks the item whose path is dir or dir's nearest ancestor: the LONGEST path
// that owns dir on a whole-component boundary, so `/movies/Film` never owns
// `/movies/Film 2`. An item with no path, a relative path, or no positive id owns nothing.
// Two items with the same winning path are an answer this build cannot choose between, so
// neither is the owner.
func nearestOwner(owners []arrOwner, dir string) (id int, found bool) {
	bestLen, tied := -1, false
	for _, o := range owners {
		if o.ID == nil || *o.ID <= 0 || o.Path == "" || o.Path[0] != '/' {
			continue
		}
		p := path.Clean(o.Path)
		if _, ok := config.UnderPrefix(dir, p); !ok {
			continue
		}
		switch {
		case len(p) > bestLen:
			id, bestLen, tied = *o.ID, len(p), false
		case len(p) == bestLen && *o.ID != id:
			tied = true
		}
	}
	return id, bestLen >= 0 && !tied
}
