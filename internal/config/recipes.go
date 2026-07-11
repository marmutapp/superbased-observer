package config

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// recipesFS embeds the operator-facing compression recipes shipped
// alongside the binary. Source-of-truth copies are mirrored to
// docs/recipes/ for documentation; the recipes_sync_test verifies
// byte-equality between the two locations.
//
// Adding a recipe: drop a new <name>.toml under internal/config/recipes/
// AND under docs/recipes/, then add the carve-out entry to
// scripts/release.sh's 3-site sync for the public mirror.
//
//go:embed recipes/*.toml
var recipesFS embed.FS

// RecipeNames returns the sorted list of available recipe names
// (filename without `.toml` extension). Used by the `--recipe` flag's
// completion + help text + invalid-name error message.
func RecipeNames() []string {
	entries, err := fs.ReadDir(recipesFS, "recipes")
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".toml") {
			continue
		}
		names = append(names, strings.TrimSuffix(name, ".toml"))
	}
	sort.Strings(names)
	return names
}

// LoadRecipe returns a Config whose values are [Default] overlaid by
// the embedded recipe identified by name. The returned Config is NOT
// validated — callers apply env / user-config overrides on top first.
// Unknown name returns an error listing the available recipes.
//
// Example merge order in `observer start --recipe codex-safe`:
//
//	Default() → recipe → ~/.observer/config.toml → env vars → Validate
//
// Recipes are intentionally minimal — they only touch keys the
// recipe needs to change. All other Default() values pass through
// untouched.
func LoadRecipe(name string) (Config, error) {
	if name == "" {
		return Default(), nil
	}
	body, err := readRecipe(name)
	if err != nil {
		return Config{}, err
	}
	cfg := Default()
	if err := toml.Unmarshal(body, &cfg); err != nil {
		return Config{}, fmt.Errorf("config: parse recipe %q: %w", name, err)
	}
	return cfg, nil
}

// readRecipe returns the raw embedded TOML for the named recipe.
// Unknown name returns an error listing the available recipes. Shared
// by LoadRecipe (recipe over Default) and ResolveCompression (recipe
// over master — the profile overlay).
func readRecipe(name string) ([]byte, error) {
	body, err := recipesFS.ReadFile("recipes/" + name + ".toml")
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("config: unknown recipe %q (available: %s)", name, strings.Join(RecipeNames(), ", "))
		}
		return nil, fmt.Errorf("config: read recipe %q: %w", name, err)
	}
	return body, nil
}
