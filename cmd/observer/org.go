package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/orgclient"
	"github.com/marmutapp/superbased-observer/internal/proxyroute"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// orgBundle is everything the org-* subcommands need from one config
// load + one DB open: the orgclient.Client (for enrolment / push), the
// underlying *store.Store (for direct cursor/state queries), and the
// resolved config so commands can render share-mode + scope. cleanup
// closes the DB; it is non-nil only when err is nil.
type orgBundle struct {
	client  *orgclient.Client
	store   *store.Store
	cfg     config.Config
	cleanup func()
}

// buildOrgClient assembles an orgclient.Client from config: the agent DB
// (store) plus the OS-keychain bearer store (0600-file fallback rooted next to
// the DB). It also reports whether the push loop is enabled in config (for
// status/enrol hints). The CLI uses a WARN-level logger so the client's own
// info logs do not compete with the command's printed output. The returned
// cleanup closes the DB; it is non-nil only when err is nil.
func buildOrgClient(ctx context.Context, configPath string) (c *orgclient.Client, cleanup func(), enabled bool, err error) {
	b, err := buildOrgBundle(ctx, configPath)
	if err != nil {
		return nil, nil, false, err
	}
	return b.client, b.cleanup, b.cfg.OrgClient.Enabled, nil
}

// buildOrgBundle is the richer variant new org subcommands use when they
// need the store handle (org status counts, org backfill, etc.) in
// addition to the client.
func buildOrgBundle(ctx context.Context, configPath string) (orgBundle, error) {
	cfg, err := config.Load(config.LoadOptions{GlobalPath: configPath})
	if err != nil {
		return orgBundle{}, fmt.Errorf("load config: %w", err)
	}
	if cfg.OrgClient.KeychainID == "" {
		cfg.OrgClient.KeychainID = config.DefaultKeychainID
	}
	if err := os.MkdirAll(filepath.Dir(cfg.Observer.DBPath), 0o755); err != nil {
		return orgBundle{}, fmt.Errorf("ensure db dir: %w", err)
	}
	database, err := db.Open(ctx, db.Options{Path: cfg.Observer.DBPath})
	if err != nil {
		return orgBundle{}, fmt.Errorf("open db %s: %w", cfg.Observer.DBPath, err)
	}
	logger := newLogger("warn")
	st := store.New(database)
	// Wire the org-tier observability provider seam (obs-org-tier plan §2):
	// obs owns the obs_* reads; this binds them as plain func fields so the
	// push path can compose the opt-in tiers without internal/store importing
	// internal/obs. A no-op (zero providers) when obs is disabled / no_obs.
	st.SetObsOrgProviders(obsOrgProviders(ctx, cfg, database, logger))
	bs := orgclient.OpenBearerStore(cfg.OrgClient.KeychainID, filepath.Dir(cfg.Observer.DBPath), logger)
	return orgBundle{
		client:  orgclient.New(cfg.OrgClient, st, bs, version, nil, logger),
		store:   st,
		cfg:     cfg,
		cleanup: func() { _ = database.Close() },
	}, nil
}

func newEnrollCmd() *cobra.Command {
	var (
		configPath  string
		linkURL     string
		writeBlock  bool
		wireClients bool
	)
	cmd := &cobra.Command{
		Use:   "enroll [<org-url> <token>]",
		Short: "Enrol this agent in an organisation's Observer server",
		Long: `Exchanges a one-time enrolment token for a long-lived bearer.

Three ways to supply credentials, in priority order:
  --link <url>             A magic link the admin shared
                           (form: ` + "`http(s)://<host>/enrol/<code>`" + `)
  <org-url> <token>        Positional pair (legacy form)
  ENROLMENT_TOKEN env      With ` + "`--link <url-without-/enrol/code>`" + ` for org URL only

After enrolling, the agent (a) writes a default [org_client] block to
your config.toml (enabled=true, share.full_content=false,
push_interval_seconds=900) so you don't have to hand-edit TOML, and
(b) auto-wires the observer proxy + hooks + MCP into every detected
AI client (claude-code, codex, cursor, cline). Pass --no-write-block
or --no-wire-clients to opt out of either side. Only activity AFTER
enrolment is ever shared (the push cursor seeds at the current
high-water id).`,
		Args: cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			orgURL, token, err := resolveEnrolCredentials(linkURL, args)
			if err != nil {
				return err
			}
			c, cleanup, enabled, err := buildOrgClient(cmd.Context(), configPath)
			if err != nil {
				return err
			}
			defer cleanup()
			enr, err := c.Enroll(cmd.Context(), orgURL, token)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Enrolled in %s (org_id %s) as %s.\n", enr.OrgName, enr.OrgID, enr.UserEmail)
			fmt.Fprintf(out, "Pushing to %s.\n", enr.OrgServerURL)
			if writeBlock {
				cfgPath, werr := resolveConfigPath(configPath)
				if werr != nil {
					fmt.Fprintf(out, "\nWarn: could not resolve config path to write [org_client] block: %v\n", werr)
				} else if added, werr := ensureOrgClientBlock(cfgPath); werr != nil {
					fmt.Fprintf(out, "\nWarn: could not write [org_client] block to %s: %v\n", cfgPath, werr)
				} else if added {
					fmt.Fprintf(out, "\nWrote default [org_client] block to %s — restart `observer start` to begin pushing.\n", cfgPath)
				} else if !enabled {
					fmt.Fprintln(out, "\nNote: [org_client] enabled = false — set it to true and restart `observer start` to begin pushing.")
				}
			} else if !enabled {
				fmt.Fprintln(out, "\nNote: [org_client] enabled = false — set it to true and restart `observer start` to begin pushing.")
			}
			if wireClients {
				fmt.Fprintln(out, "\nAuto-wiring AI clients (hooks + MCP + proxy routing). Pass --no-wire-clients to skip.")
				// Resolve the proxy port from config so we can both
				// drive the registrar and probe whether the daemon
				// is currently listening.
				cfg, cfgErr := config.Load(config.LoadOptions{GlobalPath: configPath})
				proxyPort := 8820
				if cfgErr == nil && cfg.Proxy.Port > 0 {
					proxyPort = cfg.Proxy.Port
				}
				lines, claudeHint, codexHint, codexHooksHint, werr := wireAIClients(WireAIClientsOptions{
					ConfigPath: configPath,
					ProxyPort:  proxyPort,
					All:        true,
				})
				switch {
				case werr != nil:
					fmt.Fprintf(out, "Warn: auto-wire failed: %v\n", werr)
				case len(lines) == 0:
					fmt.Fprintln(out, "  (no AI clients detected — install Claude Code / Codex / Cursor / Cline first, then run `observer init`)")
				default:
					for _, ln := range lines {
						if ln != "" {
							fmt.Fprintln(out, "  "+ln)
						}
					}
					if claudeHint != "" {
						fmt.Fprintln(out)
						fmt.Fprint(out, claudeHint)
					}
					if codexHint != "" {
						fmt.Fprintln(out)
						fmt.Fprint(out, codexHint)
					}
					if codexHooksHint {
						fmt.Fprintln(out)
						fmt.Fprintln(out, "next: codex requires per-hook trust approval (security feature).")
						fmt.Fprintln(out, "  open codex once and run /hooks to mark all 6 entries trusted.")
					}
				}
				// Loud post-wire warning when the proxy isn't
				// listening: every Claude Code session on the host
				// now routes through 127.0.0.1:<port> after the
				// settings.json write, so a stopped proxy breaks
				// EVERY Claude Code instance until the operator
				// launches `observer start`. N4 caveat in
				// docs/teams-test-regression-v1.8.2-2026-06-04.md.
				if !proxyReachable(fmt.Sprintf("http://127.0.0.1:%d", proxyPort), 200*time.Millisecond) {
					fmt.Fprintln(out)
					fmt.Fprintf(out, "WARNING: the observer proxy on 127.0.0.1:%d is not running.\n", proxyPort)
					fmt.Fprintln(out, "  Claude Code now routes through the proxy via ANTHROPIC_BASE_URL in")
					fmt.Fprintln(out, "  ~/.claude/settings.json, so every Claude Code session on this host")
					fmt.Fprintln(out, "  will fail with connection-refused until the proxy starts.")
					fmt.Fprintln(out, "  Run `observer start` in another shell (or `observer proxy start`).")
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to config.toml (defaults to ~/.observer/config.toml)")
	cmd.Flags().StringVar(&linkURL, "link", "", "Enrol magic link (http(s)://host/enrol/<code>)")
	cmd.Flags().BoolVar(&writeBlock, "write-block", true, "auto-write a default [org_client] block to config.toml (use --write-block=false to skip)")
	cmd.Flags().BoolVar(&wireClients, "wire-clients", true, "auto-register hooks + MCP + proxy routing for every detected AI client (use --wire-clients=false to skip)")
	return cmd
}

// resolveEnrolCredentials resolves (orgURL, token) from one of:
//   - --link http(s)://host/enrol/<code>  → (http(s)://host, <code>)
//   - positional [org-url, token]         → as supplied
//
// Returns a usage error when neither form provides both pieces.
func resolveEnrolCredentials(linkURL string, args []string) (string, string, error) {
	if linkURL != "" {
		u, err := url.Parse(linkURL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return "", "", fmt.Errorf("invalid --link URL %q (need http(s)://host/enrol/<code>)", linkURL)
		}
		// Accept /enrol/<code> path segment OR ?token=<code> query.
		var code string
		path := strings.TrimPrefix(u.Path, "/")
		switch {
		case strings.HasPrefix(path, "enrol/"):
			code = strings.TrimPrefix(path, "enrol/")
		case u.Query().Get("token") != "":
			code = u.Query().Get("token")
		}
		if code == "" {
			return "", "", fmt.Errorf("--link %q has no code (expected /enrol/<code> path or ?token=<code> query)", linkURL)
		}
		return u.Scheme + "://" + u.Host, code, nil
	}
	if len(args) == 2 {
		return args[0], args[1], nil
	}
	return "", "", errors.New("observer enroll: supply --link <url> OR positional <org-url> <token>")
}

// resolveConfigPath returns the effective config.toml path the operator's
// invocation will touch — explicit --config wins, otherwise
// $HOME/.observer/config.toml.
func resolveConfigPath(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("could not resolve $HOME: %w", err)
	}
	return filepath.Join(home, ".observer", "config.toml"), nil
}

// ensureOrgClientBlock appends a default [org_client] block to the
// config file at path when one is not already present. Returns (added,
// err): added=true means we wrote a block; false + nil error means the
// block already existed and the file was untouched. Idempotent: a
// second call after the first is a no-op.
//
// The append is text-only — we never re-encode the whole file — so a
// hand-curated config keeps its comments, formatting, and ordering.
func ensureOrgClientBlock(path string) (bool, error) {
	body, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	if strings.Contains(string(body), "[org_client]") {
		return false, nil
	}
	block := "\n[org_client]\n" +
		"# enabled = true means observer start ships activity rollups to the org server\n" +
		"# on every push_interval_seconds. Set to false to pause sharing without unenrolling.\n" +
		"enabled = true\n" +
		"push_interval_seconds = 900\n" +
		"max_push_bytes = 1048576  # 1 MiB\n" +
		"\n" +
		"[org_client.share]\n" +
		"# full_content = true ships raw command bodies (run_command), assistant prose\n" +
		"# (task_complete), and raw filesystem paths to the org server. Default false\n" +
		"# ships only hashes — the metadata-only posture. The org admin cannot flip\n" +
		"# this remotely; it lives solely in this file. See docs/teams-getting-started.md.\n" +
		"full_content = false\n" +
		"# target_action_allowlist = [\"read_file\", \"edit_file\", \"write_file\"]\n" +
		"\n" +
		"[org_client.scope]\n" +
		"# project_root_allowlist = [\"/home/me/work/acme\"]\n" +
		"# project_root_denylist = [\"/home/me/personal\"]\n"
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return false, err
	}
	defer f.Close()
	if _, err := f.WriteString(block); err != nil {
		return false, err
	}
	return true, nil
}

func newUnenrollCmd() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "unenroll",
		Short: "Leave the enrolled organisation and clear local credentials",
		Long: `Deletes the local enrolment and removes the bearer + signing key from the
OS keychain. A running daemon's push loop stops within one interval. This does
not delete anything already shared with the org server.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, cleanup, _, err := buildOrgClient(cmd.Context(), configPath)
			if err != nil {
				return err
			}
			defer cleanup()
			st, err := c.Status(cmd.Context())
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if !st.Enrolled {
				fmt.Fprintln(out, "Not enrolled; nothing to do.")
				return nil
			}
			if err := c.Unenroll(cmd.Context()); err != nil {
				return err
			}
			fmt.Fprintf(out, "Unenrolled from %s. Local credentials cleared.\n", st.OrgName)

			// Symmetric cleanup for the RegisterClaudeCode write
			// from enroll: drop ANTHROPIC_BASE_URL from
			// ~/.claude/settings.json when it points at a loopback
			// (our own observer); preserve a deliberate third-party
			// proxy entry. Without this, every Claude Code session
			// keeps routing through the now-stopped observer proxy
			// after unenroll. Best-effort: a failure here doesn't
			// fail unenroll. N4 caveat in
			// docs/teams-test-regression-v1.8.2-2026-06-04.md.
			cfg, cfgErr := config.Load(config.LoadOptions{GlobalPath: configPath})
			proxyPort := 8820
			if cfgErr == nil && cfg.Proxy.Port > 0 {
				proxyPort = cfg.Proxy.Port
			}
			if reg, regErr := proxyroute.NewRegistrar(proxyroute.RegisterOptions{ProxyPort: proxyPort}); regErr == nil {
				if r := reg.UnregisterClaudeCode(); r.Error == nil && r.Added {
					fmt.Fprintf(out, "Removed ANTHROPIC_BASE_URL from %s.\n", r.ConfigPath)
				}
			}
			// Symmetric cleanup for the org policy bundle (guard spec
			// §14.2): leaving the org removes its strictness floor —
			// the guard's org layer loads from this cache, so it must
			// not outlive the enrolment. Best-effort (the file may
			// never have existed); the append-only key-pin history in
			// guard_policy_state deliberately stays as audit. A
			// running daemon's engines drop the layer at next start;
			// hook processes drop it on their next invocation.
			if cfgErr == nil {
				if cachePath := orgBundleCachePath(cfg); cachePath != "" {
					if rmErr := os.Remove(cachePath); rmErr == nil {
						fmt.Fprintf(out, "Removed org policy bundle %s.\n", cachePath)
					} else if !os.IsNotExist(rmErr) {
						fmt.Fprintf(out, "Warning: org policy bundle not removed (%v) — delete %s manually to drop the org policy layer.\n", rmErr, cachePath)
					}
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to config.toml (defaults to ~/.observer/config.toml)")
	return cmd
}

func newOrgCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "org",
		Short: "Organisation (Teams) enrolment status, scope, manual push, preview, backfill",
	}
	cmd.AddCommand(
		newOrgStatusCmd(),
		newOrgPushStatusCmd(),
		newOrgPushNowCmd(),
		newOrgPreviewCmd(),
		newOrgBackfillCmd(),
		newOrgEmitManagedSettingsCmd(),
	)
	return cmd
}

func newOrgStatusCmd() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show enrolment status, share mode, scope counts, and the last push",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			b, err := buildOrgBundle(cmd.Context(), configPath)
			if err != nil {
				return err
			}
			defer b.cleanup()
			st, err := b.client.Status(cmd.Context())
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if !st.Enrolled {
				fmt.Fprintln(out, "Not enrolled.")
				fmt.Fprintf(out, "Credential store: %s\n", st.Backend)
				return nil
			}
			fmt.Fprintf(out, "Enrolled:         yes\n")
			fmt.Fprintf(out, "Organisation:     %s (org_id %s)\n", st.OrgName, st.OrgID)
			fmt.Fprintf(out, "User:             %s\n", st.UserEmail)
			fmt.Fprintf(out, "Server:           %s\n", st.OrgServerURL)
			fmt.Fprintf(out, "Enrolled at:      %s\n", st.EnrolledAt)
			fmt.Fprintf(out, "Credential store: %s\n", st.Backend)
			fmt.Fprintf(out, "Pushing enabled:  %t\n", b.cfg.OrgClient.Enabled)
			fmt.Fprintln(out)

			// Share mode — the v1.8.0 per-node opt-in. Default is
			// metadata-only (hashes ship; raw paths + content
			// withheld). full_content=true ships the raw values too.
			shareDesc := "metadata-only (hashes; raws withheld) — the default"
			if b.cfg.OrgClient.Share.FullContent {
				shareDesc = "FULL CONTENT (raw command bodies + assistant prose + raw paths SHIPPED)"
			}
			fmt.Fprintf(out, "Share mode:       %s\n", shareDesc)
			if len(b.cfg.OrgClient.Share.TargetActionAllowlist) > 0 {
				fmt.Fprintf(out, "Per-action raws: %v\n", b.cfg.OrgClient.Share.TargetActionAllowlist)
			}
			fmt.Fprintln(out)

			// Scope counts: historical (already in the DB) vs.
			// eligible-since-enrolment (above the cursor, which the
			// agent seeds to the current high-water id at enrolment so
			// only post-enrolment activity is ever shareable).
			cur, err := b.store.LoadPushCursor(cmd.Context())
			if err != nil {
				return fmt.Errorf("load cursor: %w", err)
			}
			maxIDs, err := b.store.CurrentMaxIDs(cmd.Context())
			if err != nil {
				return fmt.Errorf("current max ids: %w", err)
			}
			fmt.Fprintf(out, "%-12s  %12s  %12s\n", "Table", "historical", "eligible")
			fmt.Fprintf(out, "%-12s  %12s  %12s\n", "----", "----------", "--------")
			fmt.Fprintf(out, "%-12s  %12d  %12d\n", "sessions", maxIDs.Sessions, maxIDs.Sessions-cur.Sessions)
			fmt.Fprintf(out, "%-12s  %12d  %12d\n", "actions", maxIDs.Actions, maxIDs.Actions-cur.Actions)
			fmt.Fprintf(out, "%-12s  %12d  %12d\n", "api_turns", maxIDs.APITurns, maxIDs.APITurns-cur.APITurns)
			fmt.Fprintf(out, "%-12s  %12d  %12d\n", "token_usage", maxIDs.TokenUsage, maxIDs.TokenUsage-cur.TokenUsage)
			fmt.Fprintf(out, "%-12s  %12d  %12d\n", "guard_events", maxIDs.GuardEvents, maxIDs.GuardEvents-cur.GuardEvents)
			fmt.Fprintln(out)

			// Explain an eligible==0 state in plain language (Phase C #8)
			// so "historical N, eligible 0" isn't a silent dead end.
			if reason := pushEligibilityReason(eligibleTotal(cur, maxIDs), historicalTotal(maxIDs), st.LastPush); reason != "" {
				fmt.Fprintln(out, "Why eligible = 0:")
				fmt.Fprintf(out, "  %s\n", reason)
				fmt.Fprintln(out)
			}

			if st.LastPush == nil {
				fmt.Fprintln(out, "Last push:        (none yet)")
			} else {
				lp := st.LastPush
				fmt.Fprintf(out, "Last push:        %s — %s, %d rows, %d bytes%s\n",
					lp.PushedAt, lp.Status, lp.RowCount, lp.Bytes, errSuffix(lp.Error))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to config.toml (defaults to ~/.observer/config.toml)")
	return cmd
}

// newOrgBackfillCmd lets the operator deliberately walk the push cursor
// backwards so historical rows become eligible. By default
// `observer enroll` seeds the cursor at the current high-water id, so
// only post-enrolment activity ships — protecting against the Issue 4
// "first push exfiltrates the whole local corpus" finding. backfill is
// the explicit opt-in to share older data.
func newOrgBackfillCmd() *cobra.Command {
	var (
		configPath string
		all        bool
		confirm    bool
	)
	cmd := &cobra.Command{
		Use:   "backfill",
		Short: "Rewind the push cursor so historical rows become eligible (opt-in)",
		Long: `Rewinds the push cursor so previously-skipped historical rows become
eligible for the next push loop. By default ` + "`observer enroll`" + ` seeds
the cursor at the current high-water id, so only post-enrolment activity is
ever shared. ` + "`backfill --all --confirm`" + ` resets every table cursor to
0 so the entire local corpus ships on the next push cycle.

Use ` + "`observer org status`" + ` first to see how many historical rows
would become eligible. Dry-run is the default; --confirm is required to
actually rewind.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !all {
				return errors.New("observer org backfill: --all is currently the only supported scope (pass --all --confirm to rewind every table cursor to 0)")
			}
			b, err := buildOrgBundle(cmd.Context(), configPath)
			if err != nil {
				return err
			}
			defer b.cleanup()

			cur, err := b.store.LoadPushCursor(cmd.Context())
			if err != nil {
				return fmt.Errorf("load cursor: %w", err)
			}
			maxIDs, err := b.store.CurrentMaxIDs(cmd.Context())
			if err != nil {
				return fmt.Errorf("current max ids: %w", err)
			}
			eligible := (maxIDs.Sessions - cur.Sessions) +
				(maxIDs.Actions - cur.Actions) +
				(maxIDs.APITurns - cur.APITurns) +
				(maxIDs.TokenUsage - cur.TokenUsage) +
				(maxIDs.GuardEvents - cur.GuardEvents)
			toUnlock := maxIDs.Sessions + maxIDs.Actions + maxIDs.APITurns + maxIDs.TokenUsage + maxIDs.GuardEvents

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "currently eligible: %d rows\n", eligible)
			fmt.Fprintf(out, "would unlock:       %d more rows (entire local corpus)\n", toUnlock-eligible)

			if !confirm {
				fmt.Fprintln(out, "\nDRY RUN — pass --confirm to rewind the cursor.")
				return nil
			}
			if err := b.store.SavePushCursor(cmd.Context(), store.PushCursor{}); err != nil {
				return fmt.Errorf("rewind cursor: %w", err)
			}
			fmt.Fprintln(out, "cursor reset to 0; next push cycle will ship the full corpus.")
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to config.toml (defaults to ~/.observer/config.toml)")
	cmd.Flags().BoolVar(&all, "all", false, "rewind every table's cursor to 0 (currently the only supported scope)")
	cmd.Flags().BoolVar(&confirm, "confirm", false, "actually rewind (omit for dry-run)")
	return cmd
}

// newOrgPreviewCmd surfaces the last-pushed wire bytes so the operator
// can see exactly what their enrolment is sending. Reads
// schema_meta.org_last_push_payload (written by orgclient on every
// successful push). The dashboard exposes the same data at
// /api/enrolment/last-payload; this CLI lets a node operator inspect
// it without a browser.
func newOrgPreviewCmd() *cobra.Command {
	var (
		configPath string
		raw        bool
	)
	cmd := &cobra.Command{
		Use:   "preview",
		Short: "Show the last successfully pushed wire payload (what you actually shared)",
		Long: `Prints the JSON of the most recent successfully pushed envelope — exactly
the rollup shape that left the machine, byte for byte (gzip is stripped
before storage). The dashboard surfaces this data at
/api/enrolment/last-payload; ` + "`observer org preview`" + ` provides the
same view without a browser.

The default output is pretty-printed JSON. --raw prints the stored bytes
unchanged for piping to jq.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			b, err := buildOrgBundle(cmd.Context(), configPath)
			if err != nil {
				return err
			}
			defer b.cleanup()
			body, err := b.store.LoadLastPushPayload(cmd.Context())
			if err != nil {
				return fmt.Errorf("load last payload: %w", err)
			}
			if len(body) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "(no push has succeeded yet)")
				return nil
			}
			out := cmd.OutOrStdout()
			if raw {
				_, _ = out.Write(body)
				if len(body) > 0 && body[len(body)-1] != '\n' {
					fmt.Fprintln(out)
				}
				return nil
			}
			// Pretty-print: parse + re-marshal indented. Fall back to
			// raw if the stored bytes aren't valid JSON (e.g. a future
			// shape change). Always print a share-mode banner first so
			// the operator can interpret what they're seeing.
			shareLine := "metadata-only (hashes only; raw paths + content withheld)"
			if b.cfg.OrgClient.Share.FullContent {
				shareLine = "FULL CONTENT (raw paths + command bodies + assistant prose SHIPPED)"
			}
			fmt.Fprintf(out, "# Share mode at last push: %s\n", shareLine)
			var v any
			if err := json.Unmarshal(body, &v); err != nil {
				_, _ = out.Write(body)
				return nil
			}
			pretty, err := json.MarshalIndent(v, "", "  ")
			if err != nil {
				_, _ = out.Write(body)
				return nil
			}
			fmt.Fprintln(out, string(pretty))
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to config.toml (defaults to ~/.observer/config.toml)")
	cmd.Flags().BoolVar(&raw, "raw", false, "print the stored JSON bytes unchanged (for piping to jq)")
	return cmd
}

func newOrgPushNowCmd() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "push-now",
		Short: "Push any unpushed activity to the org server immediately",
		Long: `Runs one push cycle now instead of waiting for the interval. Useful to
verify enrolment end to end or to flush before shutting down.

When nothing is queued the command reports the current cursor state
and the last push so the operator can tell why nothing happened —
"cursor up to date" vs "the auto-loop just pushed in the previous
interval".`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			b, err := buildOrgBundle(cmd.Context(), configPath)
			if err != nil {
				return err
			}
			defer b.cleanup()
			res, err := b.client.PushOnce(cmd.Context())
			if errors.Is(err, orgclient.ErrNotEnrolled) {
				return errors.New("not enrolled; run `observer enroll <org-url> <token>` first")
			}
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if res.Empty {
				// M5.3: don't say "nothing to push" without the *why*.
				// Print the cursor + max IDs + last push so the operator
				// can tell whether the auto-loop just drained them or
				// whether the cursor is genuinely up to date with no
				// new activity.
				cur, cerr := b.store.LoadPushCursor(cmd.Context())
				maxIDs, mErr := b.store.CurrentMaxIDs(cmd.Context())
				lp, lpErr := b.store.LastPushLog(cmd.Context())
				fmt.Fprintln(out, "Nothing to push — the cursor is up to date.")
				if cerr == nil && mErr == nil {
					fmt.Fprintf(out, "  cursor:   sessions=%d actions=%d api_turns=%d token_usage=%d guard_events=%d\n",
						cur.Sessions, cur.Actions, cur.APITurns, cur.TokenUsage, cur.GuardEvents)
					fmt.Fprintf(out, "  max ids:  sessions=%d actions=%d api_turns=%d token_usage=%d guard_events=%d\n",
						maxIDs.Sessions, maxIDs.Actions, maxIDs.APITurns, maxIDs.TokenUsage, maxIDs.GuardEvents)
				}
				if lpErr == nil && lp != nil {
					fmt.Fprintf(out, "  last push: %s — %s, %d rows, %d bytes%s\n",
						lp.PushedAt, lp.Status, lp.RowCount, lp.Bytes, errSuffix(lp.Error))
				}
				return nil
			}
			fmt.Fprintf(out, "Pushed %d rows (%d bytes): accepted=%d deduped=%d\n",
				res.RowCount, res.Bytes, res.AcceptedRows, res.DedupedRows)
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to config.toml (defaults to ~/.observer/config.toml)")
	return cmd
}

func errSuffix(s string) string {
	if s == "" {
		return ""
	}
	return " (" + s + ")"
}
