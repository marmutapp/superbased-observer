package kimicode

import (
	"path/filepath"
	"strings"
)

// sessionIDFromPath extracts the canonical session id from a wire-trace
// path. The layout is
// `.../sessions/wd_<slug>_<hash>/session_<uuid>/agents/<name>/wire.jsonl`;
// the session id is the `session_<uuid>` directory component — the SAME id
// carried in `session_index.jsonl.sessionId`. Returns "" when no such
// component is present.
func sessionIDFromPath(path string) string {
	for _, seg := range splitPath(path) {
		if strings.HasPrefix(seg, "session_") {
			return seg
		}
	}
	return ""
}

// agentNameFromPath returns the agent directory name (the component
// immediately after `agents`) — "main" for the primary agent, another
// name for a sub-agent trace. Returns "" when the path has no agents
// segment.
func agentNameFromPath(path string) string {
	segs := splitPath(path)
	for i, seg := range segs {
		if seg == "agents" && i+1 < len(segs) {
			return segs[i+1]
		}
	}
	return ""
}

// stateJSONPath returns the session-root `state.json` sibling for a
// wire-trace path (two directories up from `agents/<name>/wire.jsonl`).
// Returns "" when the path is too shallow to have the expected shape.
func stateJSONPath(wirePath string) string {
	// .../session_<uuid>/agents/<name>/wire.jsonl
	//                    └ dir(agentDir) = session root
	agentDir := filepath.Dir(wirePath)         // agents/<name>
	agentsDir := filepath.Dir(agentDir)        // agents
	sessionDir := filepath.Dir(agentsDir)      // session_<uuid>
	if sessionDir == "" || sessionDir == "." { // guards against too-shallow paths
		return ""
	}
	if filepath.Base(agentsDir) != "agents" {
		return ""
	}
	return filepath.Join(sessionDir, "state.json")
}

// splitPath splits a path into components on both OS separators so the
// comparison works regardless of the recording host's slash convention.
func splitPath(path string) []string {
	norm := strings.ReplaceAll(path, `\`, "/")
	return strings.Split(norm, "/")
}

// firstNonEmpty returns the first non-blank string.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
