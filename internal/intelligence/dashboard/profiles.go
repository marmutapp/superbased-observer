package dashboard

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/marmutapp/superbased-observer/internal/config"
)

// Custom-profile CRUD (usability arc P3.4 Settings half / D11).
// Thin HTTP front door over config.ProfileStore — the one write owner
// for user profile files (~/.observer/profiles/<name>.toml). The CLI
// (`observer profile create|delete|set`) drives the same store; both
// front doors share its validation and allow-list rules.
//
// No reload poke is needed on any of these: user-profile content
// stamps ride the proxy router's instance key, so edits apply to NEW
// sessions automatically, and deleting an assigned profile falls back
// to master parameters (warn-once) by router design.

// profileStore returns the store rooted at the canonical user-profiles
// directory for this server's config path.
func (s *Server) profileStore() config.ProfileStore {
	return config.ProfileStore{Dir: config.DefaultProfilesDir(s.opts.ConfigPath)}
}

// handleConfigProfiles serves POST /api/config/profiles — create a
// user profile, optionally seeded from a base profile ("from").
// Mirrors `observer profile create <name> [--from <base>]`.
func (s *Server) handleConfigProfiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.opts.ConfigPath == "" {
		http.Error(w, "config path not configured — server has no profiles directory", http.StatusConflict)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	var req struct {
		Name string `json:"name"`
		From string `json:"from"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}
	ps := s.profileStore()
	if err := ps.Create(req.Name, req.From); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "already exists") {
			status = http.StatusConflict
		}
		http.Error(w, err.Error(), status)
		return
	}
	writeJSON(w, map[string]any{
		"created":       req.Name,
		"profile_names": ps.Names(),
	})
}

// handleConfigProfile serves /api/config/profiles/<name>:
//
//	GET    — the profile's parameters resolved against the master
//	         config (what traffic assigned to it actually runs) plus
//	         the raw TOML body for display.
//	PATCH  — set one dotted compression key ({"key","value"}), the
//	         `observer profile set` equivalent. Built-ins immutable.
//	DELETE — remove a user profile. Built-ins refused.
func (s *Server) handleConfigProfile(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/api/config/profiles/")
	if name == "" || strings.ContainsAny(name, "/\\") {
		http.NotFound(w, r)
		return
	}
	ps := s.profileStore()
	switch r.Method {
	case http.MethodGet:
		cfg, err := loadConfigForDashboard(s.opts.ConfigPath)
		if err != nil {
			writeErr(w, err)
			return
		}
		resolved, _, err := ps.ResolveCompression(cfg.Compression, name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		raw := ""
		if b, rerr := ps.Read(name); rerr == nil {
			raw = string(b)
		}
		writeJSON(w, map[string]any{
			"name":     name,
			"builtin":  config.IsBuiltin(name),
			"editable": !config.IsBuiltin(name),
			"resolved": resolved,
			"raw":      raw,
		})
	case http.MethodPatch:
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
		if err != nil {
			http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
			return
		}
		var req struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := ps.SetKey(name, req.Key, req.Value); err != nil {
			status := http.StatusBadRequest
			if strings.Contains(err.Error(), "unknown profile") {
				status = http.StatusNotFound
			}
			http.Error(w, err.Error(), status)
			return
		}
		writeJSON(w, map[string]any{"set": req.Key, "value": req.Value, "name": name})
	case http.MethodDelete:
		if err := ps.Delete(name); err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, os.ErrNotExist) {
				status = http.StatusNotFound
			}
			http.Error(w, err.Error(), status)
			return
		}
		writeJSON(w, map[string]any{"deleted": name, "profile_names": ps.Names()})
	default:
		http.Error(w, "GET, PATCH or DELETE", http.StatusMethodNotAllowed)
	}
}
