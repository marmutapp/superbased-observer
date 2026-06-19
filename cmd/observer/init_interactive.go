package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/marmutapp/superbased-observer/internal/hook"
	"github.com/marmutapp/superbased-observer/internal/mcp"
	"github.com/marmutapp/superbased-observer/internal/proxyroute"
)

// Interactive `observer init` (usability arc P6.10) — the CLI twin of
// the dashboard's setup wizard. Engaged only when init runs with ZERO
// flags on a real terminal (stdin AND stdout are char devices);
// scripts, CI, and any flagged invocation get the classic
// non-interactive behaviour untouched.
//
// Consent semantics mirror the wizard exactly: every write is
// previewed (the dry-run registry shows the precise file + entries)
// and asks its OWN yes/no — one consent per write, no apply-all.
// Hooks and proxy-route default to yes; the MCP step is never
// pre-selected and carries the per-turn token honesty note (the Q4
// rule). Plain stdin prompts; no TUI dependency.

// interactiveInitOptions parameterises runInteractiveInit so tests
// can sandbox the home directory and script stdin.
type interactiveInitOptions struct {
	BinaryPath string
	ConfigPath string
	ProxyPort  int
	// HomeDir overrides the registries' home resolution (tests). ""
	// = real home.
	HomeDir string
}

// stdinIsTerminal mirrors stdoutIsTerminal for the input side — the
// interactive checklist needs a human on both ends.
func stdinIsTerminal() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// initPrompter reads y/n answers from a line-oriented stdin. Empty
// input takes the default; anything unparseable re-asks; EOF aborts
// the whole flow (no further writes).
type initPrompter struct {
	in  *bufio.Reader
	out io.Writer
}

func (p *initPrompter) ask(question string, def bool) (bool, error) {
	suffix := "[Y/n]"
	if !def {
		suffix = "[y/N]"
	}
	for {
		fmt.Fprintf(p.out, "  %s %s ", question, suffix)
		line, err := p.in.ReadString('\n')
		answer := strings.ToLower(strings.TrimSpace(line))
		if err != nil && answer == "" {
			return false, fmt.Errorf("stdin closed — stopping; nothing further was written")
		}
		switch answer {
		case "":
			return def, nil
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		default:
			fmt.Fprintln(p.out, "  please answer y or n (enter for the default)")
			if err != nil {
				return false, fmt.Errorf("stdin closed — stopping; nothing further was written")
			}
		}
	}
}

// runInteractiveInit walks every detected tool and asks one consent
// per write, executing each write immediately after its yes.
//
//nolint:gocyclo // one prompt branch per consent step by design; the checklist reads top-to-bottom (P6.10 arc).
func runInteractiveInit(out io.Writer, in io.Reader, opts interactiveInitOptions) error {
	port := opts.ProxyPort
	if port == 0 {
		port = 8820
	}
	newHookReg := func(dryRun bool) (*hook.Registry, error) {
		return hook.NewRegistry(hook.Options{
			BinaryPath: opts.BinaryPath, DryRun: dryRun,
			ConfigPath: opts.ConfigPath, HomeDir: opts.HomeDir,
		})
	}
	newMCPReg := func(dryRun bool) (*mcp.Registrar, error) {
		return mcp.NewRegistrar(mcp.RegisterOptions{
			BinaryPath: opts.BinaryPath, DryRun: dryRun,
			ConfigPath: opts.ConfigPath, HomeDir: opts.HomeDir,
		})
	}
	newRouteReg := func(dryRun bool) (*proxyroute.Registrar, error) {
		return proxyroute.NewRegistrar(proxyroute.RegisterOptions{
			ProxyPort: port, DryRun: dryRun, HomeDir: opts.HomeDir,
		})
	}

	previewHooks, err := newHookReg(true)
	if err != nil {
		return err
	}
	writeHooks, err := newHookReg(false)
	if err != nil {
		return err
	}
	previewMCP, err := newMCPReg(true)
	if err != nil {
		return err
	}
	writeMCP, err := newMCPReg(false)
	if err != nil {
		return err
	}
	previewRoute, err := newRouteReg(true)
	if err != nil {
		return err
	}
	writeRoute, err := newRouteReg(false)
	if err != nil {
		return err
	}

	tools := selectTools(false, false, false, false, false, unionStrings(previewHooks.Installed(), previewMCP.Installed()))
	sort.Strings(tools)
	if len(tools) == 0 {
		fmt.Fprintln(out, "no AI tools detected — install one (or pass --claude-code / --cursor / --codex / --hermes explicitly) and re-run")
		return nil
	}

	fmt.Fprintln(out, "observer init — interactive setup")
	fmt.Fprintf(out, "detected: %s\n", strings.Join(tools, ", "))
	fmt.Fprintln(out, "each write below asks first; nothing is written without a yes.")
	p := &initPrompter{in: bufio.NewReader(in), out: out}

	codexHooksAdded := false
	claudeRouteDeclined := false
	for _, t := range tools {
		fmt.Fprintf(out, "\n%s\n", t)

		if hookSupported(t) {
			pre := previewHooks.Register(t)
			switch {
			case pre.Error != nil:
				fmt.Fprintf(out, "  hooks: ✗ %v\n", pre.Error)
			case len(pre.HooksAdded) == 0:
				fmt.Fprintf(out, "  hooks: already set in %s\n", pre.ConfigPath)
			default:
				fmt.Fprintln(out, "  hooks — session + tool-call capture into the local observer database.")
				fmt.Fprintf(out, "  would add %d hook(s) in %s\n", len(pre.HooksAdded), pre.ConfigPath)
				yes, err := p.ask(fmt.Sprintf("register hooks for %s?", t), true)
				if err != nil {
					return err
				}
				if yes {
					res := writeHooks.Register(t)
					printHookResult(out, t, res, false)
					if t == "codex" && res.Error == nil && len(res.HooksAdded) > 0 {
						codexHooksAdded = true
					}
				} else {
					fmt.Fprintln(out, "  skipped.")
				}
			}
		}

		if mcpSupported(t) {
			pre := previewMCP.Register(t)
			switch {
			case pre.Error != nil:
				fmt.Fprintf(out, "  mcp: ✗ %v\n", pre.Error)
			case pre.AlreadySet:
				fmt.Fprintf(out, "  mcp: already set in %s\n", pre.ConfigPath)
			default:
				fmt.Fprintln(out, "  MCP server — on-demand project-knowledge queries from inside the tool.")
				fmt.Fprintln(out, "  note: the tool schema costs ~1,800 tokens on every turn — skip unless you'll use it.")
				fmt.Fprintf(out, "  would register in %s\n", pre.ConfigPath)
				yes, err := p.ask(fmt.Sprintf("register the MCP server for %s?", t), false)
				if err != nil {
					return err
				}
				if yes {
					printMCPResult(out, t, writeMCP.Register(t), false)
				} else {
					fmt.Fprintln(out, "  skipped.")
				}
			}
		}

		if routeSupported(t) {
			previewFn, writeFn := previewRoute.RegisterCodex, writeRoute.RegisterCodex
			if t == "claude-code" {
				previewFn, writeFn = previewRoute.RegisterClaudeCode, writeRoute.RegisterClaudeCode
			}
			pre := previewFn()
			switch {
			case pre.Error != nil:
				fmt.Fprintf(out, "  route: ✗ %v\n", pre.Error)
				fmt.Fprintf(out, "  (an existing conflicting entry can be overwritten with `observer init --%s --force`)\n", t)
			case pre.AlreadySet:
				fmt.Fprintf(out, "  route: already set in %s → %s\n", pre.ConfigPath, pre.BaseURL)
			default:
				fmt.Fprintln(out, "  proxy route — exact token accounting + conversation compression via the local proxy.")
				fmt.Fprintf(out, "  would write %s → %s\n", pre.ConfigPath, pre.BaseURL)
				yes, err := p.ask(fmt.Sprintf("route %s through the observer proxy?", t), true)
				if err != nil {
					return err
				}
				if yes {
					printProxyRouteResult(out, t, writeFn(), false)
				} else {
					fmt.Fprintln(out, "  skipped.")
					if t == "claude-code" {
						claudeRouteDeclined = true
					}
				}
			}
		}
	}

	if codexHooksAdded {
		printCodexTrustHint(out)
	}
	if claudeRouteDeclined {
		printProxyRoutingHint(out, port)
	}
	home := opts.HomeDir
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	if home != "" {
		if fi, err := os.Stat(filepath.Join(home, ".hermes")); err == nil && fi.IsDir() {
			fmt.Fprintln(out, "\nhermes detected — it lives on a separate install path; run `observer init --hermes` to wire it.")
		}
	}
	return nil
}

// printCodexTrustHint reminds the user that codex hooks need one-time
// per-hook trust approval inside codex itself (its security boundary
// — observer can read trust state but never sets it).
func printCodexTrustHint(out io.Writer) {
	fmt.Fprintln(out)
	fmt.Fprintln(out, "next: codex requires per-hook trust approval (security feature).")
	fmt.Fprintln(out, "  open codex once and run /hooks to mark all 6 entries trusted.")
	fmt.Fprintln(out, "  one-time setup; trust persists in ~/.codex/config.toml [hooks.state].")
}
