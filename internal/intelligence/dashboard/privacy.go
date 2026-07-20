package dashboard

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/marmutapp/superbased-observer/internal/scrub"
)

// Privacy center (usability arc P6.5 / review §8.2): the live scrub
// tester. The rest of the page composes existing surfaces — config
// flags, enrolment status, the last-payload transparency view — this
// endpoint adds the one missing primitive: "paste a string, see
// exactly what the scrubber would do to it".

// handlePrivacyScrubTest serves POST /api/privacy/scrub-test
// {"text"} → {"enabled", "scrubbed", "changed"}.
//
// The input is processed entirely in memory and never logged, stored,
// or indexed — it exists for the duration of this request only. The
// scrubber is built fresh from config.toml's [observer.secrets] on
// every call so edits to extra_patterns test immediately; `enabled`
// reports whether the capture pipeline currently applies it (when
// false, the response shows what scrubbing WOULD do once enabled).
func (s *Server) handlePrivacyScrubTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Text string `json:"text"`
	}
	// 1MB cap — pastes beyond that are not a realistic excerpt test.
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		http.Error(w, "bad body: "+err.Error(), http.StatusBadRequest)
		return
	}
	enabled := true
	var extra []string
	if cfg, err := loadConfigForDashboard(s.opts.ConfigPath); err == nil {
		enabled = cfg.Observer.Secrets.EnableScrubbing
		extra = cfg.Observer.Secrets.ExtraPatterns
	}
	scrubbed := scrub.NewWithExtra(extra).String(body.Text)
	writeJSON(w, map[string]any{
		"enabled":  enabled,
		"scrubbed": scrubbed,
		"changed":  scrubbed != body.Text,
	})
}
