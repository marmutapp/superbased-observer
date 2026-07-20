package dashboard

// workspace.go — the Terminal Workspace dock-grid layout persistence
// (docs/plans/terminal-dock-grid-design-2026-07-20.md; operator decision
// 2026-07-21: server-side so the layout is shared across the operator's
// devices). Presentation state only — the blob maps terminal handles /
// session ids to grid cells; the store seam (store.Workspace* — migration
// 073, node-local, privacy-sentinel-pinned) validates JSON + size and never
// interprets the contents.
//
// Two single-purpose routes instead of one dual-method route (the mux's
// whole-route classification rule, dashboard.go NOTE): the READ is View so a
// remote paired device renders the shared grid read-only; the SAVE is Local —
// arranging the grid is an owner action (the xs mobile breakpoint has drag
// disabled by design, so remote devices never need the write).

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/marmutapp/superbased-observer/internal/store"
)

// defaultWorkspaceName is the single P0 workspace; named workspaces are a
// later phase and will extend the same table keyed by name.
const defaultWorkspaceName = "default"

// handleWorkspaceLayoutGet — GET /api/terminal/workspace-layout (View).
// Returns {"layout": <blob-or-null>}; the stored blob is already-validated
// JSON and is embedded verbatim.
func (s *Server) handleWorkspaceLayoutGet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	st := s.remoteManageStore()
	if st == nil {
		http.Error(w, `{"error":"no database wired"}`, http.StatusServiceUnavailable)
		return
	}
	layout, ok, err := st.GetWorkspaceLayout(r.Context(), defaultWorkspaceName)
	if err != nil {
		writeErr(w, err)
		return
	}
	// A REMOTE principal gets the blob REDACTED to the handles its own
	// visibleSnapshot admits (which already enforces the allow_terminal_view
	// redaction of attach/resume rows and the always-redacted setup handles) —
	// otherwise the stored layout would disclose handle existence the snapshot
	// boundary deliberately hides (review MED). Dead/tombstone handles are
	// stripped for remote too (fail-closed: not in any snapshot). The local
	// owner gets the blob verbatim, tombstones included.
	if ok && remoteExposedFromContext(r.Context()) {
		// No launch manager (dashboard launch disabled) ⇒ no visible handles
		// for a remote caller — serve layout:null (fail-closed) instead of
		// dereferencing a nil manager through visibleSnapshot.
		if s.opts.LaunchManager == nil {
			ok = false
		} else {
			layout, ok = redactWorkspaceLayoutForRemote(layout, s.visibleSnapshot(r.Context()))
		}
	}
	w.Header().Set("Content-Type", "application/json")
	if !ok {
		_, _ = w.Write([]byte(`{"layout":null}`))
		return
	}
	_, _ = w.Write([]byte(`{"layout":` + layout + `}`))
}

// handleWorkspaceLayoutSave — PUT /api/terminal/workspace-layout/save
// (Local). Accepts the raw layout JSON blob as the request body; the store
// seam enforces validity + the 256 KiB bound.
func (s *Server) handleWorkspaceLayoutSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut && r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	st := s.remoteManageStore()
	if st == nil {
		http.Error(w, `{"error":"no database wired"}`, http.StatusServiceUnavailable)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, store.MaxWorkspaceLayoutBytes+1))
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := st.SaveWorkspaceLayout(r.Context(), defaultWorkspaceName, string(body)); err != nil {
		if errors.Is(err, store.ErrWorkspaceLayoutInvalid) {
			http.Error(w, `{"error":"layout must be valid JSON under 256 KiB"}`, http.StatusBadRequest)
			return
		}
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// redactWorkspaceLayoutForRemote filters a stored layout blob down to the
// terminal handles the remote caller's snapshot admits. Fail-closed: a blob
// that doesn't parse into the expected shape yields ok=false (the caller then
// serves layout:null) rather than leaking it verbatim.
func redactWorkspaceLayoutForRemote(layout string, visible []LaunchInfo) (string, bool) {
	allowed := make(map[string]bool, len(visible))
	for _, info := range visible {
		allowed[info.ID] = true
	}
	var blob struct {
		V       int                          `json:"v"`
		Docked  []string                     `json:"docked"`
		Layouts map[string][]json.RawMessage `json:"layouts"`
	}
	if err := json.Unmarshal([]byte(layout), &blob); err != nil {
		return "", false
	}
	docked := make([]string, 0, len(blob.Docked))
	for _, t := range blob.Docked {
		if allowed[t] {
			docked = append(docked, t)
		}
	}
	blob.Docked = docked
	for bp, items := range blob.Layouts {
		kept := items[:0]
		for _, raw := range items {
			var it struct {
				I string `json:"i"`
			}
			if json.Unmarshal(raw, &it) == nil && allowed[it.I] {
				kept = append(kept, raw)
			}
		}
		blob.Layouts[bp] = kept
	}
	out, err := json.Marshal(blob)
	if err != nil {
		return "", false
	}
	return string(out), true
}
