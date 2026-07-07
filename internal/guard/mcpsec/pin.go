package mcpsec

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
)

// Composite pin encoding (doc.go "Pin format"). The pin_hash column
// stores "v1 cfg:<sha256hex> tools:<sha256hex|->"; this file is the
// one owner of that format — the store treats pin_hash as opaque
// text, and the cmd layer round-trips through Encode/Decode.

// pinVersion prefixes the encoded form so a future format can coexist
// with v1 rows.
const pinVersion = "v1"

// toolsUnobserved is the tools-half placeholder before any tool set
// has been observed for the server.
const toolsUnobserved = "-"

// PinHash is the decoded composite pin.
type PinHash struct {
	// Cfg is the sha256 hex over the server's config shape
	// (transport + command/URL + args + env keys).
	Cfg string
	// Tools is the sha256 hex over the observed tool declarations,
	// "" while unobserved.
	Tools string
}

// EncodePinHash renders the stored pin_hash form.
func EncodePinHash(h PinHash) string {
	tools := h.Tools
	if tools == "" {
		tools = toolsUnobserved
	}
	return pinVersion + " cfg:" + h.Cfg + " tools:" + tools
}

// DecodePinHash parses a stored pin_hash. ok=false means the value
// doesn't carry the v1 composite shape (foreign/corrupt row) — the
// diff layer then treats the pin as carrying no usable component
// hashes and re-pins silently (degrade, don't false-fire).
func DecodePinHash(s string) (PinHash, bool) {
	fields := strings.Fields(s)
	if len(fields) != 3 || fields[0] != pinVersion {
		return PinHash{}, false
	}
	cfg, okCfg := strings.CutPrefix(fields[1], "cfg:")
	tools, okTools := strings.CutPrefix(fields[2], "tools:")
	if !okCfg || !okTools || cfg == "" {
		return PinHash{}, false
	}
	if tools == toolsUnobserved {
		tools = ""
	}
	return PinHash{Cfg: cfg, Tools: tools}, true
}

// ConfigHash computes the cfg half over the server's launch shape.
// Field order is fixed and NUL-delimited so renames between fields
// can't collide; env keys are already sorted at parse time.
func ConfigHash(s Server) string {
	h := sha256.New()
	for _, part := range append([]string{s.Transport, s.Command}, s.Args...) {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	h.Write([]byte{1}) // args/env section separator
	for _, k := range s.EnvKeys {
		h.Write([]byte(k))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ToolsHash computes the tools half over a server's observed tool
// declarations: sorted by tool name, each contributing name,
// description and param docs (the §9.2 "sorted tool names + tool
// description texts"). Empty input yields "" (unobserved).
func ToolsHash(decls []ToolDecl) string {
	if len(decls) == 0 {
		return ""
	}
	sorted := make([]ToolDecl, len(decls))
	copy(sorted, decls)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	h := sha256.New()
	for _, d := range sorted {
		for _, part := range []string{d.Name, d.Description, d.ParamDoc} {
			h.Write([]byte(part))
			h.Write([]byte{0})
		}
		h.Write([]byte{1})
	}
	return hex.EncodeToString(h.Sum(nil))
}
