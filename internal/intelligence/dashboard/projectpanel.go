package dashboard

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/fsview"
	"github.com/marmutapp/superbased-observer/internal/gitview"
)

// projectPanelReadCap is the per-file read cap the project-panel file viewer
// applies (fsview's default 256KB). A file larger than this is returned
// truncated with too_large set.
const projectPanelReadCap = fsview.DefaultMaxReadBytes

// handleTerminalProject serves the per-terminal project panel (Arc A):
//
//	GET /api/terminal/project/<token>              → meta
//	GET /api/terminal/project/<token>/files?path=… → directory listing
//	GET /api/terminal/project/<token>/file?path=…  → file contents
//	GET /api/terminal/project/<token>/git          → git snapshot
//
// The browser never sends a filesystem root: the token (a live launch handle)
// resolves — server-side — to the canonical project root the run was launched
// with, from state retained at spawn. The browsable set is exactly {live
// terminal runs with a known root}. All endpoints are GET-only and View-tier.
//
// Errors are JSON {"error": code}: 404 unknown_token (unknown/exited token),
// 409 no_project_root (live run launched with the default cwd), 403
// remote_view_disabled (remote-exposed caller without allow_terminal_view), 400
// bad_path (traversal/absolute/other), 404 not_found (missing path). Absolute
// filesystem paths of failures are NEVER echoed in an error body; the meta
// payload's root field is the one sanctioned place the canonical path appears.
func (s *Server) handleTerminalProject(w http.ResponseWriter, r *http.Request) {
	// A nil resolver IS the disabled state (panel not wired) — 404 like the
	// other nil-seam terminal surfaces, so the endpoint's existence is not even
	// confirmable.
	if s.opts.ProjectRootResolver == nil {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		writeProjectError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}

	rest := strings.TrimPrefix(r.URL.Path, "/api/terminal/project/")
	token, sub, _ := strings.Cut(rest, "/")
	if token == "" {
		writeProjectError(w, http.StatusNotFound, "unknown_token")
		return
	}

	// Remote gate FIRST (finding 1): file contents/paths are at least as
	// sensitive as terminal output, so a remote-exposed caller is refused unless
	// the owner has turned on [remote].allow_terminal_view — the exact model
	// handleLaunchWS uses. This runs BEFORE ProjectRootResolver so a remote-gated
	// caller gets an IDENTICAL 403 for every real token (unknown, rootless, or
	// rooted) and the response can't be used as a token/root-state oracle.
	if remoteExposedFromContext(r.Context()) && !s.allowTerminalView() {
		writeProjectError(w, http.StatusForbidden, "remote_view_disabled")
		return
	}

	root, known := s.opts.ProjectRootResolver(token)
	if !known {
		// Unknown or exited token — indistinguishable by design (§ token-scoped,
		// server-resolved roots).
		writeProjectError(w, http.StatusNotFound, "unknown_token")
		return
	}
	if root == "" {
		writeProjectError(w, http.StatusConflict, "no_project_root")
		return
	}

	switch sub {
	case "":
		s.projectMeta(w, r, root)
	case "files":
		s.projectFiles(w, r, root)
	case "file":
		s.projectFile(w, r, root)
	case "git":
		s.projectGit(w, r, root)
	default:
		writeProjectError(w, http.StatusNotFound, "not_found")
	}
}

// projectMetaResp is the GET /api/terminal/project/<token> payload. root is the
// canonical project root (the one sanctioned place a path is disclosed; a
// remote caller reaching here already passed the allow_terminal_view gate).
type projectMetaResp struct {
	Root         string `json:"root"`
	GitAvailable bool   `json:"git_available"`
	IsGit        bool   `json:"is_git"`
	Branch       string `json:"branch"`
}

func (s *Server) projectMeta(w http.ResponseWriter, r *http.Request, root string) {
	gitAvailable := true
	var gi gitview.Info
	info, err := gitview.Snapshot(r.Context(), root)
	switch {
	case errors.Is(err, gitview.ErrGitUnavailable):
		gitAvailable = false
	case err == nil:
		gi = info
	}
	writeJSON(w, projectMetaResp{
		Root:         root,
		GitAvailable: gitAvailable,
		IsGit:        gi.IsGit,
		Branch:       gi.Branch,
	})
}

// projectEntry mirrors an fsview.Entry on the wire (relative names only).
type projectEntry struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	Size  int64  `json:"size"`
	MTime string `json:"mtime"`
}

type projectFilesResp struct {
	Path      string         `json:"path"`
	Entries   []projectEntry `json:"entries"`
	Truncated bool           `json:"truncated"`
}

func (s *Server) projectFiles(w http.ResponseWriter, r *http.Request, root string) {
	rel := r.URL.Query().Get("path")
	entries, truncated, err := fsview.List(r.Context(), root, rel)
	if err != nil {
		status, code := projectFsErr(err)
		writeProjectError(w, status, code)
		return
	}
	out := make([]projectEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, projectEntry{
			Name:  e.Name,
			Type:  string(e.Type),
			Size:  e.Size,
			MTime: e.ModTime.UTC().Format(time.RFC3339),
		})
	}
	writeJSON(w, projectFilesResp{Path: rel, Entries: out, Truncated: truncated})
}

type projectFileResp struct {
	Path      string `json:"path"`
	Content   string `json:"content"`
	Size      int64  `json:"size"`
	Truncated bool   `json:"truncated"`
	Binary    bool   `json:"binary"`
	TooLarge  bool   `json:"too_large"`
}

func (s *Server) projectFile(w http.ResponseWriter, r *http.Request, root string) {
	rel := r.URL.Query().Get("path")
	c, err := fsview.Read(r.Context(), root, rel, projectPanelReadCap)
	if err != nil {
		status, code := projectFsErr(err)
		writeProjectError(w, status, code)
		return
	}
	writeJSON(w, projectFileResp{
		Path:      rel,
		Content:   c.Data,
		Size:      c.Size,
		Truncated: c.Truncated,
		Binary:    c.Binary,
		// too_large mirrors truncated: the file exceeded the 256KB viewer cap and
		// only its leading bytes are returned.
		TooLarge: c.Truncated,
	})
}

func (s *Server) projectGit(w http.ResponseWriter, r *http.Request, root string) {
	info, err := gitview.Snapshot(r.Context(), root)
	if errors.Is(err, gitview.ErrGitUnavailable) {
		// Signal unavailability inside the git payload (the meta endpoint carries
		// the git_available:false flag the frontend keys off). Emit empty arrays,
		// not null/absent, so the frontend's array contract holds (finding 7).
		writeJSON(w, map[string]any{
			"is_git": false, "error": "git_unavailable",
			"status": []any{}, "log": []any{},
		})
		return
	}
	if err != nil {
		// Any other git-level error → report a clean not-a-repo snapshot rather
		// than leaking the error (which could carry the absolute root). EmptyInfo
		// carries non-nil empty Status/Log so the wire shape stays array-typed.
		writeJSON(w, gitview.EmptyInfo())
		return
	}
	writeJSON(w, info)
}

// projectFsErr maps an fsview sentinel to an HTTP status + wire error code.
// Non-sentinel errors collapse to bad_path so an absolute path is never echoed.
func projectFsErr(err error) (int, string) {
	switch {
	case errors.Is(err, fsview.ErrNotFound):
		return http.StatusNotFound, "not_found"
	case errors.Is(err, fsview.ErrOutsideRoot),
		errors.Is(err, fsview.ErrAbsolutePath),
		errors.Is(err, fsview.ErrNotDir),
		errors.Is(err, fsview.ErrIsDir),
		errors.Is(err, fsview.ErrNotRegular):
		return http.StatusBadRequest, "bad_path"
	default:
		return http.StatusBadRequest, "bad_path"
	}
}

// writeProjectError emits a JSON {"error": code} body with the given status.
func writeProjectError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"error":"` + code + `"}` + "\n"))
}
