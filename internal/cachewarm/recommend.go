package cachewarm

// KeepWarmAction is the closed set of keep-warm levers Recommend can
// suggest. Stable across releases (surfaces map these to pill copy).
type KeepWarmAction string

const (
	// ActionNone — do nothing. Either keep-warm is off, the cache is not
	// worth it, the session is unlikely to resume, or no content-free
	// lever exists for this provider.
	ActionNone KeepWarmAction = "none"
	// ActionUse1hTier — switch the cache_control breakpoint to the 1h TTL
	// tier (Anthropic). The cheapest CONTENT-FREE lever: a one-time
	// client-side request-shape change, no daemon pinging. The dominant
	// recommendation for Claude Code.
	ActionUse1hTier KeepWarmAction = "use_1h_tier"
	// ActionPatchTTL — extend the TTL via the provider's cache-update API
	// (Gemini explicit CachedContent). Content-free; needs an addressable
	// cache id captured at create-time.
	ActionPatchTTL KeepWarmAction = "patch_ttl"
	// ActionReplayPing — the proxy replays the last request body (held in
	// memory) with max_tokens:1 to force a cache read. The ONLY true idle
	// auto-fire; enforce-mode + proxied + Anthropic only (see the plan
	// §B.2). Not a content-free lever.
	ActionReplayPing KeepWarmAction = "replay_ping"
)

// RecommendInput is the per-window keep-warm decision input. Value and
// capability flags come from the already-assembled CacheWindow; the
// idle/economics signals (ResumeConfidence, Mode, Proxied) are resolved
// at the boundary. Pure — no clock, no I/O.
type RecommendInput struct {
	// Window is the cache under consideration (carries PrefixTokens,
	// TTLTier, ValueAtRiskUSD, and the capability flags).
	Window CacheWindow
	// ResumeConfidence is the boundary's 0..1 estimate that the session
	// will actually send another matching turn. A warm cache nobody
	// returns to is wasted spend, so this gates every bet.
	ResumeConfidence float64
	// MinValueUSD is the keep-warm value floor ([cachewarm.keepwarm]
	// min_value_usd). Below it the answer is always ActionNone.
	MinValueUSD float64
	// MinResumeConfidence gates the bet on resume likelihood
	// ([cachewarm.keepwarm] min_resume_confidence).
	MinResumeConfidence float64
	// Mode is "off" | "advise" | "enforce" ([cachewarm.keepwarm] mode).
	// off → always ActionNone. advise → content-free levers only
	// (use_1h_tier / patch_ttl). enforce → additionally permits
	// replay_ping when Proxied.
	Mode string
	// Proxied reports the session routes through the proxy, so the
	// in-memory replay path is feasible. Only consulted in enforce mode.
	Proxied bool
}

// Recommendation is Recommend's verdict for one cache window.
type Recommendation struct {
	// Action is the suggested lever.
	Action KeepWarmAction `json:"action"`
	// PaysOff is true when the recommended action is expected to save
	// money over letting the cache go cold (given ResumeConfidence).
	PaysOff bool `json:"pays_off"`
	// ProjectedSavingsUSD is the rough expected saving:
	// ValueAtRiskUSD × ResumeConfidence. Zero for ActionNone.
	ProjectedSavingsUSD float64 `json:"projected_savings_usd"`
	// ResumeConfidence echoes the input for the surface.
	ResumeConfidence float64 `json:"resume_confidence"`
	// Rationale is a short, surface-ready explanation of the verdict.
	Rationale string `json:"rationale"`
}

// Recommend returns the cheapest keep-warm lever for one cache window, or
// ActionNone with a reason. Pure function; deterministic.
//
// Decision order (first match wins):
//  1. mode "off"/empty                          → none ("keep-warm disabled")
//  2. value below floor                         → none ("cache value below threshold")
//  3. resume confidence below floor             → none ("session unlikely to resume")
//  4. Gemini explicit (RefreshableByPatch)      → patch_ttl (content-free)
//  5. Anthropic on 5m tier (Supports1hTier)     → use_1h_tier (content-free)
//  6. enforce + proxied                         → replay_ping (in-memory replay)
//  7. otherwise (e.g. OpenAI implicit, advise)  → none ("no content-free lever")
func Recommend(in RecommendInput) Recommendation {
	rec := Recommendation{ResumeConfidence: in.ResumeConfidence}

	if in.Mode == "" || in.Mode == "off" {
		rec.Action = ActionNone
		rec.Rationale = "keep-warm disabled"
		return rec
	}
	if in.Window.ValueAtRiskUSD < in.MinValueUSD {
		rec.Action = ActionNone
		rec.Rationale = "cache value below keep-warm threshold"
		return rec
	}
	if in.ResumeConfidence < in.MinResumeConfidence {
		rec.Action = ActionNone
		rec.Rationale = "session unlikely to resume — keep-warm would be wasted spend"
		return rec
	}

	savings := in.Window.ValueAtRiskUSD * in.ResumeConfidence

	switch {
	case in.Window.RefreshableByPatch:
		rec.Action = ActionPatchTTL
		rec.PaysOff = true
		rec.ProjectedSavingsUSD = savings
		rec.Rationale = "extend the cache TTL via the provider API (content-free)"
	case in.Window.Supports1hTier && in.Window.TTLTier != "1h":
		rec.Action = ActionUse1hTier
		rec.PaysOff = true
		rec.ProjectedSavingsUSD = savings
		rec.Rationale = "switch this cache to the 1h TTL tier — a one-time client-side change, no pinging"
	case in.Mode == "enforce" && in.Proxied:
		rec.Action = ActionReplayPing
		rec.PaysOff = true
		rec.ProjectedSavingsUSD = savings
		rec.Rationale = "proxy will replay the last request to refresh the cache before it expires"
	default:
		rec.Action = ActionNone
		// Anthropic already on 1h, or a provider with no content-free
		// lever (OpenAI implicit) outside enforce/proxied.
		if in.Window.Supports1hTier {
			rec.Rationale = "already on the longest content-free tier — resume soon or accept a re-write"
		} else {
			rec.Rationale = "no content-free keep-warm lever for this provider — resume soon or accept a re-write"
		}
	}
	return rec
}
