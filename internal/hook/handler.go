package hook

import (
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// Decision is the hook-protocol reply. Claude Code expects at minimum a
// "decision" field; "approve" allows the tool call to proceed.
type Decision struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason,omitempty"`
}

// HandleApprove reads a hook payload from stdin, logs the event shape to
// stderr (if a logger is wired up downstream), and always writes an
// {"decision": "approve"} response to stdout. The observer hook is
// intentionally non-blocking — spec P1: "Never break the host tool".
//
// Any error during read/write is swallowed after best-effort logging so we
// never propagate a non-zero exit.
//
// event names the hook channel (pre-tool, post-tool, stop, pre-compact,
// post-compact, cursor:beforeShellExecution, etc.) for diagnostic logging.
func HandleApprove(event string, stdin io.Reader, stdout io.Writer, stderr io.Writer) {
	// Cap stdin reads: hook payloads should be small (<64KB), but we don't
	// want to hang on a pathological writer.
	body, _ := io.ReadAll(io.LimitReader(stdin, 2*1024*1024))
	// Parse minimally to extract the fields we log. Ignore errors — the
	// hook must still reply approve.
	var payload map[string]any
	_ = json.Unmarshal(body, &payload)
	fmt.Fprintf(stderr, "observer-hook: event=%s received bytes=%d at=%s\n",
		event, len(body), time.Now().UTC().Format(time.RFC3339))

	_ = json.NewEncoder(stdout).Encode(Decision{Decision: "approve"})
}
