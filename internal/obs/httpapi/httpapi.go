// Package httpapi serves the obs subsystem's own /api/obs/* endpoints for the
// trajectory UI. It is OWNED by obs and registered into the host dashboard's
// shared mux at the wiring point (decision D4) — the dashboard package never
// imports obs; it only receives a generic list of routes. This keeps the
// reverse-import separability boundary intact while still shipping a real UI.
package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/marmutapp/superbased-observer/internal/obs"
	obsstore "github.com/marmutapp/superbased-observer/internal/obs/store"
)

// API holds the read-only obs store the trajectory endpoints serve from, plus
// the optional pull-only ProxyEnricher (§9 / P6) that renders proxy-exact
// cost + cache split + routing rationale ON spans the proxy also saw. The
// enricher is nil on a node without the proxy wiring — every handler stays
// safe in that case.
type API struct {
	store    *obsstore.Store
	enricher obs.ProxyEnricher
	logger   *slog.Logger
}

// New builds the API over the obs store. enricher may be nil (no proxy
// enrichment — handlers serve bare trajectory rows).
func New(store *obsstore.Store, enricher obs.ProxyEnricher, logger *slog.Logger) *API {
	if logger == nil {
		logger = slog.Default()
	}
	return &API{store: store, enricher: enricher, logger: logger}
}

// Route is a (pattern, handler) pair the wiring point hands the host mux. Its
// type is local to obs so this package never imports the dashboard.
type Route struct {
	Pattern string
	Handler http.HandlerFunc
}

// Routes is the full set of obs trajectory endpoints. Patterns use Go 1.22
// method+wildcard syntax, matching the host mux.
func (a *API) Routes() []Route {
	return []Route{
		{"GET /api/obs/enabled", a.handleEnabled},
		{"GET /api/obs/traces", a.handleTraces},
		{"GET /api/obs/trace/{id}", a.handleTrace},
		{"GET /api/obs/eval/datasets", a.handleEvalDatasets},
		{"GET /api/obs/eval/runs", a.handleEvalRuns},
		{"GET /api/obs/eval/run/{id}", a.handleEvalRun},
	}
}

// handleEnabled is the frontend nav-gate probe. Reaching this handler means
// the subsystem is on (it's only registered when enabled), so it always
// reports true.
func (a *API) handleEnabled(w http.ResponseWriter, _ *http.Request) {
	a.writeJSON(w, map[string]bool{"enabled": true})
}

// handleTraces lists recent traces.
func (a *API) handleTraces(w http.ResponseWriter, r *http.Request) {
	limit := intParam(r, "limit", 100)
	offset := intParam(r, "offset", 0)
	rows, err := a.store.ListTraces(r.Context(), limit, offset)
	if err != nil {
		a.writeErr(w, err)
		return
	}
	if rows == nil {
		rows = []obsstore.TraceListRow{}
	}
	a.writeJSON(w, map[string]any{"traces": rows})
}

// traceDetailResponse is the /api/obs/trace/{id} payload: the bare trajectory
// (embedded, so its trace/spans/events/links JSON shape is unchanged) plus an
// optional per-span enrichment map keyed by span_id. enrichments is present
// only for spans the proxy also observed (request_id matched an api_turn) —
// the wedge made visible (§9). Spans without a proxy turn simply have no
// entry, so the field stays empty on a pure third-party-OTLP trace.
type traceDetailResponse struct {
	obsstore.TraceDetail
	Enrichments map[string]obs.Enrichment `json:"enrichments,omitempty"`
}

// handleTrace returns one trace's full trajectory; 404 when unknown. When a
// ProxyEnricher is wired, LLM spans carrying a request_id are enriched
// pull-only with the proxy's exact cost/cache/routing facts (additive — an
// enrichment failure is logged and skipped, never failing the trace read).
func (a *API) handleTrace(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "missing trace id", http.StatusBadRequest)
		return
	}
	detail, found, err := a.store.GetTrace(r.Context(), id)
	if err != nil {
		a.writeErr(w, err)
		return
	}
	if !found {
		http.Error(w, "trace not found", http.StatusNotFound)
		return
	}
	resp := traceDetailResponse{TraceDetail: detail}
	resp.Enrichments = a.enrichSpans(r, detail.Spans)
	a.writeJSON(w, resp)
}

// enrichSpans pulls proxy facts for every span with a request_id, returning a
// span_id→Enrichment map (only Found entries). Returns nil when no enricher is
// wired or nothing matched, so the JSON field is omitted.
func (a *API) enrichSpans(r *http.Request, spans []obsstore.SpanRow) map[string]obs.Enrichment {
	if a.enricher == nil {
		return nil
	}
	var out map[string]obs.Enrichment
	for _, sp := range spans {
		if sp.RequestID == "" {
			continue
		}
		en, err := a.enricher.EnrichByRequestID(r.Context(), sp.RequestID)
		if err != nil {
			a.logger.Warn("obs httpapi: enrichment failed", "request_id", sp.RequestID, "err", err)
			continue
		}
		if !en.Found {
			continue
		}
		if out == nil {
			out = make(map[string]obs.Enrichment)
		}
		out[sp.SpanID] = en
	}
	return out
}

func (a *API) writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		a.logger.Warn("obs httpapi: encode failed", "err", err)
	}
}

func (a *API) writeErr(w http.ResponseWriter, err error) {
	a.logger.Warn("obs httpapi: handler error", "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func intParam(r *http.Request, key string, def int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}
