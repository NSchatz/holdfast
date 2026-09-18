package server

// The self-describing HTTP surface (http-surface H4, S0128).
//
// The document served at GET /api/schema is GENERATED, at request time, from two things
// this package already holds and nothing else:
//
//   - the ROUTES come from chi's own router, walked. There is no list of paths in this
//     file: a route that is registered is described, and a route that is not registered
//     cannot be described (surfaceOf refuses a described endpoint the router does not
//     route, and refuses a routed endpoint nothing describes). A hand-maintained document
//     standing between the two is exactly what H4 exists to remove.
//   - the BODY SHAPES come from the Go types the handlers encode, by reflection over the
//     json tags. A field renamed, retyped, made a pointer or given an omitempty moves the
//     served document in the same build, because the document IS that type read back.
//
// What is hand-written here is the part no Go type carries: which STATUS CODES an endpoint
// answers with, and which media type and which body goes out under each. That half is
// bound to the routed set - it can neither omit a routed endpoint nor invent one - and the
// 405 every partially-routed path answers is derived from the router rather than declared.
//
// shapeOf REFUSES an interface-typed field, which is the load-bearing refusal in this
// file: a response encoded from map[string]any would otherwise be describable as "an
// object of whatever keys", a declaration that makes every validator pass while saying
// nothing. That is H4's property lost rather than met, so it is a build-time error and the
// handlers carry named response types instead.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/secret"
	"github.com/NSchatz/holdfast/internal/version"
)

// SchemaPath is the fixed path the surface document is served at.
const SchemaPath = "/api/schema"

// SchemaFormat names the document's own format, and SchemaFormatVersion that format's
// version. They are the document's self-identification (AC-1): a consumer reads these two
// before anything else, and the diff gate ignores both when comparing surfaces, because
// the format's version is not the service's surface.
const (
	SchemaFormat        = "holdfast.http-surface"
	SchemaFormatVersion = "1"
)

// The media types this surface answers with, named once so the document and the handlers
// cannot spell them differently.
const (
	mediaJSON   = "application/json; charset=utf-8"
	mediaText   = "text/plain; charset=utf-8"
	mediaSSE    = "text/event-stream"
	mediaMetric = "text/plain" // Prometheus exposition; the parameters are negotiated
)

// The shape vocabulary. It is CLOSED: every kind is one of these, and a type that maps to
// none of them is an error rather than a permissive default.
const (
	kindObject  = "object"
	kindArray   = "array"
	kindMap     = "map"
	kindString  = "string"
	kindInteger = "integer"
	kindNumber  = "number"
	kindBoolean = "boolean"
	// kindText is a body that is not JSON at all - http.Error's plain sentence, or the
	// root page. kindEmpty is a body with no bytes (chi's 405). kindStream is the SSE
	// stream, whose frames are not a response body and are not validated as one.
	kindText   = "text"
	kindEmpty  = "empty"
	kindStream = "event-stream"
	// kindShape is this format describing ITSELF: the body of GET /api/schema carries
	// shape declarations, which are written in this same kind/fields/elem vocabulary. A
	// self-referential format has to terminate somewhere and this is where.
	kindShape = "shape"
)

// Document is the surface document. It is the ONE artifact with two consumers: the running
// server serves it at SchemaPath, and the diff gate generates it to compare against the
// committed baseline. Nothing here is a map, and every slice is sorted before it is
// written, so marshalling it twice produces the same bytes (AC-3).
type Document struct {
	Schema          string     `json:"schema"`
	SchemaVersion   string     `json:"schema_version"`
	HoldfastVersion string     `json:"holdfast_version"`
	Endpoints       []Endpoint `json:"endpoints"`
}

// Endpoint is one routed method+path and everything it can answer with.
type Endpoint struct {
	Method    string     `json:"method"`
	Path      string     `json:"path"`
	Responses []Response `json:"responses"`
}

// Response is one status code an endpoint answers with, and what goes out under it.
type Response struct {
	Status    int    `json:"status"`
	MediaType string `json:"media_type"`
	Body      Shape  `json:"body"`
}

// Shape is a body's declared shape. Kind is from the closed vocabulary above; Fields is
// the CLOSED set of named fields of an object; Elem is the element of an array or the
// value of a map.
type Shape struct {
	Kind string `json:"kind"`
	// Nullable says the value may be JSON null. It is true for every pointer field and
	// for every slice, because a nil slice encodes as null.
	Nullable bool    `json:"nullable,omitempty"`
	Fields   []Field `json:"fields,omitempty"`
	Elem     *Shape  `json:"elem,omitempty"`
}

// Field is one named field of an object. Required is false exactly when the Go field
// carries `omitempty`, which is the only way a field can be absent from a response.
type Field struct {
	Name     string `json:"name"`
	Required bool   `json:"required"`
	Type     Shape  `json:"type"`
}

// chiMethods is the method set chi routes. A path that registers fewer than all of them
// answers 405 for the rest, which is where the 405 in the document comes from: it is
// derived from the router, never declared beside a handler.
var chiMethods = []string{
	http.MethodConnect, http.MethodDelete, http.MethodGet, http.MethodHead,
	http.MethodOptions, http.MethodPatch, http.MethodPost, http.MethodPut,
	http.MethodTrace,
}

// --- the generator -----------------------------------------------------------

// Surface builds the document for THIS server's router.
//
// A description this server's router does not route is not an error here: New makes the
// Prometheus route conditional, so a deployment with metrics disabled legitimately routes
// fewer paths than the descriptions cover. The stale-description half of AC-2 is caught on
// the FULL surface instead, by ReferenceSurface, which routes everything.
func (s *Server) Surface() (Document, error) { return surfaceOf(s.mux, false) }

// SurfaceJSON is Surface marshalled the one way this package ever marshals it: indented,
// newline-terminated, and deterministic. The served bytes and the gate's bytes are this
// function's output or they are not comparable (AC-3).
func (s *Server) SurfaceJSON() ([]byte, error) {
	doc, err := s.Surface()
	if err != nil {
		return nil, err
	}
	return MarshalDocument(doc)
}

// MarshalDocument renders a document the one way it is ever rendered.
func MarshalDocument(doc Document) ([]byte, error) {
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// ReferenceSurface is the document for a server constructed with EVERY optional route
// present, which is the surface the baseline records and the diff gate compares against.
// It is built by constructing a real Server and walking its real router: there is no
// second route table here for the gate to read.
//
// The Prometheus handler is the one route New makes conditional, so the reference supplies
// one. A deployment that disables metrics serves a document without /metrics, which is
// AC-2 working rather than a disagreement - the reference is the full surface, and the
// baseline is a record of the full surface.
// The reference routes every route this build has, so a description matching none of them
// is a description that has outlived its route: an error here and nowhere else.
func ReferenceSurface() (Document, error) {
	return surfaceOf(referenceServer().mux, true)
}

// ReferenceSurfaceJSON is ReferenceSurface's bytes.
func ReferenceSurfaceJSON() ([]byte, error) {
	doc, err := ReferenceSurface()
	if err != nil {
		return nil, err
	}
	return MarshalDocument(doc)
}

func referenceServer() *Server {
	return New(context.Background(), config.Config{}, secret.Value{}, secret.Value{},
		nil, nil, nil, http.NotFoundHandler(),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// surfaceOf walks the router and describes exactly what it routes.
//
// Both directions are errors, and that is the whole of AC-2's mechanism: a routed endpoint
// nothing describes fails, so a route cannot be added without the document moving, and a
// described endpoint the router does not route fails, so the descriptions cannot outlive
// the routes they are about.
func surfaceOf(routes chi.Routes, requireEveryDescriptionRouted bool) (Document, error) {
	routed := map[string][]string{} // path -> methods
	if err := chi.Walk(routes, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		routed[route] = append(routed[route], method)
		return nil
	}); err != nil {
		return Document{}, fmt.Errorf("walking the router: %w", err)
	}

	declared, err := declaredResponses()
	if err != nil {
		return Document{}, err
	}
	seen := map[string]bool{}

	doc := Document{
		Schema:          SchemaFormat,
		SchemaVersion:   SchemaFormatVersion,
		HoldfastVersion: version.Version,
	}
	paths := make([]string, 0, len(routed))
	for p := range routed {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, path := range paths {
		methods := routed[path]
		sort.Strings(methods)
		partial := len(methods) < len(chiMethods)
		for _, method := range methods {
			key, ok := describedKey(declared, method, path)
			if !ok {
				return Document{}, fmt.Errorf(
					"%s %s is routed and nothing in schema.go describes it: add its status codes and "+
						"response bodies, or the served document would be silent about a live endpoint", method, path)
			}
			seen[key] = true
			resp := append([]Response(nil), declared[key]...)
			if partial {
				// Derived, never declared: chi answers 405 for a method a registered path
				// does not route, so the document says so wherever that is reachable.
				resp = append(resp, Response{Status: http.StatusMethodNotAllowed, MediaType: "", Body: Shape{Kind: kindEmpty}})
			}
			sort.Slice(resp, func(i, j int) bool { return resp[i].Status < resp[j].Status })
			doc.Endpoints = append(doc.Endpoints, Endpoint{Method: method, Path: path, Responses: resp})
		}
	}

	var orphans []string
	for key := range declared {
		if !seen[key] {
			orphans = append(orphans, key)
		}
	}
	if requireEveryDescriptionRouted && len(orphans) > 0 {
		sort.Strings(orphans)
		return Document{}, fmt.Errorf(
			"schema.go describes %s, which this router does not route: the document must describe the "+
				"routes that exist and no others", strings.Join(orphans, ", "))
	}
	return doc, nil
}

// describedKey finds the description for a routed method+path: the exact key first, then
// the every-method key a path registered with chi's Handle needs.
func describedKey(declared map[string][]Response, method, path string) (string, bool) {
	exact := method + " " + path
	if _, ok := declared[exact]; ok {
		return exact, true
	}
	any := "* " + path
	if _, ok := declared[any]; ok {
		return any, true
	}
	return "", false
}

// --- what each endpoint answers ----------------------------------------------

// declaredResponses is the hand-written half: the status codes, media types and bodies no
// Go type carries. Every BODY here is derived by reflection from the type the handler
// actually encodes, so the field names and types in this document cannot drift from the
// ones on the wire.
//
// 401 and 403 are the token gates' own answers and belong to every endpoint behind one:
// requireReadToken answers 401 on the reads while a read token is configured, and
// requireToken answers 403 with control disabled and 401 on a wrong credential.
func declaredResponses() (map[string][]Response, error) {
	var firstErr error
	body := func(v any) Shape {
		sh, err := shapeOf(reflect.TypeOf(v))
		if err != nil && firstErr == nil {
			firstErr = err
		}
		return sh
	}
	jsonOK := func(status int, v any) Response {
		return Response{Status: status, MediaType: mediaJSON, Body: body(v)}
	}
	text := func(status int) Response {
		return Response{Status: status, MediaType: mediaText, Body: Shape{Kind: kindText}}
	}

	// The read gate's refusal, on every endpoint under /api that it fronts.
	readGate := []Response{text(http.StatusUnauthorized)}
	// The control gate's refusals, on every mutating endpoint and the control-gated read.
	controlGate := []Response{text(http.StatusUnauthorized), text(http.StatusForbidden)}

	declared := map[string][]Response{
		"GET /": {
			{Status: http.StatusOK, MediaType: mediaText, Body: Shape{Kind: kindText}},
			// The source offer could not be resolved, so the root page refuses rather
			// than serving a page carrying no offer.
			text(http.StatusServiceUnavailable),
		},

		"GET /api/summary": append([]Response{
			jsonOK(http.StatusOK, controlState{}),
			text(http.StatusInternalServerError),
		}, readGate...),

		"GET /api/queue": append([]Response{
			jsonOK(http.StatusOK, queueResponse{}),
			text(http.StatusInternalServerError),
		}, readGate...),

		"GET /api/history": append([]Response{
			jsonOK(http.StatusOK, historyResponse{}),
			text(http.StatusInternalServerError),
		}, readGate...),

		"GET /api/events": append([]Response{
			{Status: http.StatusOK, MediaType: mediaSSE, Body: Shape{Kind: kindStream}},
			// The ResponseWriter cannot flush, so no stream can be served.
			text(http.StatusInternalServerError),
		}, readGate...),

		"GET " + SchemaPath: {
			{Status: http.StatusOK, MediaType: mediaJSON, Body: body(Document{})},
			text(http.StatusInternalServerError),
		},

		"POST /api/rescan": append([]Response{
			jsonOK(http.StatusAccepted, rescanResponse{}),
			jsonOK(http.StatusConflict, rescanResponse{}),
		}, controlGate...),

		"POST /api/scan": append([]Response{
			jsonOK(http.StatusAccepted, scanResponse{}),
			jsonOK(http.StatusBadRequest, scanResponse{}),
			jsonOK(http.StatusConflict, scanResponse{}),
			jsonOK(http.StatusRequestEntityTooLarge, scanResponse{}),
			jsonOK(http.StatusServiceUnavailable, scanResponse{}),
		}, controlGate...),

		"POST /api/pause": append([]Response{
			jsonOK(http.StatusOK, controlToggleResponse{}),
		}, controlGate...),

		"POST /api/resume": append([]Response{
			jsonOK(http.StatusOK, controlToggleResponse{}),
		}, controlGate...),

		"GET /api/search": append([]Response{
			jsonOK(http.StatusOK, searchResponse{}),
			text(http.StatusBadRequest),
			text(http.StatusInternalServerError),
		}, controlGate...),

		"GET /api/exclusions/": append([]Response{
			jsonOK(http.StatusOK, exclusionsResponse{}),
			text(http.StatusInternalServerError),
		}, controlGate...),

		"POST /api/exclusions/": append([]Response{
			jsonOK(http.StatusOK, excludeResponse{}),
			text(http.StatusBadRequest),
			text(http.StatusInternalServerError),
		}, controlGate...),

		"DELETE /api/exclusions/": append([]Response{
			jsonOK(http.StatusOK, excludeResponse{}),
			text(http.StatusBadRequest),
			text(http.StatusInternalServerError),
		}, controlGate...),

		// Registered with chi's Handle, so every method routes to it.
		"* /metrics": {
			{Status: http.StatusOK, MediaType: mediaMetric, Body: Shape{Kind: kindText}},
		},
	}
	if firstErr != nil {
		return nil, firstErr
	}
	return declared, nil
}

// --- deriving a shape from the type the handler encodes ------------------------

func shapeOf(t reflect.Type) (Shape, error) { return shapeOfIn(t, nil) }

// shapeOfIn walks a Go type into a Shape. stack carries the struct types currently being
// expanded, so a self-referential type terminates instead of recursing for ever.
func shapeOfIn(t reflect.Type, stack []reflect.Type) (Shape, error) {
	if t == nil {
		return Shape{}, fmt.Errorf("schema: no type to describe")
	}
	if t.Kind() == reflect.Pointer {
		inner, err := shapeOfIn(t.Elem(), stack)
		if err != nil {
			return Shape{}, err
		}
		inner.Nullable = true
		return inner, nil
	}
	switch t.Kind() {
	case reflect.String:
		return Shape{Kind: kindString}, nil
	case reflect.Bool:
		return Shape{Kind: kindBoolean}, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return Shape{Kind: kindInteger}, nil
	case reflect.Float32, reflect.Float64:
		return Shape{Kind: kindNumber}, nil
	case reflect.Slice, reflect.Array:
		elem, err := shapeOfIn(t.Elem(), stack)
		if err != nil {
			return Shape{}, err
		}
		// A nil slice encodes as JSON null, so an array is nullable by construction.
		return Shape{Kind: kindArray, Nullable: t.Kind() == reflect.Slice, Elem: &elem}, nil
	case reflect.Map:
		if t.Key().Kind() != reflect.String {
			return Shape{}, fmt.Errorf("schema: %s has non-string keys and cannot be described", t)
		}
		elem, err := shapeOfIn(t.Elem(), stack)
		if err != nil {
			return Shape{}, err
		}
		return Shape{Kind: kindMap, Nullable: true, Elem: &elem}, nil
	case reflect.Interface:
		// The refusal this file exists for. An interface-typed field is an open object on
		// the wire, and a document that declared one would make every validator pass while
		// describing nothing at all - H4's property lost rather than met. Give the handler
		// a named response type instead.
		return Shape{}, fmt.Errorf(
			"schema: %s is an interface, so its fields are not a closed set: give the handler a named "+
				"response type with json tags instead of an anonymous map[string]any", t)
	case reflect.Struct:
		// This format describing itself. A Shape declaration is written in this same
		// vocabulary, which is where the recursion terminates.
		if t == reflect.TypeOf(Shape{}) {
			return Shape{Kind: kindShape}, nil
		}
		for _, seen := range stack {
			if seen == t {
				return Shape{}, fmt.Errorf(
					"schema: %s is recursive and has no terminal in this format's vocabulary", t)
			}
		}
		stack = append(stack, t)
		sh := Shape{Kind: kindObject}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if f.PkgPath != "" {
				continue // unexported: encoding/json never writes it
			}
			name, required, ok := jsonFieldName(f)
			if !ok {
				continue
			}
			ft, err := shapeOfIn(f.Type, stack)
			if err != nil {
				return Shape{}, fmt.Errorf("%s.%s: %w", t.Name(), f.Name, err)
			}
			sh.Fields = append(sh.Fields, Field{Name: name, Required: required, Type: ft})
		}
		return sh, nil
	default:
		return Shape{}, fmt.Errorf("schema: %s has no shape in this format's vocabulary", t)
	}
}

// jsonFieldName reads the json tag the way encoding/json reads it: the name, whether the
// field can be omitted, and whether it is written at all.
func jsonFieldName(f reflect.StructField) (name string, required, ok bool) {
	tag := f.Tag.Get("json")
	if tag == "-" {
		return "", false, false
	}
	parts := strings.Split(tag, ",")
	name = parts[0]
	if name == "" {
		name = f.Name
	}
	required = true
	for _, opt := range parts[1:] {
		if opt == "omitempty" {
			required = false
		}
	}
	return name, required, true
}

// --- the validator (AC-4) ------------------------------------------------------

// ValidateResponse checks a LIVE response body against the shape this document declares
// for that endpoint and status. It returns one line per violation, empty when the response
// is valid, and an error when the document declares nothing for that endpoint at all -
// which is not a pass.
//
// The two violations it exists to find are H4's whole point: a field on the wire that the
// document does not declare (the document has gone stale), and a field the document
// declares required that the response omits (the document overclaims).
func (d Document) ValidateResponse(method, path string, status int, body []byte) ([]string, error) {
	for _, ep := range d.Endpoints {
		if ep.Method != method || ep.Path != path {
			continue
		}
		for _, resp := range ep.Responses {
			if resp.Status != status {
				continue
			}
			if resp.Body.Kind != kindObject && resp.Body.Kind != kindArray && resp.Body.Kind != kindMap {
				return nil, fmt.Errorf("%s %s %d declares a %s body, which is not JSON to validate",
					method, path, status, resp.Body.Kind)
			}
			dec := json.NewDecoder(bytes.NewReader(body))
			dec.UseNumber()
			var v any
			if err := dec.Decode(&v); err != nil {
				return nil, fmt.Errorf("%s %s %d: body is not JSON: %w", method, path, status, err)
			}
			return validateValue("", resp.Body, v), nil
		}
		return nil, fmt.Errorf("%s %s declares no response for status %d", method, path, status)
	}
	return nil, fmt.Errorf("the document declares no endpoint %s %s", method, path)
}

func validateValue(at string, sh Shape, v any) []string {
	where := at
	if where == "" {
		where = "(body)"
	}
	if v == nil {
		if sh.Nullable {
			return nil
		}
		return []string{where + " is null and the document does not declare it nullable"}
	}
	switch sh.Kind {
	case kindObject:
		obj, ok := v.(map[string]any)
		if !ok {
			return []string{fmt.Sprintf("%s is %s and the document declares an object", where, jsonKindOf(v))}
		}
		var out []string
		declared := map[string]Field{}
		for _, f := range sh.Fields {
			declared[f.Name] = f
		}
		names := make([]string, 0, len(obj))
		for k := range obj {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			f, ok := declared[k]
			if !ok {
				out = append(out, fmt.Sprintf("%s carries %q, which the document does not declare", where, k))
				continue
			}
			out = append(out, validateValue(join(at, k), f.Type, obj[k])...)
		}
		for _, f := range sh.Fields {
			if _, present := obj[f.Name]; !present && f.Required {
				out = append(out, fmt.Sprintf("%s omits %q, which the document declares required", where, f.Name))
			}
		}
		return out
	case kindArray:
		items, ok := v.([]any)
		if !ok {
			return []string{fmt.Sprintf("%s is %s and the document declares an array", where, jsonKindOf(v))}
		}
		if sh.Elem == nil {
			return []string{where + " is declared as an array with no element shape"}
		}
		var out []string
		for i, item := range items {
			out = append(out, validateValue(at+"["+strconv.Itoa(i)+"]", *sh.Elem, item)...)
		}
		return out
	case kindMap:
		obj, ok := v.(map[string]any)
		if !ok {
			return []string{fmt.Sprintf("%s is %s and the document declares a map", where, jsonKindOf(v))}
		}
		if sh.Elem == nil {
			return []string{where + " is declared as a map with no value shape"}
		}
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var out []string
		for _, k := range keys {
			out = append(out, validateValue(join(at, k), *sh.Elem, obj[k])...)
		}
		return out
	case kindString:
		if _, ok := v.(string); !ok {
			return []string{fmt.Sprintf("%s is %s and the document declares a string", where, jsonKindOf(v))}
		}
	case kindBoolean:
		if _, ok := v.(bool); !ok {
			return []string{fmt.Sprintf("%s is %s and the document declares a boolean", where, jsonKindOf(v))}
		}
	case kindInteger:
		n, ok := v.(json.Number)
		if !ok {
			return []string{fmt.Sprintf("%s is %s and the document declares an integer", where, jsonKindOf(v))}
		}
		if _, err := n.Int64(); err != nil {
			return []string{fmt.Sprintf("%s is %s and the document declares an integer", where, n.String())}
		}
	case kindNumber:
		n, ok := v.(json.Number)
		if !ok {
			return []string{fmt.Sprintf("%s is %s and the document declares a number", where, jsonKindOf(v))}
		}
		if _, err := n.Float64(); err != nil {
			return []string{fmt.Sprintf("%s is %s and the document declares a number", where, n.String())}
		}
	case kindShape:
		if _, ok := v.(map[string]any); !ok {
			return []string{fmt.Sprintf("%s is %s and the document declares a shape declaration", where, jsonKindOf(v))}
		}
	default:
		return []string{fmt.Sprintf("%s is declared with kind %q, which is not JSON to validate", where, sh.Kind)}
	}
	return nil
}

func join(at, key string) string {
	if at == "" {
		return key
	}
	return at + "." + key
}

func jsonKindOf(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "a boolean"
	case json.Number:
		return "a number"
	case string:
		return "a string"
	case []any:
		return "an array"
	case map[string]any:
		return "an object"
	default:
		return fmt.Sprintf("%T", v)
	}
}

// --- the handler ---------------------------------------------------------------

// handleSchema serves the surface document. It is OUTSIDE both token gates: the document
// carries endpoint paths, methods, status codes, media types, field names and field types
// and nothing else - no credential, no configured filesystem path, no per-file datum - so
// there is nothing behind it for a credential to protect, and a caller has to be able to
// read the surface before it can authenticate against it.
func (s *Server) handleSchema(w http.ResponseWriter, _ *http.Request) {
	b, err := s.SurfaceJSON()
	if err != nil {
		s.log.Warn("could not generate the surface document", "err", err)
		http.Error(w, "internal error generating the API surface document", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", mediaJSON)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}
