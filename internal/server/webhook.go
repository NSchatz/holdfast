package server

// POST|PUT /api/webhook/sonarr and /api/webhook/radarr - the native webhook intake
// (docs/design/media-clients.md#webhook-intake).
//
// A Sonarr or Radarr `Connect > Webhook` connection posts the arr's own JSON here. This
// file reads the event type and the imported files' paths out of it, maps each path back
// from the arr's view of the library to holdfast's, and hands the paths to admit - the same
// admission step POST /api/scan uses. It adds a CALLER to the targeted scan and nothing
// else: no rule of its own decides whether a file is eligible, no row is re-opened, and a
// payload it does not recognise queues nothing.
//
// The payload shapes are read from the arr source at the release tags this was written
// against (read 2026-10-02):
//
//   - Sonarr v4.0.20.3014, https://github.com/Sonarr/Sonarr/tree/v4.0.20.3014 ,
//     src/NzbDrone.Core/Notifications/Webhook/: WebhookPayload.cs (`eventType`),
//     WebhookEventType.cs (the event names), WebhookImportPayload.cs (`episodeFile`,
//     `isUpgrade`), WebhookImportCompletePayload.cs (`episodeFiles`), WebhookEpisodeFile.cs
//     (`path`), WebhookRenamePayload.cs (`renamedEpisodeFiles`), WebhookRenamedEpisodeFile.cs
//     (`previousPath`), and WebhookBase.cs (BuildOnDownloadPayload and
//     BuildOnImportCompletePayload both set EventType to Download; BuildTestPayload carries
//     `series` and `episodes`).
//   - Radarr v6.4.4.10685, https://github.com/Radarr/Radarr/tree/v6.4.4.10685 , the same
//     directory: WebhookImportPayload.cs (`movieFile`, `isUpgrade`), WebhookMovieFile.cs
//     (`path`), WebhookRenamePayload.cs (`renamedMovieFiles`), WebhookRenamedMovieFile.cs,
//     WebhookEventType.cs, and WebhookBase.cs (BuildTestPayload carries `movie`,
//     `remoteMovie` and `release`).
//   - Property names are camelCase and null properties are omitted in both:
//     src/NzbDrone.Common/Serializer/Newtonsoft.Json/Json.cs sets
//     CamelCasePropertyNamesContractResolver and NullValueHandling.Ignore, and
//     WebhookProxy.cs sends `body.ToJson()`. The event type keeps its declared spelling
//     (`Download`, not `download`): WebhookEventType.cs pins a StringEnumConverter with
//     DefaultNamingStrategy on the enum.
//   - The method is POST or PUT (WebhookMethod.cs), and the connection can send HTTP Basic
//     credentials and custom headers (WebhookSettings.cs: Username, Password, Headers;
//     WebhookProxy.cs applies both). Basic is sent on the first request, unprompted
//     (src/NzbDrone.Common/Http/Dispatchers/ManagedHttpDispatcher.cs, the
//     BasicNetworkCredential branch).
//
// Names are matched exactly as that source serialises them. A payload spelled any other way
// is one this file does not recognise, and it queues nothing.

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"path"
	"strings"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/secret"
)

// WebhookPathPrefix is the path the intake routes sit under; the arr's name follows it.
const WebhookPathPrefix = "/api/webhook/"

// SetWebhookToken hands the server the RESOLVED webhook credential (`webhook_token`). Set it
// once, before serving. The two intake routes exist either way, so the surface is the same
// in every deployment, and they answer 403 until a non-empty value is set.
func (s *Server) SetWebhookToken(token secret.Value) { s.webhookToken = token }

// webhookApp is one arr the intake accepts payloads from.
type webhookApp struct {
	// name is the last path segment of the route and the name in every log record.
	name string
	// pathMapKey names the configuration key whose map translates this arr's paths.
	pathMapKey string
	// pathMap picks that map out of the configuration.
	pathMap func(config.Config) config.PathMap
	// file, files and renamed are the top-level keys this arr carries imported files under:
	// one object, a list of objects (empty where the arr has no such shape), and the list a
	// Rename carries.
	file, files, renamed string
	// foreign are top-level keys only the OTHER arr sends. One present means the connection
	// points at the wrong endpoint.
	foreign []string
}

var (
	webhookSonarr = webhookApp{
		name: "sonarr", pathMapKey: "sonarr_path_map",
		pathMap: func(c config.Config) config.PathMap { return c.SonarrPathMap },
		file:    "episodeFile", files: "episodeFiles", renamed: "renamedEpisodeFiles",
		foreign: []string{"movie", "remoteMovie", "movieFile", "renamedMovieFiles"},
	}
	webhookRadarr = webhookApp{
		name: "radarr", pathMapKey: "radarr_path_map",
		pathMap: func(c config.Config) config.PathMap { return c.RadarrPathMap },
		file:    "movieFile", renamed: "renamedMovieFiles",
		foreign: []string{"series", "episodes", "episodeFile", "episodeFiles", "renamedEpisodeFiles"},
	}
	// webhookApps is every arr the intake routes, in route order.
	webhookApps = []webhookApp{webhookSonarr, webhookRadarr}
)

// The event types the intake acts on, spelled as the arr sends them.
const (
	webhookEventTest     = "Test"
	webhookEventDownload = "Download"
	webhookEventRename   = "Rename"
	// webhookEventUnrecognised stands in, in the answer and in the log, for an event type
	// neither arr declares. The text a caller sent is never echoed into a log record.
	webhookEventUnrecognised = "unrecognised"
)

// webhookKnownEvents is every event type either arr declares at the cited tags
// (WebhookEventType.cs in each). Only Test, Download and Rename are acted on; the rest are
// named here so the answer and the log can say which event was not consumed.
var webhookKnownEvents = map[string]bool{
	webhookEventTest: true, "Grab": true, webhookEventDownload: true, webhookEventRename: true,
	"SeriesAdd": true, "SeriesDelete": true, "EpisodeFileDelete": true,
	"MovieAdded": true, "MovieDelete": true, "MovieFileDelete": true,
	"Health": true, "HealthRestored": true, "ApplicationUpdate": true, "ManualInteractionRequired": true,
}

// The rule tokens the intake adds, for the answers that are about the EVENT and not about a
// path. With the engine's path rules and POST /api/scan's request rules they are a closed
// vocabulary, stated in docs/api-reference.md.
const (
	// ruleWebhookTest is a Test event: the connection reached holdfast with a credential it
	// accepts, and a Test names no file.
	ruleWebhookTest = "test-event"
	// ruleWebhookNotConsumed is an event type the intake does not act on, or does not
	// recognise.
	ruleWebhookNotConsumed = "event-not-consumed"
	// ruleWebhookWrongApp is a payload carrying the other arr's shape: a Sonarr connection
	// pointed at /api/webhook/radarr, or the reverse.
	ruleWebhookWrongApp = "wrong-app-shape"
	// ruleWebhookNoPath is a Download or Rename that names no file, and each file entry that
	// carries no path.
	ruleWebhookNoPath = "no-file-path"
	// ruleWebhookNotClean is an absolute path that is not in its clean form: a `.` or `..`
	// segment, a doubled slash or a trailing slash. Mapping a prefix is a lexical operation
	// and resolving `..` is not - `/tv/link/../x` names whatever the link's target's parent
	// holds - so such a path is neither mapped nor cleaned nor judged. An arr builds a path
	// by joining a folder and a relative path and sends none of these.
	ruleWebhookNotClean = "path-not-clean"
	// ruleWebhookNothingAccepted is a Download or Rename whose every path was refused by a
	// path rule. The per-path results carry the rule each one broke.
	ruleWebhookNothingAccepted = "nothing-accepted"
)

// webhookResult is what happened to ONE file the payload named, in payload order.
type webhookResult struct {
	// Path is the path as the arr sent it.
	Path string `json:"path"`
	// Mapped is that path in holdfast's view, after the path map: the one every rule was
	// answered against. Absent where the entry carried no path.
	Mapped   string `json:"mapped,omitempty"`
	Accepted bool   `json:"accepted"`
	// Resolved is the path the pipeline will act on. Present only on an accepted path.
	Resolved string `json:"resolved,omitempty"`
	// Rule is the token that refused this path, and Detail the same refusal in words.
	Rule   string `json:"rule,omitempty"`
	Detail string `json:"detail,omitempty"`
	// Retryable is true only for a path the queue could not take.
	Retryable bool `json:"retryable"`
}

// webhookResponse is the body of EVERY answer the intake gives past the credential check.
type webhookResponse struct {
	// App is the arr this endpoint reads payloads as: sonarr or radarr.
	App string `json:"app"`
	// EventType is the event type the payload carried, where it is one either arr declares,
	// and "unrecognised" otherwise. Empty where the body could not be read at all.
	EventType string `json:"event_type"`
	Accepted  int    `json:"accepted"`
	Rejected  int    `json:"rejected"`
	// Rule and Reason say why nothing was queued, or why the request was not fully
	// honoured. Both are empty on an answer that queued every file it named.
	Rule   string `json:"rule,omitempty"`
	Reason string `json:"reason,omitempty"`
	// Retryable says whether the same event, sent again unchanged, could queue a file.
	Retryable bool            `json:"retryable"`
	Results   []webhookResult `json:"results"`
}

// requireWebhookToken gates the intake on `webhook_token`, and on nothing else. With no
// webhook credential resolved the intake is DISABLED (403), as every mutating endpoint is
// without its credential.
//
// A request presents the credential EITHER as `Authorization: Bearer <token>` OR as the
// PASSWORD of `Authorization: Basic` with any username, because those are the two things an
// arr's Webhook connection can send: its Username and Password fields, or a custom header.
// Nothing is read from the URL: a query string or a path segment reaches access logs.
//
// The control token and the read token are NOT accepted here, and this credential
// authorises nothing anywhere else - the other gates compare against their own tokens only -
// so the secret an arr holds can queue a file inside a library root and do nothing more. Both comparisons are constant time and both always run. The credential
// never reaches a response body, a header or a log.
func (s *Server) requireWebhookToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.webhookToken.Empty() {
			http.Error(w, "webhook intake disabled: point webhook_token at a secret "+
				"(file:/run/secrets/... or cmd:...) to enable /api/webhook/sonarr and "+
				"/api/webhook/radarr - see docs/secrets.md",
				http.StatusForbidden)
			return
		}
		want := []byte(s.webhookToken.Expose())
		_, password, _ := r.BasicAuth()
		ok := subtle.ConstantTimeCompare([]byte(bearerToken(r.Header.Get("Authorization"))), want)
		ok |= subtle.ConstantTimeCompare([]byte(password), want)
		if ok != 1 {
			w.Header().Add("WWW-Authenticate", `Bearer realm="holdfast"`)
			w.Header().Add("WWW-Authenticate", `Basic realm="holdfast"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// webhookEvent is what the intake read out of one payload.
type webhookEvent struct {
	// eventType is the label for the answer and the log: a declared event name, or
	// webhookEventUnrecognised.
	eventType string
	// wrongApp names the other arr's key the payload carried, or is empty.
	wrongApp string
	// files is every file entry the event names, in payload order. An entry with no path is
	// kept, with hasPath false, so the answer can account for it.
	files []webhookFile
}

type webhookFile struct {
	path    string
	hasPath bool
}

// parseWebhook reads the event type and the file entries out of a payload. It returns an
// error only for a body that is not the JSON object an arr sends, or that carries a value of
// the wrong JSON type under a key this file reads; everything else is an event, recognised
// or not.
func parseWebhook(app webhookApp, raw []byte) (webhookEvent, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return webhookEvent{}, fmt.Errorf("body must be the JSON object a %s Webhook connection sends: %w", app.name, err)
	}
	if obj == nil {
		// JSON null decodes into a map without an error, and it is not an object.
		return webhookEvent{}, fmt.Errorf("body must be the JSON object a %s Webhook connection sends: it is null", app.name)
	}
	present := func(key string) bool {
		v, ok := obj[key]
		return ok && string(v) != "null"
	}

	ev := webhookEvent{eventType: webhookEventUnrecognised}
	if present("eventType") {
		var sent string
		if err := json.Unmarshal(obj["eventType"], &sent); err != nil {
			return webhookEvent{}, fmt.Errorf(`"eventType" must be a string: %w`, err)
		}
		if webhookKnownEvents[sent] {
			ev.eventType = sent
		}
	}
	for _, key := range app.foreign {
		if present(key) {
			ev.wrongApp = key
			break
		}
	}

	// The keys an event's files arrive under. A Download carries one file, or - Sonarr's
	// import-complete shape - a list of them; a Rename carries a list.
	var single, lists []string
	switch ev.eventType {
	case webhookEventDownload:
		single, lists = []string{app.file}, []string{app.files}
	case webhookEventRename:
		lists = []string{app.renamed}
	}
	// An entry is read as a map and its "path" taken by that exact name: a struct field
	// would match PATH and Path too, and every other key here is matched exactly.
	type entry map[string]json.RawMessage
	add := func(key string, e entry) error {
		if e == nil {
			return fmt.Errorf("%q carries null where a file object belongs", key)
		}
		rawPath, ok := e["path"]
		if !ok || string(rawPath) == "null" {
			ev.files = append(ev.files, webhookFile{})
			return nil
		}
		var p string
		if err := json.Unmarshal(rawPath, &p); err != nil {
			return fmt.Errorf("%q carries a \"path\" that is not a string: %w", key, err)
		}
		ev.files = append(ev.files, webhookFile{path: p, hasPath: p != ""})
		return nil
	}
	for _, key := range single {
		if !present(key) {
			continue
		}
		var e entry
		if err := json.Unmarshal(obj[key], &e); err != nil {
			return webhookEvent{}, fmt.Errorf("%q must be an object carrying a string \"path\": %w", key, err)
		}
		if err := add(key, e); err != nil {
			return webhookEvent{}, err
		}
	}
	for _, key := range lists {
		if key == "" || !present(key) {
			continue
		}
		var es []entry
		if err := json.Unmarshal(obj[key], &es); err != nil {
			return webhookEvent{}, fmt.Errorf("%q must be an array of objects carrying a string \"path\": %w", key, err)
		}
		for _, e := range es {
			if err := add(key, e); err != nil {
				return webhookEvent{}, err
			}
		}
	}
	return ev, nil
}

// mapWebhookPath translates one path from the arr's view to holdfast's, and reports false
// for a path it will not hand on at all.
//
// An absolute path already in its clean form is mapped back through the arr's path map
// (unchanged where no entry matches). The map never cleans anything here: an absolute path
// that is NOT clean is refused (ok false), because cleaning it would resolve a `..` by
// spelling where the filesystem resolves it through whatever the segment before it really
// is, and the engine would then be judging a different file from the one POST /api/scan
// judges for the same string. A path that is not absolute is handed on exactly as it was
// sent, so the engine's own rule refuses it by name.
func mapWebhookPath(m config.PathMap, sent string) (mapped string, ok bool) {
	if !strings.HasPrefix(sent, "/") {
		return sent, true
	}
	if path.Clean(sent) != sent {
		return "", false
	}
	return m.Reverse(sent), true
}

// handleWebhook is the intake for one arr. It answers BEFORE anything is processed, as
// POST /api/scan does, and every answer goes out in the webhookResponse envelope.
//
// The statuses, all of them:
//
//	202  at least one file was accepted and enqueued
//	200  nothing was queued and nothing is wrong with the request: a Test event, an event
//	     the intake does not consume, the other arr's shape, an event naming no path, every
//	     path refused by a rule, or holdfast paused. `rule` and `reason` say which.
//	400  the body is not the JSON an arr sends; or a Test event carrying the other arr's shape
//	401  a webhook credential is configured and the request did not carry it
//	403  no webhook credential is configured, so the intake is disabled outright
//	413  the body is over MaxScanBodyBytes, or the event names more than MaxScanPaths files
//	503  the queue could not take an accepted path (retryable), or no queue is wired
//
// A refusal that is not the request's fault is a 2xx on purpose: an arr marks a connection
// unhealthy, and then stops sending to it for a while, on any other answer, and an event
// holdfast does not act on is not a failed delivery. Each one writes one log record.
func (s *Server) handleWebhook(app webhookApp) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		resp := webhookResponse{App: app.name, Results: []webhookResult{}}
		// nothing answers a request that queued nothing, with the one log record that says so.
		nothing := func(code int, rule, reason string) {
			resp.Rule, resp.Reason = rule, reason
			s.log.Info("webhook queued nothing", "app", app.name, "event_type", resp.EventType,
				"rule", rule, "status", code, "rejected", resp.Rejected)
			writeJSON(w, code, resp)
		}

		if s.subs == nil {
			// Not reachable in the daemon, which wires the queue before the listener exists.
			nothing(http.StatusServiceUnavailable, ruleNotWired,
				"targeted scanning is not wired on this server; nothing was enqueued")
			return
		}
		raw, bad := readBoundedBody(w, r)
		if bad != nil {
			nothing(bad.code, bad.refusal.rule, bad.refusal.message)
			return
		}
		ev, err := parseWebhook(app, raw)
		if err != nil {
			nothing(http.StatusBadRequest, ruleMalformedBody, err.Error()+"; nothing was enqueued")
			return
		}
		resp.EventType = ev.eventType

		if ev.wrongApp != "" {
			// A Test is the arr's own "does this connection work" button, and this one does
			// not: the answer is the one status that makes the button say so. Any other
			// event is a delivery, and a delivery is never failed for being unconsumed.
			code := http.StatusOK
			if ev.eventType == webhookEventTest {
				code = http.StatusBadRequest
			}
			nothing(code, ruleWebhookWrongApp, fmt.Sprintf(
				"the payload carries %q, which only the other arr sends: this endpoint reads %s payloads. "+
					"Point the connection at the endpoint for its own arr. Nothing was enqueued.",
				ev.wrongApp, app.name))
			return
		}
		switch ev.eventType {
		case webhookEventTest:
			nothing(http.StatusOK, ruleWebhookTest,
				"a Test event names no file: the connection reached holdfast and its credential was accepted")
			return
		case webhookEventDownload, webhookEventRename:
		default:
			nothing(http.StatusOK, ruleWebhookNotConsumed, fmt.Sprintf(
				"the intake acts on %s and %s events only; this event queued nothing",
				webhookEventDownload, webhookEventRename))
			return
		}

		if len(ev.files) == 0 {
			nothing(http.StatusOK, ruleWebhookNoPath, fmt.Sprintf(
				"the %s event carries no file under the keys a %s payload names one by; nothing was enqueued",
				ev.eventType, app.name))
			return
		}
		if len(ev.files) > MaxScanPaths {
			nothing(http.StatusRequestEntityTooLarge, ruleTooManyPaths, fmt.Sprintf(
				"the event names %d files, more than the maximum this endpoint accepts in one request (%d); "+
					"the whole request was refused and nothing was enqueued", len(ev.files), MaxScanPaths))
			return
		}
		// Paused answers as POST /api/scan's refusal does in substance - nothing is fed to
		// the workers - and as a 2xx, because a pause is the operator's own standing decision
		// and not a failed delivery.
		if s.ctrl.Paused() {
			resp.Retryable = true
			nothing(http.StatusOK, rulePaused,
				"holdfast is paused, so no new file is fed to the workers; nothing was enqueued. "+
					"The next whole-library scan after POST /api/resume finds the file.")
			return
		}

		// Map every path back to holdfast's view, then hand the lot to the one admission
		// step. An entry with no path never reaches it.
		pathMap := app.pathMap(s.cfg)
		mapped := make([]string, 0, len(ev.files))
		clean := make([]bool, len(ev.files))
		for i, f := range ev.files {
			if !f.hasPath {
				continue
			}
			if m, ok := mapWebhookPath(pathMap, f.path); ok {
				clean[i] = true
				mapped = append(mapped, m)
			}
		}
		admitted, full := s.admit(mapped)

		next := 0
		for i, f := range ev.files {
			if !f.hasPath {
				resp.Results = append(resp.Results, webhookResult{Rule: ruleWebhookNoPath,
					Detail: "this file entry carries no path"})
				resp.Rejected++
				continue
			}
			if !clean[i] {
				resp.Results = append(resp.Results, webhookResult{Path: f.path, Rule: ruleWebhookNotClean,
					Detail: "the path is not in its clean form (a . or .. segment, a doubled or a trailing " +
						"slash); it was not mapped, not resolved and not judged"})
				resp.Rejected++
				continue
			}
			a := admitted[next]
			next++
			resp.Results = append(resp.Results, webhookResult{
				Path: f.path, Mapped: a.Path, Accepted: a.Accepted, Resolved: a.Resolved,
				Rule: a.Rule, Detail: a.Detail, Retryable: a.Retryable,
			})
			if a.Accepted {
				resp.Accepted++
			} else {
				resp.Rejected++
			}
		}
		// The paths, in holdfast's view, at debug: a refusal under a path rule is most often
		// a path map that is missing or wrong, and this is where an operator sees what the
		// map produced. A file name may spell a title, which is why these are at debug and
		// nowhere else; no other payload field is ever logged.
		s.log.Debug("webhook paths", "app", app.name, "event_type", ev.eventType, "paths", mapped)

		switch {
		case full:
			// The one answer that may have queued SOME files, so it is its own record and
			// not the "queued nothing" one.
			resp.Retryable = true
			resp.Rule = ruleQueueFull
			resp.Reason = fmt.Sprintf("the submission queue is full (capacity %d); the paths marked %s were "+
				"NOT taken and nothing was recorded for them", s.subs.Cap(), ruleQueueFull)
			s.log.Warn("webhook not fully queued", "app", app.name, "event_type", ev.eventType,
				"rule", ruleQueueFull, "accepted", resp.Accepted, "rejected", resp.Rejected)
			writeJSON(w, http.StatusServiceUnavailable, resp)
		case resp.Accepted == 0:
			nothing(http.StatusOK, ruleWebhookNothingAccepted, fmt.Sprintf(
				"every path was refused by a rule, named per path in results; nothing was enqueued. "+
					"A path outside every library root usually means %s does not map the %s view of the "+
					"library to holdfast's", app.pathMapKey, app.name))
		default:
			s.log.Info("webhook accepted", "app", app.name, "event_type", ev.eventType,
				"accepted", resp.Accepted, "rejected", resp.Rejected)
			writeJSON(w, http.StatusAccepted, resp)
		}
	}
}
