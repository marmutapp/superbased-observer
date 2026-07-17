package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/guard"
	"github.com/marmutapp/superbased-observer/internal/guard/mcpsec"
	"github.com/marmutapp/superbased-observer/internal/mcp/locate"
	"github.com/marmutapp/superbased-observer/internal/policy"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// MCP security orchestration (guard spec §9 / G10). mcpsecRunner is
// the I/O sequencer between the pure mcpsec logic, the guard engine
// and the store's one-owner guard helpers — the guardScannerAdapter
// pattern: mcpsec computes, guard evaluates, the store persists, and
// THIS file only sequences. Three triggers share one runner flow:
// the `observer start` baseline scan, the watcher-ingest config-change
// re-scan (debounced), and the `observer guard mcp scan` CLI.

// mcpScanMu serializes scan/observe passes per process: the proxy's
// decl observation and a watcher-triggered re-scan may otherwise
// interleave their read-diff-write cycles and double-emit findings.
var mcpScanMu sync.Mutex

// mcpsecRunner sequences one store+guard pair through mcpsec flows.
type mcpsecRunner struct {
	st     *store.Store
	g      *guard.Guard
	logger *slog.Logger
	home   string
	// pinning / poisoning mirror [guard.mcp] (config gates the work;
	// the R-30x rules additionally answer to [guard.rules] disable).
	pinning   bool
	poisoning bool
}

// newMCPSecRunner builds a runner; home resolution failure degrades
// to an inert runner (nil), logged — never a daemon failure.
func newMCPSecRunner(cfg configGuardMCP, st *store.Store, g *guard.Guard, logger *slog.Logger) *mcpsecRunner {
	if st == nil || g == nil || (!cfg.Pinning && !cfg.PoisoningHeuristics) {
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		logger.Warn("guard mcp: home dir unavailable; MCP inventory inert", "err", err)
		return nil
	}
	return &mcpsecRunner{
		st: st, g: g, logger: logger, home: home,
		pinning: cfg.Pinning, poisoning: cfg.PoisoningHeuristics,
	}
}

// configGuardMCP is the [guard.mcp] subset the runner needs — a tiny
// local mirror so newMCPSecRunner doesn't take the whole config.
type configGuardMCP struct {
	Pinning             bool
	PoisoningHeuristics bool
}

// mcpScanSummary reports one config-scan pass for CLI output.
type mcpScanSummary struct {
	Servers        int
	NewPins        int
	Findings       []string // "R-305 github (claude-code): ..." lines
	EventsRecorded int
	Issues         []string
}

// loadPins loads + decodes the mcp_server pin rows into mcpsec's
// plain shape. Undecodable pin hashes arrive as zero PinHash values —
// mcpsec re-pins those silently by contract.
func (r *mcpsecRunner) loadPins(ctx context.Context) ([]mcpsec.Pin, error) {
	rows, err := r.st.LoadGuardPins(ctx, "mcp_server")
	if err != nil {
		return nil, err
	}
	pins := make([]mcpsec.Pin, 0, len(rows))
	for _, row := range rows {
		h, _ := mcpsec.DecodePinHash(row.PinHash)
		pins = append(pins, mcpsec.Pin{
			Client: row.Client, Name: row.Name, Hash: h, Status: row.Status,
		})
	}
	return pins, nil
}

// apply persists pin updates through the store's one-owner helpers
// and routes findings through the real engine to audit rows + alerts.
// Partial failures are WARN-and-continue (§17.4: a guard persistence
// problem never fails the surrounding flow). Returns rows recorded.
func (r *mcpsecRunner) apply(ctx context.Context, updates []mcpsec.PinUpdate, findings []policy.MCPFinding) int {
	now := time.Now().UTC()
	for _, u := range updates {
		if err := r.st.UpsertGuardPin(ctx, store.GuardPinRow{
			Kind: "mcp_server", Name: u.Name, Client: u.Client,
			PinHash:   mcpsec.EncodePinHash(u.Hash),
			FirstSeen: now, LastVerified: now, Status: u.Status,
		}); err != nil {
			r.logger.Warn("guard mcp: pin upsert failed", "server", u.Name, "client", u.Client, "err", err)
		}
	}
	if len(findings) == 0 {
		return 0
	}
	verdicts := r.g.EvaluateMCPFindings(findings, now)
	if len(verdicts) == 0 {
		return 0
	}
	n, err := r.st.PersistGuardVerdicts(ctx, verdicts)
	if err != nil {
		r.logger.Warn("guard mcp: verdict persist failed", "recorded", n, "err", err)
	}
	for i := range verdicts {
		r.g.MaybeAlert(verdicts[i])
	}
	return n
}

// ScanConfigs runs one §9.2 config-scan pass: inventory every
// locate-table client config, diff against pins, persist. No-op
// (zero summary) when pinning is off.
func (r *mcpsecRunner) ScanConfigs(ctx context.Context) mcpScanSummary {
	var sum mcpScanSummary
	if r == nil || !r.pinning {
		return sum
	}
	mcpScanMu.Lock()
	defer mcpScanMu.Unlock()

	servers, issues := mcpsec.Inventory(locate.Locations(r.home), nil)
	sum.Servers = len(servers)
	sum.Issues = issues
	for _, issue := range issues {
		r.logger.Warn("guard mcp: inventory issue", "issue", issue)
	}
	pins, err := r.loadPins(ctx)
	if err != nil {
		r.logger.Warn("guard mcp: pin load failed; scan skipped", "err", err)
		sum.Issues = append(sum.Issues, err.Error())
		return sum
	}
	findings, updates := mcpsec.DiffConfigs(servers, pins)
	for _, u := range updates {
		if u.First {
			sum.NewPins++
		}
	}
	for _, f := range findings {
		sum.Findings = append(sum.Findings, findingLine(f))
	}
	sum.EventsRecorded = r.apply(ctx, updates, findings)
	return sum
}

// ObserveDecls runs the §9.2 tools-half observation for one request's
// MCP declarations (handed across by the proxy seam, already
// session-deduped). Called off the request path (goroutine in the
// scanner adapter).
func (r *mcpsecRunner) ObserveDecls(ctx context.Context, decls []mcpsec.ToolDecl) {
	if r == nil || len(decls) == 0 {
		return
	}
	mcpScanMu.Lock()
	defer mcpScanMu.Unlock()

	byServer := map[string][]mcpsec.ToolDecl{}
	var servers []string
	for _, d := range decls {
		if _, ok := byServer[d.Server]; !ok {
			servers = append(servers, d.Server)
		}
		byServer[d.Server] = append(byServer[d.Server], d)
	}
	sort.Strings(servers)

	var allFindings []policy.MCPFinding
	var allUpdates []mcpsec.PinUpdate
	if r.pinning {
		pins, err := r.loadPins(ctx)
		if err != nil {
			r.logger.Warn("guard mcp: pin load failed; observation skipped", "err", err)
			return
		}
		for _, s := range servers {
			findings, updates := mcpsec.DiffTools(s, byServer[s], pins)
			allFindings = append(allFindings, filterPoisoning(findings, r.poisoning)...)
			allUpdates = append(allUpdates, updates...)
		}
	} else {
		// Poisoning-only posture ([guard.mcp] pinning=false): no pin
		// state to change-gate on, so analyze each (session-deduped)
		// observation directly and persist nothing.
		for _, s := range servers {
			for _, hit := range mcpsec.AnalyzeTools(byServer[s]) {
				allFindings = append(allFindings, policy.MCPFinding{
					Kind: policy.MCPFindingPoisoning, Server: hit.Server, Client: mcpsec.ClientObserved,
					Detail: hit.Heuristic + " on tool " + hit.Tool + ": " + hit.Detail,
				})
			}
		}
	}
	if n := r.apply(ctx, allUpdates, allFindings); n > 0 {
		r.logger.Info("guard mcp: tool observation recorded", "servers", len(servers), "events", n)
	}
}

// filterPoisoning drops poisoning findings when the heuristics are
// configured off (pin/drift findings always pass).
func filterPoisoning(findings []policy.MCPFinding, poisoning bool) []policy.MCPFinding {
	if poisoning {
		return findings
	}
	out := findings[:0]
	for _, f := range findings {
		if f.Kind != policy.MCPFindingPoisoning {
			out = append(out, f)
		}
	}
	return out
}

// findingLine renders one finding for CLI summaries.
func findingLine(f policy.MCPFinding) string {
	rule := map[string]string{
		policy.MCPFindingNewServer:        "R-301",
		policy.MCPFindingDescriptionDrift: "R-302",
		policy.MCPFindingPoisoning:        "R-303",
		policy.MCPFindingBinaryChanged:    "R-305",
	}[f.Kind]
	return fmt.Sprintf("%s %s (%s): %s", rule, f.Server, f.Client, f.Detail)
}

// debouncedMCPRescan coalesces watcher-triggered re-scan bursts (an
// editor save loop touching ~/.claude.json shouldn't run N scans).
func debouncedMCPRescan(r *mcpsecRunner, wait time.Duration) func() {
	var mu sync.Mutex
	var timer *time.Timer
	return func() {
		mu.Lock()
		defer mu.Unlock()
		if timer != nil {
			timer.Stop()
		}
		timer = time.AfterFunc(wait, func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			sum := r.ScanConfigs(ctx)
			if len(sum.Findings) > 0 {
				r.logger.Info("guard mcp: config-change re-scan", "servers", sum.Servers, "findings", len(sum.Findings))
			}
		})
	}
}

// ---- CLI surfaces (§11.1: observer guard mcp list|scan|approve|report) ----

func newGuardMCPCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "MCP server inventory, pinning and approval (guard spec §9)",
		Long: "Inventories every supported client's MCP config (claude-code,\n" +
			"cursor, codex), pins each server on first sight, detects rug-pull\n" +
			"and binary-swap drift, and runs poisoning heuristics over tool\n" +
			"descriptions observed on the proxy path. Approved servers stop\n" +
			"marking mcp_unpinned taint.",
	}
	cmd.AddCommand(newGuardMCPListCmd())
	cmd.AddCommand(newGuardMCPScanCmd())
	cmd.AddCommand(newGuardMCPApproveCmd())
	cmd.AddCommand(newGuardMCPReportCmd())
	cmd.AddCommand(newGuardMCPReputationCmd())
	return cmd
}

// buildMCPRunnerCLI assembles config + db + guard + runner for the
// CLI surfaces. The caller owns cleanup.
func buildMCPRunnerCLI(ctx context.Context, configPath string) (*mcpsecRunner, *store.Store, func(), error) {
	cfg, g, err := buildCLIGuard(configPath)
	if err != nil {
		return nil, nil, nil, err
	}
	database, err := db.Open(ctx, db.Options{Path: cfg.Observer.DBPath})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("open db %s: %w", cfg.Observer.DBPath, err)
	}
	st := store.New(database)
	logger := newLogger(cfg.Observer.LogLevel)
	r := newMCPSecRunner(configGuardMCP{
		Pinning:             cfg.Guard.MCP.Pinning,
		PoisoningHeuristics: cfg.Guard.MCP.PoisoningHeuristics,
	}, st, g, logger)
	if r == nil {
		_ = database.Close()
		return nil, nil, nil, fmt.Errorf("guard mcp is disabled ([guard.mcp] pinning and poisoning_heuristics both false) or home is unresolvable")
	}
	return r, st, func() { _ = database.Close() }, nil
}

func newGuardMCPListCmd() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "Inventory: configured MCP servers joined with their pin status",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			r, _, cleanup, err := buildMCPRunnerCLI(cmd.Context(), configPath)
			if err != nil {
				return err
			}
			defer cleanup()

			servers, issues := mcpsec.Inventory(locate.Locations(r.home), nil)
			pins, err := r.loadPins(cmd.Context())
			if err != nil {
				return fmt.Errorf("load pins: %w", err)
			}
			type key struct{ client, name string }
			pinned := map[key]mcpsec.Pin{}
			for _, p := range pins {
				pinned[key{p.Client, p.Name}] = p
			}
			fmt.Fprintf(out, "%-12s %-20s %-7s %-10s %-6s %s\n", "CLIENT", "SERVER", "TRANS", "STATUS", "TOOLS", "COMMAND")
			seen := map[key]bool{}
			for _, s := range servers {
				k := key{s.Client, s.Name}
				seen[k] = true
				status, tools := "unpinned", "-"
				if p, ok := pinned[k]; ok {
					status = p.Status
					if p.Hash.Tools != "" {
						tools = "seen"
					}
				}
				fmt.Fprintf(out, "%-12s %-20s %-7s %-10s %-6s %s\n",
					s.Client, s.Name, s.Transport, status, tools, truncateMCP(s.Command, 48))
			}
			for _, p := range pins {
				k := key{p.Client, p.Name}
				if seen[k] {
					continue
				}
				tools := "-"
				if p.Hash.Tools != "" {
					tools = "seen"
				}
				fmt.Fprintf(out, "%-12s %-20s %-7s %-10s %-6s %s\n",
					p.Client, p.Name, "-", p.Status, tools, "(pinned but absent from config)")
			}
			for _, issue := range issues {
				fmt.Fprintf(out, "ISSUE: %s\n", issue)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to config.toml")
	return cmd
}

func newGuardMCPScanCmd() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "scan",
		Short: "Scan client MCP configs now: pin new servers, detect drift",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			r, _, cleanup, err := buildMCPRunnerCLI(cmd.Context(), configPath)
			if err != nil {
				return err
			}
			defer cleanup()
			sum := r.ScanConfigs(cmd.Context())
			fmt.Fprintf(out, "scanned: %d server(s), %d newly pinned, %d finding(s), %d audit row(s)\n",
				sum.Servers, sum.NewPins, len(sum.Findings), sum.EventsRecorded)
			for _, line := range sum.Findings {
				fmt.Fprintf(out, "  %s\n", line)
			}
			for _, issue := range sum.Issues {
				fmt.Fprintf(out, "  ISSUE: %s\n", issue)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to config.toml")
	return cmd
}

func newGuardMCPApproveCmd() *cobra.Command {
	var configPath, client string
	cmd := &cobra.Command{
		Use:   "approve <server>",
		Short: "Approve (re-pin) an MCP server: trusted servers stop marking taint",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			name := args[0]
			r, st, cleanup, err := buildMCPRunnerCLI(cmd.Context(), configPath)
			if err != nil {
				return err
			}
			defer cleanup()
			// Fresh scan first so approval covers the LIVE config state,
			// not a stale pin (§9.2 "approve re-pins").
			r.ScanConfigs(cmd.Context())
			n, err := st.UpdateGuardPinStatus(cmd.Context(), "mcp_server", name, client, "approved", time.Now().UTC())
			if err != nil {
				return err
			}
			if n == 0 {
				return fmt.Errorf("no pin for server %q — check `observer guard mcp list` for the exact name", name)
			}
			fmt.Fprintf(out, "approved %d pin(s) for %s; its results no longer mark mcp_unpinned taint\n", n, name)
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to config.toml")
	cmd.Flags().StringVar(&client, "client", "", "limit approval to one client's pin (default: all clients)")
	return cmd
}

func newGuardMCPReportCmd() *cobra.Command {
	var configPath string
	var windowDays int
	cmd := &cobra.Command{
		Use:   "report",
		Short: "MCP compliance summary: inventory, pin posture, finding counts (§14.4)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			r, st, cleanup, err := buildMCPRunnerCLI(cmd.Context(), configPath)
			if err != nil {
				return err
			}
			defer cleanup()
			ctx := cmd.Context()

			servers, issues := mcpsec.Inventory(locate.Locations(r.home), nil)
			pins, err := r.loadPins(ctx)
			if err != nil {
				return fmt.Errorf("load pins: %w", err)
			}
			byStatus := map[string]int{}
			toolsSeen := 0
			for _, p := range pins {
				byStatus[p.Status]++
				if p.Hash.Tools != "" {
					toolsSeen++
				}
			}
			byClient := map[string]int{}
			for _, s := range servers {
				byClient[s.Client]++
			}
			fmt.Fprintf(out, "MCP security report (window %dd)\n\n", windowDays)
			fmt.Fprintf(out, "Inventory:   %d server(s) across %d client config(s)\n", len(servers), len(byClient))
			for _, c := range sortedKeysMCP(byClient) {
				fmt.Fprintf(out, "  %-12s %d server(s)\n", c, byClient[c])
			}
			fmt.Fprintf(out, "Pins:        %d total — %d approved, %d pinned (unapproved), %d drifted; tools observed for %d\n",
				len(pins), byStatus["approved"], byStatus["pinned"], byStatus["drifted"], toolsSeen)

			since := time.Now().UTC().Add(-time.Duration(windowDays) * 24 * time.Hour)
			events, err := st.LoadRecentGuardEvents(ctx, since, 10000)
			if err != nil {
				return fmt.Errorf("load events: %w", err)
			}
			byRule := map[string]int{}
			for _, e := range events {
				if e.Category == "mcp" {
					byRule[e.RuleID]++
				}
			}
			fmt.Fprintf(out, "Findings:    ")
			if len(byRule) == 0 {
				fmt.Fprintf(out, "none in window\n")
			} else {
				var parts []string
				for _, rule := range sortedKeysMCP(byRule) {
					parts = append(parts, fmt.Sprintf("%s ×%d", rule, byRule[rule]))
				}
				fmt.Fprintf(out, "%s\n", strings.Join(parts, ", "))
			}
			for _, issue := range issues {
				fmt.Fprintf(out, "ISSUE:       %s\n", issue)
			}
			fmt.Fprintf(out, "\nUnapproved or drifted servers mark mcp_unpinned taint (T-501/T-505);\napprove reviewed servers with `observer guard mcp approve <server>`.\n")
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to config.toml")
	cmd.Flags().IntVar(&windowDays, "window", 30, "report window in days")
	return cmd
}

func sortedKeysMCP[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func truncateMCP(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
