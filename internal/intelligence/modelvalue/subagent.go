package modelvalue

import (
	"sort"

	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/routing"
)

// buildSubagentEvidence walks each session's action stream and
// attributes sidechain tool actions to the persona named by the most
// recent subagent_start marker (claude-code's agent-name event, the
// clinecli agent_id — both land as SubagentName on the loader's
// ActionRow). The persona's model is the dominant model of the sessions
// it ran in, weighted by attributed actions — an approximation, stated
// as such in the §R10.3 rationale, because token rows carry no
// sidechain flag.
func buildSubagentEvidence(turns []TurnRow, actionsBySession map[string][]ActionRow) []routing.SubagentEvidence {
	// Dominant model per session (by turn count; ties → lexicographic).
	sessionModel := map[string]string{}
	{
		counts := map[string]map[string]int{}
		for _, tr := range turns {
			if counts[tr.SessionID] == nil {
				counts[tr.SessionID] = map[string]int{}
			}
			counts[tr.SessionID][tr.Model]++
		}
		for sid, byModel := range counts {
			best, bestN := "", -1
			for m, n := range byModel {
				if n > bestN || (n == bestN && m < best) {
					best, bestN = m, n
				}
			}
			sessionModel[sid] = best
		}
	}

	type agg struct {
		ev          routing.SubagentEvidence
		sessions    map[string]bool
		modelWeight map[string]int64 // session-dominant model → attributed actions
	}
	byName := map[string]*agg{}
	order := []string{}

	sids := make([]string, 0, len(actionsBySession))
	for sid := range actionsBySession {
		sids = append(sids, sid)
	}
	sort.Strings(sids)

	for _, sid := range sids {
		current := ""
		for _, a := range actionsBySession[sid] {
			if a.Type == models.ActionSubagentStart && a.SubagentName != "" {
				current = a.SubagentName
				continue
			}
			if !a.IsSidechain || current == "" || !toolActionTypes[a.Type] {
				continue
			}
			g, ok := byName[current]
			if !ok {
				g = &agg{
					ev:          routing.SubagentEvidence{Name: current},
					sessions:    map[string]bool{},
					modelWeight: map[string]int64{},
				}
				byName[current] = g
				order = append(order, current)
			}
			g.sessions[sid] = true
			g.modelWeight[sessionModel[sid]]++
			g.ev.Actions++
			if !a.Success {
				g.ev.Failures++
			}
			switch a.Type {
			case models.ActionReadFile, models.ActionSearchText, models.ActionSearchFiles,
				models.ActionWebSearch, models.ActionWebFetch:
				g.ev.Reads++
			case models.ActionWriteFile, models.ActionEditFile:
				g.ev.Mutations++
			case models.ActionRunCommand:
				g.ev.Commands++
			}
		}
	}

	sort.Strings(order)
	out := make([]routing.SubagentEvidence, 0, len(order))
	for _, name := range order {
		g := byName[name]
		g.ev.Sessions = len(g.sessions)
		best, bestN := "", int64(-1)
		for m, n := range g.modelWeight {
			if m == "" {
				continue
			}
			if n > bestN || (n == bestN && m < best) {
				best, bestN = m, n
			}
		}
		g.ev.Model = best
		out = append(out, g.ev)
	}
	return out
}
