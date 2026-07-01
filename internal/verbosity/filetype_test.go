package verbosity

import "testing"

func TestFileType(t *testing.T) {
	cases := []struct {
		path     string
		wantLang string
		wantCat  Category
		wantOK   bool
	}{
		{"internal/store/verbosity.go", "go", Code, true},
		{"web/src/App.tsx", "tsx", Code, true},
		{"scripts/release.sh", "bash", Code, true},    // shell = code
		{"deploy/run.ps1", "powershell", Code, true},  // shell = code
		{"build.bat", "batch", Code, true},            // shell = code
		{`C:\Users\me\go.mod`, "gomod", Config, true}, // windows sep + filename rule
		{"Makefile", "make", Code, true},              // extensionless filename rule
		{"ops/Dockerfile", "dockerfile", Code, true},  // filename rule
		{".gitignore", "gitignore", Config, true},     // dotfile
		{"README.md", "markdown", Docs, true},
		{"config/app.toml", "toml", Config, true},
		{"data/rows.csv", "csv", Data, true},
		{"query.sql", "sql", Code, true},
		{"notes.txt", "text", Prose, true},
		{"foo.bar.go", "go", Code, true},    // multi-dot → last ext
		{"weird.xyzzy", "", Unknown, false}, // unknown ext → ledger
		{"LICENSE", "license", Docs, true},
		{"no_ext_file", "", Unknown, false},
		// P6 close-out additions (from the live unknown-ext ledger).
		{"queries/imports.scm", "scheme", Code, true},
		{"notebook.ipynb", "jupyter", Code, true},
		{"data/events.jsonl", "json", Config, true},
		{"build/out.log", "log", Data, true},
		{"bin/observer.exe", "binary", Data, true},
		{"tsconfig.tsbuildinfo", "json", Config, true},
	}
	for _, c := range cases {
		got, ok := FileType(c.path)
		if ok != c.wantOK {
			t.Errorf("FileType(%q) ok = %v, want %v", c.path, ok, c.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if got.Name != c.wantLang || got.Category != c.wantCat {
			t.Errorf("FileType(%q) = {%s,%s}, want {%s,%s}", c.path, got.Name, got.Category, c.wantLang, c.wantCat)
		}
	}
}

func TestLangForTag(t *testing.T) {
	cases := []struct {
		tag      string
		wantLang string
		wantCat  Category
		wantOK   bool
	}{
		{"go", "go", Code, true},
		{"golang", "go", Code, true},
		{"bash", "bash", Code, true},
		{"sh", "bash", Code, true},
		{"shell", "bash", Code, true},
		{"console", "bash", Code, true},
		{"powershell", "powershell", Code, true},
		{"ps1", "powershell", Code, true},
		{"pwsh", "powershell", Code, true},
		{"ts", "typescript", Code, true},
		{"py", "python", Code, true},
		{"c++", "cpp", Code, true},
		{"c#", "csharp", Code, true},
		{"json", "json", Config, true},
		{"yaml", "yaml", Config, true},
		{"yml", "yaml", Config, true},
		{"markdown", "markdown", Docs, true},
		{"md", "markdown", Docs, true},
		{"text", "text", Prose, true},
		{"  Bash  ", "bash", Code, true}, // trim + case-insensitive
		{"notalanguage", "", Unknown, false},
		{"", "", Unknown, false},
	}
	for _, c := range cases {
		got, ok := LangForTag(c.tag)
		if ok != c.wantOK {
			t.Errorf("LangForTag(%q) ok = %v, want %v", c.tag, ok, c.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if got.Name != c.wantLang || got.Category != c.wantCat {
			t.Errorf("LangForTag(%q) = {%s,%s}, want {%s,%s}", c.tag, got.Name, got.Category, c.wantLang, c.wantCat)
		}
	}
}

// TestEveryResolverValueHasCategory guards the one-owner invariant: every
// canonical language reachable through extLang / filenameLang / tagAlias
// must be registered in langCategory, else CategoryOf returns Unknown and
// the report mislabels real content.
func TestEveryResolverValueHasCategory(t *testing.T) {
	check := func(src string, m map[string]string) {
		for k, lang := range m {
			if _, ok := langCategory[lang]; !ok {
				t.Errorf("%s[%q] = %q has no langCategory entry", src, k, lang)
			}
		}
	}
	check("extLang", extLang)
	check("filenameLang", filenameLang)
	check("tagAlias", tagAlias)
}
