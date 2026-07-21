package devin

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/marmutapp/superbased-observer/internal/adapter/transcriptutil"
	"github.com/marmutapp/superbased-observer/internal/models"
)

// ReadTranscript implements handoffsvc.TranscriptReader — the
// session-handoff transcript tier (docs/session-handoff.md). Devin has no
// proxy tier, so the transcript is re-read from its canonical store: the
// message_nodes tree, walked along the ACTIVE chain (sessions.main_chain_id
// up to the root) so regenerated/forked turns don't double-count. The
// rendered ATIF-v1.7 export at cli/transcripts/<id>.json is a convenience
// artifact only and is intentionally not consulted — the SQLite store is
// always present. reasoning/thinking is dropped at read time (plan §8).
func (a *Adapter) ReadTranscript(ctx context.Context, sess models.Session, sourceHints []string) ([]models.TranscriptMessage, error) {
	dbPath, err := a.storeDBPath(sourceHints)
	if err != nil {
		return nil, err
	}
	db, err := openReadOnlyDB(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	s, err := loadSessionMeta(ctx, db, sess.ID)
	if err != nil {
		return nil, err
	}
	chain, _ := loadActiveChain(ctx, db, s)

	// Pre-collect tool-role results so an assistant call resolves to its
	// output regardless of ordering within the chain.
	results := map[string]toolResult{}
	decoded := make([]*chatMessage, len(chain))
	for i, n := range chain {
		var cm chatMessage
		if err := json.Unmarshal([]byte(n.RawMsg), &cm); err != nil {
			continue
		}
		decoded[i] = &cm
		if strings.EqualFold(cm.Role, "tool") && cm.ToolCall != "" {
			results[cm.ToolCall] = toolResult{Content: contentString(cm.Content)}
		}
	}

	b := transcriptutil.New()
	for i, cm := range decoded {
		if cm == nil {
			continue
		}
		ts := secondsToTime(chain[i].Created)
		switch strings.ToLower(cm.Role) {
		case "user":
			b.User(contentString(cm.Content), ts)
		case "assistant":
			model := firstNonEmpty(metaModel(cm.Metadata), s.Model)
			b.AssistantText(contentString(cm.Content), model, ts)
			for _, tc := range cm.ToolCalls {
				b.AssistantCall(tc.ID, tc.Name, string(tc.Arguments), model, ts)
				if res, ok := results[tc.ID]; ok {
					b.Resolve(tc.ID, res.Content, ts)
				}
			}
		}
	}
	return b.Finish(), nil
}

// storeDBPath resolves the sessions.db: a usable hint first, else the
// first watch root holding one.
func (a *Adapter) storeDBPath(hints []string) (string, error) {
	for _, h := range hints {
		if strings.EqualFold(filepath.Base(resolveDBPath(h)), "sessions.db") {
			p := resolveDBPath(h)
			if fileReadable(p) {
				return p, nil
			}
		}
	}
	for _, root := range a.roots {
		p := filepath.Join(root, "sessions.db")
		if fileReadable(p) {
			return p, nil
		}
	}
	return "", errNoStore
}
