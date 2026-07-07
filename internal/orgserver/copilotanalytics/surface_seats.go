package copilotanalytics

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"time"
)

// seatsPerPage is the max page size for /copilot/billing/seats (GitHub caps at 100).
const seatsPerPage = 100

// pollSeats reads the seat subscription baseline: the org seat-breakdown summary
// (org-aggregate counts) + the per-seat list (per-login activity). Seats are
// COUNTS, not dollars — the per-seat price ($19/$39) is applied at CostSummary,
// never stored here. (Enterprise has no /copilot/billing summary — preview
// /billing/seats only — so the summary is skipped for enterprise owners.)
func pollSeats(ctx context.Context, p *Poller, win window) ([]DailyMetric, error) {
	var out []DailyMetric

	// Snapshot day = the last full day in the window (seat counts are
	// point-in-time; the upsert overwrites the snapshot on each re-poll).
	snapDay := dayOf(win.End.Add(-time.Second))

	if p.OwnerType == OwnerOrg {
		summary, err := p.fetchSeatSummary(ctx)
		if err != nil {
			return nil, err
		}
		out = append(
			out,
			emitMetric(snapDay, orgAggregateKey, ActorOrg, SurfaceSeats, UnitSeats, MetricSeatsTotal, float64(summary.SeatBreakdown.Total)),
			emitMetric(snapDay, orgAggregateKey, ActorOrg, SurfaceSeats, UnitSeats, MetricSeatsActive, float64(summary.SeatBreakdown.ActiveThisCycle)),
			emitMetric(snapDay, orgAggregateKey, ActorOrg, SurfaceSeats, UnitSeats, MetricSeatsInactive, float64(summary.SeatBreakdown.InactiveThisCycle)),
		)
	}

	seats, err := p.fetchAllSeats(ctx)
	if err != nil {
		return nil, err
	}
	for _, st := range seats {
		if st.Assignee.Login == "" || st.LastActivityAt == "" {
			continue
		}
		day := utcDayFromTimestamp(st.LastActivityAt)
		if day == "" {
			continue
		}
		// Only attribute activity that falls inside the polled window.
		if t, err := time.Parse(time.RFC3339, st.LastActivityAt); err == nil {
			if t.Before(win.Start) || !t.Before(win.End) {
				continue
			}
		}
		actor := actorForLogin(st.Assignee.Login, st.Assignee.Type)
		out = append(out, emitMetric(day, st.Assignee.Login, actor, SurfaceSeats, UnitCount, MetricActiveSeat, 1))
	}
	return out, nil
}

// seatSummary is the /orgs/{org}/copilot/billing response (counts only, no $).
type seatSummary struct {
	SeatBreakdown struct {
		Total             int `json:"total"`
		AddedThisCycle    int `json:"added_this_cycle"`
		ActiveThisCycle   int `json:"active_this_cycle"`
		InactiveThisCycle int `json:"inactive_this_cycle"`
	} `json:"seat_breakdown"`
	PlanType string `json:"plan_type"`
}

func (p *Poller) fetchSeatSummary(ctx context.Context) (seatSummary, error) {
	body, _, err := p.get(ctx, p.BaseURL+p.ownerPrefix()+"/copilot/billing")
	if err != nil {
		return seatSummary{}, err
	}
	var s seatSummary
	if err := json.Unmarshal(body, &s); err != nil {
		return seatSummary{}, fmt.Errorf("copilotanalytics: parse seat summary: %w", err)
	}
	return s, nil
}

// seatRow is one /copilot/billing/seats entry. Identity = assignee.login (NOT
// email). last_activity_at drives the active-seat day attribution.
type seatRow struct {
	Assignee struct {
		Login string `json:"login"`
		ID    int64  `json:"id"`
		Type  string `json:"type"`
	} `json:"assignee"`
	LastActivityAt     string `json:"last_activity_at"`
	LastActivityEditor string `json:"last_activity_editor"`
	PlanType           string `json:"plan_type"`
}

// seatsPage is the paginated seats envelope: a total + the page's rows.
type seatsPage struct {
	TotalSeats int       `json:"total_seats"`
	Seats      []seatRow `json:"seats"`
}

// fetchAllSeats walks the seats pages until a short page (GitHub uses Link-header
// pagination; a page shorter than per_page is the terminal page — robust without
// parsing the Link header for the small seat volumes here).
func (p *Poller) fetchAllSeats(ctx context.Context) ([]seatRow, error) {
	var all []seatRow
	for page := 1; ; page++ {
		q := url.Values{}
		q.Set("per_page", fmt.Sprintf("%d", seatsPerPage))
		q.Set("page", fmt.Sprintf("%d", page))
		body, _, err := p.get(ctx, p.BaseURL+p.ownerPrefix()+"/copilot/billing/seats?"+q.Encode())
		if err != nil {
			return nil, err
		}
		var pg seatsPage
		if err := json.Unmarshal(body, &pg); err != nil {
			return nil, fmt.Errorf("copilotanalytics: parse seats page %d: %w", page, err)
		}
		all = append(all, pg.Seats...)
		if len(pg.Seats) < seatsPerPage {
			break
		}
	}
	return all, nil
}
