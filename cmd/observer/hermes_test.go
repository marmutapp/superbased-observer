package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestHermesRouteFor pins the argv the launcher hands hermes for every row of
// hermesRouteRules. The regression it guards: injecting `--provider observer`
// with NO model, which hermes rejects outright on its -z one-shot path
// ("--provider requires --model (or HERMES_INFERENCE_MODEL)") — every wrapped
// API call failed while the same command worked natively.
func TestHermesRouteFor(t *testing.T) {
	const dflt = "nvidia/nemotron-3-ultra-550b-a55b:free"

	tests := []struct {
		name     string
		in       hermesRouteInputs
		wantArgs []string
		wantEns  bool
		wantRtd  bool
		notice   string // substring; "" = expect silence
	}{
		{
			name:     "no model, config default injected",
			in:       hermesRouteInputs{args: []string{"-z", "say OK"}, configModel: dflt},
			wantArgs: []string{"--provider", "observer", "--model", dflt, "-z", "say OK"},
			wantEns:  true,
			wantRtd:  true,
		},
		{
			name:     "user --model respected, not duplicated",
			in:       hermesRouteInputs{args: []string{"-z", "hi", "--model", "x/y"}, configModel: dflt},
			wantArgs: []string{"--provider", "observer", "-z", "hi", "--model", "x/y"},
			wantEns:  true,
			wantRtd:  true,
		},
		{
			name:     "user --model=x joined form respected",
			in:       hermesRouteInputs{args: []string{"--model=x/y", "-z", "hi"}, configModel: dflt},
			wantArgs: []string{"--provider", "observer", "--model=x/y", "-z", "hi"},
			wantEns:  true,
			wantRtd:  true,
		},
		{
			name:     "user -m short form respected",
			in:       hermesRouteInputs{args: []string{"-m", "x/y"}, configModel: dflt},
			wantArgs: []string{"--provider", "observer", "-m", "x/y"},
			wantEns:  true,
			wantRtd:  true,
		},
		{
			name:     "HERMES_INFERENCE_MODEL counts as supplied",
			in:       hermesRouteInputs{args: []string{"-z", "hi"}, envModel: "env/model", configModel: dflt},
			wantArgs: []string{"--provider", "observer", "-z", "hi"},
			wantEns:  true,
			wantRtd:  true,
		},
		{
			name:     "no model anywhere: unrouted, loud",
			in:       hermesRouteInputs{args: []string{"-z", "hi"}},
			wantArgs: []string{"-z", "hi"},
			wantEns:  false,
			wantRtd:  false,
			notice:   "NOT proxy-routed",
		},
		{
			name:     "operator's own foreign --provider left alone",
			in:       hermesRouteInputs{args: []string{"--provider", "openrouter", "-z", "hi"}, configModel: dflt},
			wantArgs: []string{"--provider", "openrouter", "-z", "hi"},
			wantEns:  false,
			wantRtd:  false,
			notice:   "--provider openrouter supplied",
		},
		{
			name:     "operator selected the observer provider: model injected, no duplicate --provider",
			in:       hermesRouteInputs{args: []string{"--provider", "observer", "-z", "hi"}, configModel: dflt},
			wantArgs: []string{"--model", dflt, "--provider", "observer", "-z", "hi"},
			wantEns:  true,
			wantRtd:  true,
		},
		{
			name:     "a --model after a bare -- is prompt text, not a flag",
			in:       hermesRouteInputs{args: []string{"-z", "--", "--model", "literal"}, configModel: dflt},
			wantArgs: []string{"--provider", "observer", "--model", dflt, "-z", "--", "--model", "literal"},
			wantEns:  true,
			wantRtd:  true,
		},
		{
			name:     "config default is trimmed",
			in:       hermesRouteInputs{args: nil, configModel: "  " + dflt + "\n"},
			wantArgs: []string{"--provider", "observer", "--model", dflt},
			wantEns:  true,
			wantRtd:  true,
		},

		// F2 — the `chat` subcommand re-declares -m/--model and --provider with
		// plain None defaults, so top-level values placed BEFORE `chat` are
		// silently discarded (proven against hermes' real parser). Inject AFTER
		// the subcommand instead, where chat's own parser honours them.
		{
			name:     "chat: flags injected AFTER the subcommand",
			in:       hermesRouteInputs{args: []string{"chat", "-q", "hi"}, configModel: dflt},
			wantArgs: []string{"chat", "--provider", "observer", "--model", dflt, "-q", "hi"},
			wantEns:  true,
			wantRtd:  true,
		},
		{
			name:     "chat: a user model AFTER chat is respected",
			in:       hermesRouteInputs{args: []string{"chat", "-m", "x/y", "-q", "hi"}, configModel: dflt},
			wantArgs: []string{"chat", "--provider", "observer", "-m", "x/y", "-q", "hi"},
			wantEns:  true,
			wantRtd:  true,
		},
		{
			name: "chat: a model BEFORE chat is discarded by argparse, so a real one is injected",
			in: hermesRouteInputs{
				args: []string{"--model", "x/y", "chat", "-q", "hi"}, configModel: dflt,
			},
			wantArgs: []string{"--model", "x/y", "chat", "--provider", "observer", "--model", dflt, "-q", "hi"},
			wantEns:  true,
			wantRtd:  true,
		},
		{
			name:     "chat: a foreign provider AFTER chat still de-routes",
			in:       hermesRouteInputs{args: []string{"chat", "--provider", "openrouter"}, configModel: dflt},
			wantArgs: []string{"chat", "--provider", "openrouter"},
			wantEns:  false,
			wantRtd:  false,
			notice:   "--provider openrouter supplied",
		},
		{
			name:     "chat: an observer provider BEFORE chat would be discarded, so it is re-injected",
			in:       hermesRouteInputs{args: []string{"--provider", "observer", "chat", "-q", "hi"}, configModel: dflt},
			wantArgs: []string{"--provider", "observer", "chat", "--provider", "observer", "--model", dflt, "-q", "hi"},
			wantEns:  true,
			wantRtd:  true,
		},
		{
			name:     "a `chat` that is a -z PROMPT is not a subcommand",
			in:       hermesRouteInputs{args: []string{"-z", "chat"}, configModel: dflt},
			wantArgs: []string{"--provider", "observer", "--model", dflt, "-z", "chat"},
			wantEns:  true,
			wantRtd:  true,
		},

		// F4 — a blank model value is NOT "a model supplied": hermes' guard
		// tests `(model or "").strip()`, so `--provider` + `--model ""` is the
		// exact `exit 2` this table exists to prevent. It also cannot be fixed
		// by injecting a real model, because argparse is last-wins and the
		// blank comes after the prepend (verified against hermes' own parser:
		// `--provider observer --model D --model "" -z hi` → model=''). So the
		// row fails OPEN: untouched argv, no config write, loud notice.
		{
			name:     "blank --model value: unrouted fail-open, never provider-without-model",
			in:       hermesRouteInputs{args: []string{"--model", "", "-z", "hi"}, configModel: dflt},
			wantArgs: []string{"--model", "", "-z", "hi"},
			wantEns:  false,
			wantRtd:  false,
			notice:   "EMPTY `--model`",
		},
		{
			name:     "--model= joined empty value is the same row",
			in:       hermesRouteInputs{args: []string{"--model=", "-z", "hi"}, configModel: dflt},
			wantArgs: []string{"--model=", "-z", "hi"},
			wantEns:  false,
			wantRtd:  false,
			notice:   "EMPTY `--model`",
		},
		{
			name:     "whitespace-only -m value is the same row",
			in:       hermesRouteInputs{args: []string{"-m", "   "}, configModel: dflt},
			wantArgs: []string{"-m", "   "},
			wantEns:  false,
			wantRtd:  false,
			notice:   "EMPTY `--model`",
		},
		{
			name:     "trailing --model with no value at all is the same row",
			in:       hermesRouteInputs{args: []string{"-z", "hi", "--model"}, configModel: dflt},
			wantArgs: []string{"-z", "hi", "--model"},
			wantEns:  false,
			wantRtd:  false,
			notice:   "EMPTY `--model`",
		},
		{
			name: "blank --model but HERMES_INFERENCE_MODEL set: hermes' guard passes, so it ROUTES",
			in: hermesRouteInputs{
				args: []string{"--model", "", "-z", "hi"}, envModel: "env/model", configModel: dflt,
			},
			wantArgs: []string{"--provider", "observer", "--model", "", "-z", "hi"},
			wantEns:  true,
			wantRtd:  true,
		},
		{
			name:     "blank model with NO config default stays unrouted, not exit-2 bait",
			in:       hermesRouteInputs{args: []string{"--model", "", "-z", "hi"}},
			wantArgs: []string{"--model", "", "-z", "hi"},
			wantEns:  false,
			wantRtd:  false,
			notice:   "NOT proxy-routed",
		},

		// F5 — argparse abbreviations. `--prov openrouter` resolves to
		// provider=openrouter, so it MUST trip the provider-supplied guard;
		// injecting anyway would hand the run to openrouter (last-wins) while
		// claiming it was routed.
		{
			name:     "abbreviated foreign --prov de-routes",
			in:       hermesRouteInputs{args: []string{"--prov", "openrouter", "-z", "hi"}, configModel: dflt},
			wantArgs: []string{"--prov", "openrouter", "-z", "hi"},
			wantEns:  false,
			wantRtd:  false,
			notice:   "--provider openrouter supplied",
		},
		{
			name:     "abbreviated --provid=openrouter de-routes",
			in:       hermesRouteInputs{args: []string{"--provid=openrouter"}, configModel: dflt},
			wantArgs: []string{"--provid=openrouter"},
			wantEns:  false,
			wantRtd:  false,
			notice:   "--provider openrouter supplied",
		},
		{
			name:     "abbreviated --prov observer is OUR provider: no duplicate flag",
			in:       hermesRouteInputs{args: []string{"--prov", "observer", "-z", "hi"}, configModel: dflt},
			wantArgs: []string{"--model", dflt, "--prov", "observer", "-z", "hi"},
			wantEns:  true,
			wantRtd:  true,
		},
		{
			name:     "abbreviated --mod counts as a supplied model",
			in:       hermesRouteInputs{args: []string{"--mod", "x/y", "-z", "hi"}, configModel: dflt},
			wantArgs: []string{"--provider", "observer", "--mod", "x/y", "-z", "hi"},
			wantEns:  true,
			wantRtd:  true,
		},

		// F6 — a bare/blank `--provider` names nothing; say so instead of
		// printing an empty provider name with a double space.
		{
			name:     "bare trailing --provider is reported as valueless, not as a foreign provider",
			in:       hermesRouteInputs{args: []string{"-z", "hi", "--provider"}, configModel: dflt},
			wantArgs: []string{"-z", "hi", "--provider"},
			wantEns:  false,
			wantRtd:  false,
			notice:   "`--provider` supplied with no value",
		},
		{
			name:     "empty --provider= value is reported the same way",
			in:       hermesRouteInputs{args: []string{"--provider=", "-z", "hi"}, configModel: dflt},
			wantArgs: []string{"--provider=", "-z", "hi"},
			wantEns:  false,
			wantRtd:  false,
			notice:   "`--provider` supplied with no value",
		},
		{
			// argparse will not take `-z` as the provider's value, so this is
			// the same valueless typo, not a provider named "-z".
			name:     "--provider followed by another flag is valueless, not a provider named -z",
			in:       hermesRouteInputs{args: []string{"--provider", "-z", "hi"}, configModel: dflt},
			wantArgs: []string{"--provider", "-z", "hi"},
			wantEns:  false,
			wantRtd:  false,
			notice:   "`--provider` supplied with no value",
		},
		{
			// Same shape on the model side: a valueless --model lands on the
			// blank-model fail-open row instead of injecting a provider that
			// hermes would then refuse.
			name:     "--model followed by another flag is valueless",
			in:       hermesRouteInputs{args: []string{"--model", "-z", "hi"}, configModel: dflt},
			wantArgs: []string{"--model", "-z", "hi"},
			wantEns:  false,
			wantRtd:  false,
			notice:   "EMPTY `--model`",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hermesRouteFor(tt.in)
			if !reflect.DeepEqual(got.args, tt.wantArgs) {
				t.Errorf("args = %q, want %q", got.args, tt.wantArgs)
			}
			if got.ensureProvider != tt.wantEns {
				t.Errorf("ensureProvider = %v, want %v", got.ensureProvider, tt.wantEns)
			}
			if got.routed != tt.wantRtd {
				t.Errorf("routed = %v, want %v", got.routed, tt.wantRtd)
			}
			switch {
			case tt.notice == "" && got.notice != "":
				t.Errorf("notice = %q, want silence", got.notice)
			case tt.notice != "" && !strings.Contains(got.notice, tt.notice):
				t.Errorf("notice = %q, want substring %q", got.notice, tt.notice)
			}
		})
	}
}

// TestHermesRouteForDoesNotMutateInput pins that the prepend never aliases and
// writes through the operator's own args slice.
func TestHermesRouteForDoesNotMutateInput(t *testing.T) {
	args := []string{"-z", "hi"}
	orig := append([]string(nil), args...)
	_ = hermesRouteFor(hermesRouteInputs{args: args, configModel: "a/b"})
	if !reflect.DeepEqual(args, orig) {
		t.Fatalf("input args mutated: %q, want %q", args, orig)
	}
}

// TestHermesConfiguredModel exercises the model.default resolution against
// FIXTURE configs — never the operator's real ~/.hermes/config.yaml.
func TestHermesConfiguredModel(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		write   bool // false = file absent
		want    string
		wantErr bool
	}{
		{
			name:  "model.default",
			write: true,
			yaml:  "model:\n    api_mode: chat_completions\n    default: nvidia/nemotron:free\n    provider: openrouter\n",
			want:  "nvidia/nemotron:free",
		},
		{
			name:  "model.model fallback",
			write: true,
			yaml:  "model:\n    model: anthropic/claude-sonnet-4\n",
			want:  "anthropic/claude-sonnet-4",
		},
		{
			name:  "scalar model",
			write: true,
			yaml:  "model: openai/gpt-4o\n",
			want:  "openai/gpt-4o",
		},
		{
			name:  "no model block",
			write: true,
			yaml:  "providers:\n    observer:\n        base_url: http://127.0.0.1:8820\n",
			want:  "",
		},
		{
			name:  "empty default falls through to nothing",
			write: true,
			yaml:  "model:\n    default: \"\"\n",
			want:  "",
		},
		{
			name:    "missing file",
			write:   false,
			wantErr: true,
		},
		{
			name:    "malformed yaml",
			write:   true,
			yaml:    "model:\n  - default: x\n\tbad: [",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if tt.write {
				if err := os.WriteFile(path, []byte(tt.yaml), 0o600); err != nil {
					t.Fatalf("write fixture: %v", err)
				}
			}
			got, err := hermesConfiguredModel(path)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("hermesConfiguredModel(%s) = %q, want error", tt.name, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("hermesConfiguredModel: %v", err)
			}
			if got != tt.want {
				t.Errorf("model = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestHermesFlagValue pins the flag scanner's spellings, its ABBREVIATION
// handling (argparse accepts any unambiguous prefix — `--prov openrouter`
// really does set provider=openrouter, which used to escape the
// provider-supplied guard entirely), its last-wins semantics, and its
// bare-`--` boundary (tokens after `--` are prompt text, not flags).
func TestHermesFlagValue(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		long     string
		shorts   []string
		wantVal  string
		wantPres bool
		wantHas  bool
	}{
		{name: "space form", args: []string{"--provider", "x"}, long: "--provider", wantVal: "x", wantPres: true, wantHas: true},
		{name: "equals form", args: []string{"--provider=x"}, long: "--provider", wantVal: "x", wantPres: true, wantHas: true},
		{name: "trailing flag, no value", args: []string{"--provider"}, long: "--provider", wantPres: true},
		{name: "equals form with empty value", args: []string{"--provider="}, long: "--provider", wantPres: true, wantHas: true},
		{name: "absent", args: []string{"-z", "hi"}, long: "--provider"},
		{name: "after bare --", args: []string{"--", "--provider", "x"}, long: "--provider"},
		{name: "short alias", args: []string{"-m", "x"}, long: "--model", shorts: []string{"-m"}, wantVal: "x", wantPres: true, wantHas: true},
		{name: "last occurrence wins", args: []string{"--provider", "a", "--provider", "b"}, long: "--provider", wantVal: "b", wantPres: true, wantHas: true},

		// F5 — argparse abbreviations.
		{name: "abbrev --prov", args: []string{"--prov", "openrouter"}, long: "--provider", wantVal: "openrouter", wantPres: true, wantHas: true},
		{name: "abbrev --pr", args: []string{"--pr", "openrouter"}, long: "--provider", wantVal: "openrouter", wantPres: true, wantHas: true},
		{name: "abbrev --provid=x", args: []string{"--provid=x"}, long: "--provider", wantVal: "x", wantPres: true, wantHas: true},
		{name: "abbrev --mod", args: []string{"--mod", "x/y"}, long: "--model", shorts: []string{"-m"}, wantVal: "x/y", wantPres: true, wantHas: true},
		// Ambiguous prefixes match NOTHING — argparse refuses them too
		// (--p → --pass-session-id/--provider, --m → --max-turns/--model).
		{name: "ambiguous --p", args: []string{"--p", "openrouter"}, long: "--provider"},
		{name: "ambiguous --m", args: []string{"--m", "x/y"}, long: "--model", shorts: []string{"-m"}},
		// Not a prefix at all (`--prof` diverges from `provider` at the 4th
		// char) — hermes' own pre-parser only strips the exact `--profile`.
		{name: "non-prefix --prof", args: []string{"--prof", "coder"}, long: "--provider"},
		// argparse never swallows an option-looking token as a value, so a
		// flag followed by another flag is VALUELESS — not a provider named
		// "-z". (This is the shell form of the bare-`--provider` typo.)
		{name: "next token is a flag: valueless", args: []string{"--provider", "-z", "hi"}, long: "--provider", wantPres: true},
		{name: "next token is a flag: valueless model", args: []string{"--model", "-z", "hi"}, long: "--model", shorts: []string{"-m"}, wantPres: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hermesFlagValue(tt.args, tt.long, tt.shorts...)
			if got.present != tt.wantPres || got.value != tt.wantVal || got.hasValue != tt.wantHas {
				t.Errorf("hermesFlagValue = %+v, want {present:%v value:%q hasValue:%v}",
					got, tt.wantPres, tt.wantVal, tt.wantHas)
			}
		})
	}
}

// TestHermesChatIndex pins the `chat` subcommand scanner: chat's own parser
// re-declares --model/--provider, so the launcher must know where `chat` is to
// inject after it. A `chat` that is a PROMPT (a -z value, or past a bare --)
// is not a subcommand.
func TestHermesChatIndex(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"bare chat", []string{"chat", "-q", "hi"}, 0},
		{"chat after a store-true flag", []string{"--tui", "chat"}, 1},
		{"chat after a value flag pair", []string{"--model", "x/y", "chat", "-q", "hi"}, 2},
		{"chat after a profile pair", []string{"-p", "coder", "chat"}, 2},
		{"chat as a -z prompt is not a subcommand", []string{"-z", "chat"}, -1},
		{"chat after a bare -- is positional", []string{"--", "chat"}, -1},
		{"another subcommand", []string{"setup"}, -1},
		{"no subcommand", []string{"-z", "hi"}, -1},
		{"empty", nil, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hermesChatIndex(tc.args); got != tc.want {
				t.Errorf("hermesChatIndex(%q) = %d, want %d", tc.args, got, tc.want)
			}
		})
	}
}

// TestHermesArgsAreHeadless pins the attach-incompatible gate. `--oneshot` is
// the long alias of `-z` and used to slip through, so a scripted
// `observer hermes -- --oneshot "…"` was handed to a daemon-owned PTY.
func TestHermesArgsAreHeadless(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"-z", []string{"-z", "hi"}, true},
		{"--oneshot", []string{"--oneshot", "hi"}, true},
		{"--oneshot= joined", []string{"--oneshot=hi"}, true},
		{"chat -q", []string{"chat", "-q", "hi"}, true},
		{"chat --query", []string{"chat", "--query", "hi"}, true},
		{"interactive chat", []string{"chat"}, false},
		{"tui", []string{"--tui"}, false},
		{"bare", nil, false},
		{"-z after a bare -- is positional", []string{"--", "-z"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hermesArgsAreHeadless(tc.args); got != tc.want {
				t.Errorf("hermesArgsAreHeadless(%q) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

// TestHermesProfileArg pins the `--profile`/`-p` pre-parse, mirroring
// hermes_cli/main.py::_apply_profile_override — the scan that decides WHICH
// config.yaml the run uses.
func TestHermesProfileArg(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"long form", []string{"--profile", "coder", "-z", "hi"}, "coder"},
		{"short form", []string{"-p", "coder"}, "coder"},
		{"joined form", []string{"--profile=coder"}, "coder"},
		{"after a value flag", []string{"--model", "x/y", "-p", "coder"}, "coder"},
		{"after a subcommand", []string{"chat", "-p", "coder", "-q", "hi"}, "coder"},
		{"a -p that is a value of -z", []string{"-z", "-p"}, ""},
		{"invalid id rejected", []string{"-p", "no:xdist"}, ""},
		{"past a bare --", []string{"--", "-p", "coder"}, ""},
		{"mcp add --args passthrough", []string{"mcp", "add", "x", "--args", "-p", "coder"}, ""},
		{"trailing -p with no value", []string{"-p"}, ""},
		{"absent", []string{"-z", "hi"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hermesProfileArg(tc.args); got != tc.want {
				t.Errorf("hermesProfileArg(%q) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

// TestHermesResolveHome pins the home resolution against FIXTURE homes
// (t.TempDir()) — never the operator's real ~/.hermes. The regression it
// guards: the launcher READ the model from one config.yaml and WROTE
// providers.observer into another, so the run went direct-and-uncaptured under
// a "routing via …" banner with the wrong home's model substituted.
func TestHermesResolveHome(t *testing.T) {
	root := t.TempDir()   // stands in for the user's home dir
	custom := t.TempDir() // stands in for a HERMES_HOME outside ~/.hermes

	nativeHome := filepath.Join(root, ".hermes")
	if err := os.MkdirAll(nativeHome, 0o700); err != nil {
		t.Fatalf("mkdir native home: %v", err)
	}

	cases := []struct {
		name    string
		in      hermesHomeInputs
		want    string
		sticky  string // write this into <root>/active_profile first
		wantErr bool
	}{
		{
			name: "platform default",
			in:   hermesHomeInputs{userHome: root},
			want: nativeHome,
		},
		{
			name: "HERMES_HOME honoured",
			in:   hermesHomeInputs{userHome: root, env: custom},
			want: custom,
		},
		{
			name: "HERMES_HOME already a profile dir is trusted as-is",
			in:   hermesHomeInputs{userHome: root, env: filepath.Join(custom, "profiles", "coder")},
			want: filepath.Join(custom, "profiles", "coder"),
		},
		{
			name: "--profile resolves under the native root",
			in:   hermesHomeInputs{userHome: root, args: []string{"--profile", "coder", "-z", "hi"}},
			want: filepath.Join(nativeHome, "profiles", "coder"),
		},
		{
			name: "-p resolves under the native root",
			in:   hermesHomeInputs{userHome: root, args: []string{"-p", "coder"}},
			want: filepath.Join(nativeHome, "profiles", "coder"),
		},
		{
			name: "--profile is normalized to lowercase",
			in:   hermesHomeInputs{userHome: root, args: []string{"--profile=Coder"}},
			want: filepath.Join(nativeHome, "profiles", "coder"),
		},
		{
			name: "--profile default means the root itself",
			in:   hermesHomeInputs{userHome: root, args: []string{"-p", "default"}},
			want: nativeHome,
		},
		{
			name: "--profile WINS over HERMES_HOME",
			in:   hermesHomeInputs{userHome: root, env: custom, args: []string{"-p", "coder"}},
			want: filepath.Join(custom, "profiles", "coder"),
		},
		{
			name: "HERMES_HOME under the native home keeps the native root for profiles",
			in: hermesHomeInputs{
				userHome: root, env: filepath.Join(nativeHome, "sub"),
				args: []string{"-p", "coder"},
			},
			want: filepath.Join(nativeHome, "profiles", "coder"),
		},
		{
			name:   "sticky active_profile followed when no flag",
			in:     hermesHomeInputs{userHome: root},
			sticky: "coder\n",
			want:   filepath.Join(nativeHome, "profiles", "coder"),
		},
		{
			name:   "sticky active_profile=default is the root",
			in:     hermesHomeInputs{userHome: root},
			sticky: "default",
			want:   nativeHome,
		},
		{
			name:   "an explicit flag beats the sticky marker",
			in:     hermesHomeInputs{userHome: root, args: []string{"-p", "other"}},
			sticky: "coder",
			want:   filepath.Join(nativeHome, "profiles", "other"),
		},
		{
			name: "windows default without LOCALAPPDATA",
			in:   hermesHomeInputs{userHome: root, windows: true},
			want: filepath.Join(root, "AppData", "Local", "hermes"),
		},
		{
			name: "windows default with LOCALAPPDATA",
			in:   hermesHomeInputs{userHome: root, windows: true, localAppData: custom},
			want: filepath.Join(custom, "hermes"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			marker := filepath.Join(nativeHome, "active_profile")
			if tc.sticky != "" {
				if err := os.WriteFile(marker, []byte(tc.sticky), 0o600); err != nil {
					t.Fatalf("write active_profile: %v", err)
				}
				defer func() { _ = os.Remove(marker) }()
			}
			if got := hermesResolveHome(tc.in); got != tc.want {
				t.Errorf("hermesResolveHome = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestHermesConfigPathForResolvedHome pins that the config path is the
// resolved home's config.yaml — the ONE file both the model read and the
// provider write must agree on.
func TestHermesConfigPathForResolvedHome(t *testing.T) {
	root := t.TempDir()
	home := hermesResolveHome(hermesHomeInputs{userHome: root, env: filepath.Join(root, "hh")})
	if got, want := hermesConfigPathFor(home), filepath.Join(root, "hh", "config.yaml"); got != want {
		t.Errorf("config path = %q, want %q", got, want)
	}
}

// TestEnsureHermesObserverProviderWritesResolvedPath pins that the writer
// touches the path it is HANDED (a fixture home) and nothing else — in
// particular it must never fall back to the operator's real ~/.hermes.
func TestEnsureHermesObserverProviderWritesResolvedPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	const original = "model:\n    default: a/b\n    provider: openrouter\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if err := ensureHermesObserverProvider(path, "http://127.0.0.1:8820/up/openrouter/api/v1", "OPENROUTER_API_KEY"); err != nil {
		t.Fatalf("ensureHermesObserverProvider: %v", err)
	}
	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	for _, want := range []string{"observer:", "base_url: http://127.0.0.1:8820/up/openrouter/api/v1", "key_env: OPENROUTER_API_KEY", "default: a/b"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("written config missing %q:\n%s", want, out)
		}
	}
	// The model.default read earlier must still resolve from the SAME file.
	if m, err := hermesConfiguredModel(path); err != nil || m != "a/b" {
		t.Errorf("hermesConfiguredModel after write = (%q, %v), want (\"a/b\", nil)", m, err)
	}
	// A missing config is an error, never an authored fresh file.
	if err := ensureHermesObserverProvider(filepath.Join(dir, "nope", "config.yaml"), "http://x", "K"); err == nil {
		t.Error("expected an error for a missing config.yaml")
	}
}
