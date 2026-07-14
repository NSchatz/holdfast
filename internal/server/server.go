package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/store"
)

// Server is the HTTP surface: chi router + read endpoints + SSE + token-gated
// controls + the embedded UI. It holds no media handles — every mutating action
// routes through the Controller (scan/pause), which cannot touch a file.
type Server struct {
	baseCtx context.Context
	cfg     config.Config
	store   store.Store
	ctrl    *Controller
	hub     *Hub
	ui      http.Handler
	metrics http.Handler
	log     *slog.Logger
	mux     http.Handler
}

// New builds the Server and its router. baseCtx bounds long-lived handlers (the SSE
// stream watches it, so a shutdown that cancels baseCtx releases open streams
// promptly instead of hanging graceful shutdown). ui is the embedded web UI handler
// (served at "/"); pass nil to serve a minimal API-only page. metrics is the
// Prometheus /metrics handler (TRANSCODE-8); pass nil to omit the route. The caller
// starts hub.Run and listens on cfg.EffectiveServerAddr() with s as the handler.
func New(baseCtx context.Context, cfg config.Config, st store.Store, ctrl *Controller, hub *Hub, ui, metrics http.Handler, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	s := &Server{baseCtx: baseCtx, cfg: cfg, store: st, ctrl: ctrl, hub: hub, ui: ui, metrics: metrics, log: log}
	s.mux = s.routes()
	return s
}

// ServeHTTP makes Server an http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func (s *Server) routes() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer) // a panicking handler must never crash the daemon

	r.Route("/api", func(r chi.Router) {
		// Read endpoints — open (protected by the localhost-default bind, not a token).
		r.Get("/summary", s.handleSummary)
		r.Get("/queue", s.handleQueue)
		r.Get("/history", s.handleHistory)
		r.Get("/events", s.handleEvents)

		// Mutating endpoints — token required (and disabled entirely when no token
		// is configured). These only ever start a scan or toggle pause; none can
		// touch a file.
		r.Group(func(r chi.Router) {
			r.Use(s.requireToken)
			r.Post("/rescan", s.handleRescan)
			r.Post("/pause", s.handlePause)
			r.Post("/resume", s.handleResume)
		})
	})

	// Prometheus metrics (TRANSCODE-8), when enabled.
	if s.metrics != nil {
		r.Handle("/metrics", s.metrics)
	}

	// The embedded UI at the root.
	if s.ui != nil {
		r.Handle("/*", s.ui)
	} else {
		r.Get("/", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte("holdfast API is running. See /api/summary, /api/queue, /api/history, /api/events.\n"))
		})
	}
	return r
}

// --- read endpoints ----------------------------------------------------------

// controlState is the at-a-glance status payload (no per-job rows). The reclaimed
// figure is still session-scoped — see snapshot: making it a durable lifetime total is
// TRANSCODE-14's, on the sizes TRANSCODE-13 now persists.
type controlState struct {
	Summary               map[string]int `json:"summary"`
	BytesReclaimedSession int64          `json:"bytes_reclaimed_session"`
	Paused                bool           `json:"paused"`
	Scanning              bool           `json:"scanning"`
}

func (s *Server) handleSummary(w http.ResponseWriter, r *http.Request) {
	sum, err := s.store.Summary(r.Context())
	if err != nil {
		s.fail(w, "summary", err)
		return
	}
	counts := make(map[string]int, len(sum))
	for st, n := range sum {
		counts[string(st)] = n
	}
	writeJSON(w, http.StatusOK, controlState{
		Summary:               counts,
		BytesReclaimedSession: s.hub.BytesReclaimed(),
		Paused:                s.ctrl.Paused(),
		Scanning:              s.ctrl.Scanning(),
	})
}

func (s *Server) handleQueue(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.store.List(r.Context(), activeAndPending, queueLimit)
	if err != nil {
		s.fail(w, "queue", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"queue": toDTOs(jobs)})
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	limit := historyLimit
	if q := r.URL.Query().Get("limit"); q != "" {
		// Clamp a requested limit into (0, historyLimit]; anything larger keeps the
		// cap, anything else keeps the default. `<=` lets a caller ask for exactly
		// the cap.
		if n, err := strconv.Atoi(q); err == nil && n > 0 && n <= limit {
			limit = n
		}
	}
	jobs, err := s.store.List(r.Context(), terminal, limit)
	if err != nil {
		s.fail(w, "history", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"history": toDTOs(jobs)})
}

// handleEvents is the SSE stream: an initial snapshot, then a fresh snapshot on
// every engine transition / control change, plus a periodic heartbeat comment so
// idle connections (and proxies) stay open.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable proxy buffering (nginx)

	ch, cancel := s.hub.Subscribe()
	defer cancel()

	// Initial state so a just-connected client renders immediately.
	if data, err := s.hub.SnapshotJSON(r.Context()); err == nil {
		writeSSE(w, data)
		flusher.Flush()
	}

	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.baseCtx.Done(): // server shutting down — release the stream
			return
		case data := <-ch:
			writeSSE(w, data)
			flusher.Flush()
		case <-heartbeat.C:
			_, _ = fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

// --- mutating endpoints (token-gated) ----------------------------------------

func (s *Server) handleRescan(w http.ResponseWriter, _ *http.Request) {
	started, reason := s.ctrl.Rescan()
	code := http.StatusAccepted
	if !started {
		code = http.StatusConflict // paused / already scanning
	}
	writeJSON(w, code, map[string]any{
		"started":  started,
		"reason":   reason,
		"paused":   s.ctrl.Paused(),
		"scanning": s.ctrl.Scanning(),
	})
}

func (s *Server) handlePause(w http.ResponseWriter, _ *http.Request) {
	s.ctrl.Pause()
	writeJSON(w, http.StatusOK, map[string]any{"paused": s.ctrl.Paused(), "scanning": s.ctrl.Scanning()})
}

func (s *Server) handleResume(w http.ResponseWriter, _ *http.Request) {
	s.ctrl.Resume()
	writeJSON(w, http.StatusOK, map[string]any{"paused": s.ctrl.Paused(), "scanning": s.ctrl.Scanning()})
}

// --- auth --------------------------------------------------------------------

// requireToken gates mutating endpoints. With no token configured, control is
// DISABLED (403) — remote control is off until an operator opts in by setting one.
// A configured token is compared in constant time.
func (s *Server) requireToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := s.cfg.ServerAuthToken
		if token == "" {
			http.Error(w, "control disabled: set server_auth_token (or HOLDFAST_SERVER_AUTH_TOKEN) to enable rescan/pause/resume", http.StatusForbidden)
			return
		}
		got := bearerToken(r.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="holdfast"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// bearerToken extracts the token from an "Authorization: Bearer <token>" header
// (case-insensitive scheme), or "" if absent/malformed.
func bearerToken(header string) string {
	const prefix = "bearer "
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
}

// --- helpers -----------------------------------------------------------------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// writeSSE writes one SSE "snapshot" event. data is compact JSON (no embedded
// newlines), so a single data: line is correct.
func writeSSE(w http.ResponseWriter, data []byte) {
	_, _ = fmt.Fprintf(w, "event: snapshot\ndata: %s\n\n", data)
}

func (s *Server) fail(w http.ResponseWriter, what string, err error) {
	s.log.Warn("api read failed", "endpoint", what, "err", err)
	http.Error(w, "internal error reading "+what, http.StatusInternalServerError)
}

// StartScanLoop kicks an initial scan (unless paused) and, when intervalSec > 0,
// re-scans on that interval until ctx is cancelled. It is the serve command's
// driver; overlap and pause are handled by the Controller (a tick that lands while
// a scan runs or while paused is simply refused). Runs in the caller's goroutine —
// serve launches it in the background.
func (s *Server) StartScanLoop(ctx context.Context, intervalSec int) {
	if started, reason := s.ctrl.Rescan(); !started {
		s.log.Info("initial scan not started", "reason", reason)
	}
	if intervalSec <= 0 {
		return
	}
	ticker := time.NewTicker(time.Duration(intervalSec) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if started, reason := s.ctrl.Rescan(); !started {
				s.log.Debug("periodic scan skipped", "reason", reason)
			}
		}
	}
}
