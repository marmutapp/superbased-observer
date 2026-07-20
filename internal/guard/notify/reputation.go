package notify

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// Reputation lookups (guard spec §15.3): optional npm-registry
// metadata for MCP server packages. v1 is the on-demand surface
// (`observer guard mcp reputation <package>`) — lookups route through
// the Egress worker like every other cloud call, the registry host is
// allowlisted only when [guard.cloud].reputation.enabled, and results
// are presented to the operator rather than auto-firing rules (a
// young-package rule needs threshold calibration first; recorded as
// deferred in the spec §22 tick). GitHub metadata and curl|sh domain
// reputation join the same builder pattern later.

// NPMRegistryBase is the allowlist prefix and lookup base. The
// trailing slash makes it a prefix-form allow entry.
const NPMRegistryBase = "https://registry.npmjs.org/"

// BuildNPMLookup renders the metadata GET for one package name
// (scoped names are escaped per the registry's URL convention).
func BuildNPMLookup(pkg string) (Request, error) {
	if pkg == "" {
		return Request{}, fmt.Errorf("notify.BuildNPMLookup: package name required")
	}
	// The registry wants %2f for the scope separator; PathEscape
	// produces exactly that for "/".
	return Request{
		Feature:  "reputation",
		Endpoint: NPMRegistryBase + url.PathEscape(pkg),
		Method:   http.MethodGet,
	}, nil
}

// NPMPackageInfo is the operator-facing reputation summary.
type NPMPackageInfo struct {
	Name         string
	Description  string
	Latest       string
	VersionCount int
	// CreatedAt / ModifiedAt are the registry's package timestamps.
	CreatedAt  time.Time
	ModifiedAt time.Time
	// AgeDays is the whole-day age of the package at `now`.
	AgeDays int
	// Maintainers is the registry's maintainer count.
	Maintainers int
}

// ParseNPMMetadata extracts the reputation summary from a registry
// metadata document.
func ParseNPMMetadata(body []byte, now time.Time) (NPMPackageInfo, error) {
	var doc struct {
		Name        string            `json:"name"`
		Description string            `json:"description"`
		DistTags    map[string]string `json:"dist-tags"`
		Versions    map[string]any    `json:"versions"`
		Time        map[string]string `json:"time"`
		Maintainers []any             `json:"maintainers"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return NPMPackageInfo{}, fmt.Errorf("notify.ParseNPMMetadata: %w", err)
	}
	if doc.Name == "" {
		return NPMPackageInfo{}, fmt.Errorf("notify.ParseNPMMetadata: not a package document")
	}
	info := NPMPackageInfo{
		Name:         doc.Name,
		Description:  bound(doc.Description),
		Latest:       doc.DistTags["latest"],
		VersionCount: len(doc.Versions),
		Maintainers:  len(doc.Maintainers),
	}
	if created, err := time.Parse(time.RFC3339, doc.Time["created"]); err == nil {
		info.CreatedAt = created
		if now.After(created) {
			info.AgeDays = int(now.Sub(created).Hours() / 24)
		}
	}
	if modified, err := time.Parse(time.RFC3339, doc.Time["modified"]); err == nil {
		info.ModifiedAt = modified
	}
	return info, nil
}

// FormatNPMInfo renders the operator-facing summary lines.
func FormatNPMInfo(info NPMPackageInfo) []string {
	lines := []string{
		fmt.Sprintf("package:     %s", info.Name),
		fmt.Sprintf("latest:      %s (%d versions)", info.Latest, info.VersionCount),
		fmt.Sprintf("age:         %d days (created %s)", info.AgeDays, fmtDay(info.CreatedAt)),
		fmt.Sprintf("modified:    %s", fmtDay(info.ModifiedAt)),
		fmt.Sprintf("maintainers: %d", info.Maintainers),
	}
	if info.Description != "" {
		lines = append(lines, "description: "+info.Description)
	}
	return lines
}

// fmtDay renders a date or "unknown".
func fmtDay(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.UTC().Format("2006-01-02")
}
