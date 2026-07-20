package routing

import "strings"

// ResolveCommandClass maps a run_command action's command text to its
// CommandClass capability flag. This is the documented boundary adapter
// (§24.3): loaders call it once at row-resolution time and discard the
// command text — classifier rules and the decision engine only ever see
// the resulting flag, never content.
//
// The resolution is an ordered keyword table walked top-down, first
// match wins. Test commands are checked before build (so "make test"
// is a test, bare "make" a build), lint before build (so "go vet" is
// lint, "go build" build). Unmatched commands are CommandOther — a
// real command whose class we don't claim to know; empty input is
// CommandNone.
func ResolveCommandClass(command string) CommandClass {
	cmd := strings.TrimSpace(strings.ToLower(command))
	if cmd == "" {
		return CommandNone
	}
	for _, row := range commandClassRules {
		for _, kw := range row.keywords {
			if strings.Contains(cmd, kw) {
				return row.class
			}
		}
	}
	return CommandOther
}

// commandClassRules is the ordered keyword table. Keywords are matched
// as substrings of the lowercased command; rows are walked top-down.
var commandClassRules = []struct {
	class    CommandClass
	keywords []string
}{
	{CommandTest, []string{
		"go test", "pytest", "npm test", "yarn test", "pnpm test",
		"cargo test", "make test", "vitest", "jest", "rspec", "phpunit",
		"ctest", "dotnet test", "mvn test", "gradle test", "tox",
	}},
	{CommandLint, []string{
		"golangci-lint", "go vet", "gofmt", "gofumpt", "eslint", "ruff",
		"flake8", "prettier", "clippy", "black ", "mypy", "tsc --noemit",
		"shellcheck", "staticcheck",
	}},
	{CommandBuild, []string{
		"go build", "cargo build", "npm run build", "yarn build",
		"pnpm build", "make", "tsc", "gradle", "mvn", "dotnet build",
		"cmake", "go install",
	}},
	{CommandVCS, []string{
		"git ", "gh ", "svn ", "hg ",
	}},
}
