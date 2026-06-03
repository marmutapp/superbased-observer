package codexipc

import (
	"sort"
	"strings"
	"testing"
)

// TestParseTasklistCSV_DetectsV5DocReproducer pins the PID 10072 +
// PID 35388 case from docs/observer-platform-issues-v5.md §V5-1. The
// CSV shape mirrors the ConvertTo-Csv output of Get-CimInstance
// Win32_Process | Select-Object ProcessId,Name,Path,CommandLine.
func TestParseTasklistCSV_DetectsV5DocReproducer(t *testing.T) {
	in := []byte(`"ProcessId","Name","Path","CommandLine"
"10072","codex.exe","c:\Users\marmu\.vscode\extensions\openai.chatgpt-26.519.32039-win32-x64\bin\windows-x86_64\codex.exe","""c:\Users\marmu\.vscode\extensions\openai.chatgpt-26.519.32039-win32-x64\bin\windows-x86_64\codex.exe"" app-server --analytics-default-enabled"
"35388","codex.exe","c:\Users\marmu\.vscode\extensions\openai.chatgpt-26.519.32039-win32-x64\bin\windows-x86_64\codex.exe","""c:\Users\marmu\.vscode\extensions\openai.chatgpt-26.519.32039-win32-x64\bin\windows-x86_64\codex.exe"" app-server --analytics-default-enabled"
`)
	got := parseTasklistCSV(in)
	if len(got) != 2 {
		t.Fatalf("expected 2 processes, got %d: %+v", len(got), got)
	}
	sort.Slice(got, func(i, j int) bool { return got[i].PID < got[j].PID })
	if got[0].PID != 10072 || got[1].PID != 35388 {
		t.Errorf("PIDs: got %d, %d; want 10072, 35388", got[0].PID, got[1].PID)
	}
	for _, p := range got {
		if p.Source != "vscode-extension" {
			t.Errorf("PID %d Source: got %q, want vscode-extension", p.PID, p.Source)
		}
		if !strings.Contains(p.CommandLine, "app-server") {
			t.Errorf("PID %d CommandLine missing app-server: %q", p.PID, p.CommandLine)
		}
	}
}

// TestParseTasklistCSV_ExcludesNonAppServer pins that codex.exe
// running `exec` (not app-server) is NOT detected — only long-running
// app-server holders are.
func TestParseTasklistCSV_ExcludesNonAppServer(t *testing.T) {
	in := []byte(`"ProcessId","Name","Path","CommandLine"
"4444","codex.exe","C:\Users\marmu\AppData\Local\OpenAI\Codex\bin\codex.exe","""codex.exe"" exec ""reply OK"""
`)
	got := parseTasklistCSV(in)
	if len(got) != 0 {
		t.Errorf("expected 0 processes for plain `exec`, got %d: %+v", len(got), got)
	}
}

// TestParseTasklistCSV_ExcludesElectronChildren pins that the
// `--type=renderer` / `--type=utility` child processes that Electron-
// based codex variants spawn under the same image name are NOT
// detected. The v5 doc's PowerShell reproducer (line 135) uses the
// same heuristic.
func TestParseTasklistCSV_ExcludesElectronChildren(t *testing.T) {
	in := []byte(`"ProcessId","Name","Path","CommandLine"
"9001","codex.exe","C:\Path\codex.exe","""codex.exe"" app-server --type=renderer --enable-features=foo"
"9002","codex.exe","C:\Path\codex.exe","""codex.exe"" app-server --type=utility"
`)
	got := parseTasklistCSV(in)
	if len(got) != 0 {
		t.Errorf("expected 0 processes for --type= children, got %d: %+v", len(got), got)
	}
}

// TestParseTasklistCSV_HeaderOnly returns empty (clean host case).
func TestParseTasklistCSV_HeaderOnly(t *testing.T) {
	in := []byte(`"ProcessId","Name","Path","CommandLine"` + "\n")
	got := parseTasklistCSV(in)
	if len(got) != 0 {
		t.Errorf("expected 0 processes for header-only output, got %d", len(got))
	}
}

// TestParseTasklistCSV_Empty pins the empty-input guard.
func TestParseTasklistCSV_Empty(t *testing.T) {
	if got := parseTasklistCSV(nil); len(got) != 0 {
		t.Errorf("expected nil for nil input, got %+v", got)
	}
	if got := parseTasklistCSV([]byte{}); len(got) != 0 {
		t.Errorf("expected nil for empty input, got %+v", got)
	}
}

// TestParseTasklistCSV_CodexDesktopClassification covers the
// Source = "codex-desktop" path (Windows AppData install).
func TestParseTasklistCSV_CodexDesktopClassification(t *testing.T) {
	in := []byte(`"ProcessId","Name","Path","CommandLine"
"7777","Codex.exe","C:\Users\u\AppData\Local\Programs\Codex\Codex.exe","""Codex.exe"" app-server"
`)
	got := parseTasklistCSV(in)
	if len(got) != 1 {
		t.Fatalf("expected 1 process, got %d", len(got))
	}
	if got[0].Source != "codex-desktop" {
		t.Errorf("Source: got %q, want codex-desktop", got[0].Source)
	}
}

// TestParsePSOutput_DetectsAppServer covers the POSIX path.
func TestParsePSOutput_DetectsAppServer(t *testing.T) {
	in := []byte(`  PID COMMAND          COMMAND
12345 codex            /home/u/.codex/bin/codex app-server --analytics-default-enabled
67890 bash             /bin/bash -c something else
22222 codex            /opt/Codex/codex app-server --type=renderer
33333 codex            /home/u/.vscode/extensions/openai.chatgpt-26.5/bin/codex app-server
`)
	got := parsePSOutput(in)
	if len(got) != 2 {
		t.Fatalf("expected 2 processes, got %d: %+v", len(got), got)
	}
	sort.Slice(got, func(i, j int) bool { return got[i].PID < got[j].PID })
	if got[0].PID != 12345 {
		t.Errorf("first PID: got %d, want 12345", got[0].PID)
	}
	if got[1].PID != 33333 {
		t.Errorf("second PID: got %d, want 33333", got[1].PID)
	}
	if got[1].Source != "vscode-extension" {
		t.Errorf("PID 33333 Source: got %q, want vscode-extension", got[1].Source)
	}
}

// TestParsePSOutput_ExcludesNonCodexAndChildren pins the negative
// filters: non-codex processes, --type= children, and plain `codex
// exec` invocations all skipped.
func TestParsePSOutput_ExcludesNonCodexAndChildren(t *testing.T) {
	in := []byte(`  PID COMMAND          COMMAND
1111 bash             /bin/bash codex app-server
2222 codex            /usr/bin/codex exec "hello"
3333 codex            /usr/bin/codex app-server --type=utility
`)
	got := parsePSOutput(in)
	if len(got) != 0 {
		t.Errorf("expected 0 processes, got %d: %+v", len(got), got)
	}
}

// TestParsePSOutput_Empty covers the empty-input guard.
func TestParsePSOutput_Empty(t *testing.T) {
	if got := parsePSOutput(nil); len(got) != 0 {
		t.Errorf("expected nil for nil input, got %+v", got)
	}
}

// TestClassifySource is table-driven over the path heuristics.
func TestClassifySource(t *testing.T) {
	cases := []struct {
		name  string
		probe string
		want  string
	}{
		{
			name:  "vscode extension Windows",
			probe: `C:\Users\u\.vscode\extensions\openai.chatgpt-26.519.32039-win32-x64\bin\windows-x86_64\codex.exe app-server`,
			want:  "vscode-extension",
		},
		{
			name:  "vscode extension POSIX",
			probe: "/home/u/.vscode/extensions/openai.chatgpt-26.5/bin/codex app-server",
			want:  "vscode-extension",
		},
		{
			name:  "codex desktop macOS",
			probe: "/Applications/Codex.app/Contents/MacOS/Codex app-server",
			want:  "codex-desktop",
		},
		{
			name:  "codex desktop Windows installer",
			probe: `C:\Users\u\AppData\Local\Programs\Codex\Codex.exe app-server`,
			want:  "codex-desktop",
		},
		{
			name:  "codex desktop Linux AppImage drop",
			probe: "/opt/Codex/codex app-server",
			want:  "codex-desktop",
		},
		{
			name:  "unknown plain bin path",
			probe: "/usr/local/bin/codex app-server",
			want:  "unknown",
		},
		{
			name:  "vscode without openai.chatgpt does not match",
			probe: "/home/u/.vscode/extensions/some-other-ext/codex app-server",
			want:  "unknown",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifySource(tc.probe); got != tc.want {
				t.Errorf("classifySource(%q) = %q, want %q", tc.probe, got, tc.want)
			}
		})
	}
}

// TestStartsWithDigitRune covers the header-vs-data discriminator
// used by parsePSOutput.
func TestStartsWithDigitRune(t *testing.T) {
	cases := map[string]bool{
		"  PID COMMAND  COMMAND":  false,
		"PID":                     false,
		"  123 codex app-server":  true,
		"1":                       true,
		"":                        false,
		"   ":                     false,
		"\t456 codex app-server ": true,
	}
	for in, want := range cases {
		if got := startsWithDigitRune(in); got != want {
			t.Errorf("startsWithDigitRune(%q) = %v, want %v", in, got, want)
		}
	}
}
