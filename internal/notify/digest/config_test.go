package digest

import (
	"testing"
	"time"
)

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"disabled ignores junk", Config{Enabled: false, Frequency: "yearly", SendHour: 99}, false},
		{"enabled empty freq ok", Config{Enabled: true}, false},
		{"enabled weekly ok", Config{Enabled: true, Frequency: "weekly", SendHour: 8}, false},
		{"enabled monthly ok", Config{Enabled: true, Frequency: "monthly", SendHour: 0}, false},
		{"bad freq", Config{Enabled: true, Frequency: "daily"}, true},
		{"bad hour high", Config{Enabled: true, SendHour: 24}, true},
		{"bad hour low", Config{Enabled: true, SendHour: -1}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() err=%v wantErr=%v", err, tt.wantErr)
			}
		})
	}
}

func TestFrequencyAndSendHourDefaults(t *testing.T) {
	if got := (Config{}).FrequencyOrDefault(); got != Weekly {
		t.Errorf("default frequency = %q, want weekly", got)
	}
	if got := (Config{Frequency: "MONTHLY"}).FrequencyOrDefault(); got != Monthly {
		t.Errorf("frequency = %q, want monthly", got)
	}
	if got := (Config{}).SendHourOrDefault(); got != 8 {
		t.Errorf("default send hour = %d, want 8", got)
	}
	if got := (Config{SendHour: 14}).SendHourOrDefault(); got != 14 {
		t.Errorf("send hour = %d, want 14", got)
	}
}

func TestDuePeriodWeekly(t *testing.T) {
	// Wednesday 2026-07-08 15:00 UTC. The current ISO week starts Mon 2026-07-06,
	// so the most-recently-completed week is Mon 2026-06-29 → Sun 2026-07-05.
	now := time.Date(2026, 7, 8, 15, 0, 0, 0, time.UTC)
	p, ready := DuePeriod(Weekly, 8, now)
	if !ready {
		t.Fatalf("expected ready at 15:00 with send_hour=8")
	}
	wantStart := time.Date(2026, 6, 29, 0, 0, 0, 0, time.UTC)
	wantEnd := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)
	if !p.Start.Equal(wantStart) || !p.End.Equal(wantEnd) {
		t.Fatalf("window = [%s,%s), want [%s,%s)", p.Start, p.End, wantStart, wantEnd)
	}
	if p.Key != "2026-W27" {
		t.Errorf("key = %q, want 2026-W27", p.Key)
	}
}

func TestDuePeriodWeeklySendHourGate(t *testing.T) {
	now := time.Date(2026, 7, 8, 6, 0, 0, 0, time.UTC) // 06:00 < send_hour 8
	if _, ready := DuePeriod(Weekly, 8, now); ready {
		t.Fatalf("expected NOT ready at 06:00 with send_hour=8")
	}
}

func TestDuePeriodMonthly(t *testing.T) {
	// 2026-07-03 10:00 UTC → completed month is June 2026.
	now := time.Date(2026, 7, 3, 10, 0, 0, 0, time.UTC)
	p, ready := DuePeriod(Monthly, 8, now)
	if !ready {
		t.Fatalf("expected ready")
	}
	wantStart := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	wantEnd := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	if !p.Start.Equal(wantStart) || !p.End.Equal(wantEnd) {
		t.Fatalf("window = [%s,%s), want [%s,%s)", p.Start, p.End, wantStart, wantEnd)
	}
	if p.Key != "2026-06" {
		t.Errorf("key = %q, want 2026-06", p.Key)
	}
}

func TestDuePeriodMonthlyYearBoundary(t *testing.T) {
	now := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC) // completed month = Dec 2025
	p, _ := DuePeriod(Monthly, 8, now)
	if p.Key != "2025-12" {
		t.Errorf("key = %q, want 2025-12", p.Key)
	}
}
