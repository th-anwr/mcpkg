// Package launch decides how to start a server for the smoke test from its
// package metadata (registry server.json, npm, PyPI) without guessing commands.
package launch

import (
	"regexp"
	"sort"
	"strings"

	"github.com/mcpkg/mcpkg/internal/identity"
	"github.com/mcpkg/mcpkg/internal/model"
)

// Placeholder is the dummy value given to required secrets during smoke tests.
const Placeholder = "mcpkg-smoke-placeholder"

// Plan is the chosen way to start a server.
type Plan struct {
	Kind        string // npm | pypi | oci | remote | none
	PackageIdx  int
	Package     model.Package
	Remote      model.Remote
	Args        []string
	Env         map[string]string
	PreferBin   string
	EntryPoints []string
	SkipReason  string
}

var tmplVar = regexp.MustCompile(`\{[^{}]+\}`)

// Choose picks the best smoke-testable launch method. pathPlaceholder is a
// writable directory inside the sandbox used for required path arguments.
func Choose(t *model.Tool, pathPlaceholder string) Plan {
	order := map[string]int{"npm": 0, "pypi": 1, "oci": 2}
	idx := -1
	for i, p := range t.Packages {
		rank, ok := order[p.RegistryType]
		if !ok {
			continue
		}
		if tt := p.Transport.Type; tt != "" && tt != "stdio" {
			continue
		}
		if idx < 0 || rank < order[t.Packages[idx].RegistryType] {
			idx = i
		}
	}
	if idx >= 0 {
		p := t.Packages[idx]
		plan := Plan{Kind: p.RegistryType, PackageIdx: idx, Package: p, PreferBin: p.Bin}
		plan.Args = BuildArgs(p.PackageArguments, pathPlaceholder)
		plan.Env = BuildEnv(p.EnvironmentVariables, pathPlaceholder)
		if t.Security != nil {
			plan.EntryPoints = t.Security.EntryPoints
		}
		return plan
	}
	// Prefer Streamable HTTP over legacy SSE.
	rem := append([]model.Remote(nil), t.Remotes...)
	sort.SliceStable(rem, func(i, j int) bool { return rem[i].Type == "streamable-http" && rem[j].Type != "streamable-http" })
	for _, r := range rem {
		if strings.HasPrefix(r.URL, "https://") || strings.HasPrefix(r.URL, "http://") {
			if tmplVar.MatchString(r.URL) {
				return Plan{Kind: "none", SkipReason: "remote URL needs user-specific variables"}
			}
			return Plan{Kind: "remote", Remote: r}
		}
	}
	for _, p := range t.Packages {
		switch {
		case p.Transport.Type == "streamable-http" || p.Transport.Type == "sse":
			return Plan{Kind: "none", SkipReason: "package runs a local HTTP server (not smoke-tested yet)"}
		case p.RegistryType == "crate":
			return Plan{Kind: "none", SkipReason: "Rust crate: build-from-source not smoke-tested yet"}
		case p.RegistryType == "nuget" || p.RegistryType == "mcpb":
			return Plan{Kind: "none", SkipReason: p.RegistryType + " packages are not smoke-tested yet"}
		}
	}
	if t.RepoURL != "" {
		return Plan{Kind: "none", SkipReason: "no published package: source-only repository"}
	}
	return Plan{Kind: "none", SkipReason: "nothing installable"}
}

// BuildArgs turns server.json arguments into argv, filling required ones with placeholders.
func BuildArgs(args []model.Argument, pathPlaceholder string) []string {
	var out []string
	for _, a := range args {
		v := a.Value
		if v == "" {
			v = a.Default
		}
		if v == "" && a.IsRequired {
			v = placeholderFor(a.Name+" "+a.ValueHint+" "+a.Description, a.Format, pathPlaceholder)
		}
		if v == "" {
			continue
		}
		v = tmplVar.ReplaceAllString(v, Placeholder)
		if a.Type == "named" && a.Name != "" {
			out = append(out, a.Name, v)
		} else {
			out = append(out, v)
		}
	}
	return out
}

// BuildEnv sets required variables to defaults or placeholders.
func BuildEnv(vars []model.EnvVar, pathPlaceholder string) map[string]string {
	env := map[string]string{}
	for _, v := range vars {
		if v.Name == "" {
			continue
		}
		val := v.Default
		if val == "" && v.IsRequired {
			val = placeholderFor(v.Name+" "+v.Description, v.Format, pathPlaceholder)
		}
		if val != "" {
			env[v.Name] = tmplVar.ReplaceAllString(val, Placeholder)
		}
	}
	return env
}

// Names like ALLOWED_DIRECTORY or data_dir count: underscores are separators here.
var pathish = regexp.MustCompile(`(?i)(^|[^a-z])(path|dir|directory|folder|root|workspace|file)s?([^a-z]|$)`)
var urlish = regexp.MustCompile(`(?i)(^|[^a-z])(url|endpoint|host|base_?url)([^a-z]|$)`)

func placeholderFor(text, format, pathPlaceholder string) string {
	switch {
	case format == "filepath" || pathish.MatchString(text):
		return pathPlaceholder
	case urlish.MatchString(text):
		return "https://example.com"
	case format == "number":
		return "1"
	case format == "boolean":
		return "false"
	}
	return Placeholder
}

// PickBin chooses the executable among a package's bins/entry points.
func PickBin(pkg string, bins []string, prefer string, hints []string) string {
	if len(bins) == 0 {
		return ""
	}
	has := map[string]bool{}
	for _, b := range bins {
		has[b] = true
	}
	if prefer != "" && has[prefer] {
		return prefer
	}
	for _, h := range hints {
		if has[h] {
			return h
		}
	}
	short := pkg
	if i := strings.LastIndex(short, "/"); i >= 0 {
		short = short[i+1:]
	}
	norm := identity.PyPIName(short)
	for _, b := range bins {
		if b == short || identity.PyPIName(b) == norm {
			return b
		}
	}
	if len(bins) == 1 {
		return bins[0]
	}
	sorted := append([]string(nil), bins...)
	sort.Strings(sorted)
	for _, b := range sorted {
		if strings.Contains(strings.ToLower(b), "mcp") {
			return b
		}
	}
	return sorted[0]
}
