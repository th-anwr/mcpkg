package sources

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/mcpkg/mcpkg/internal/httpx"
	"github.com/mcpkg/mcpkg/internal/model"
)

// Npm finds npm packages that depend on an MCP SDK and ship a runnable bin.
type Npm struct {
	Registry string // https://registry.npmjs.org
	HTTP     *httpx.Client
	Queries  []string
	Log      *slog.Logger // optional: crawl progress
}

// Name implements Source.
func (n *Npm) Name() string { return "npm" }

// DefaultNpmQueries are the search texts used for discovery.
var DefaultNpmQueries = []string{
	"keywords:mcp", "keywords:mcp-server", "keywords:modelcontextprotocol",
	"keywords:model-context-protocol", "keywords:mcp-servers", "mcp server",
}

type npmSearch struct {
	Objects []struct {
		Package struct {
			Name        string    `json:"name"`
			Version     string    `json:"version"`
			Description string    `json:"description"`
			Keywords    []string  `json:"keywords"`
			Date        time.Time `json:"date"`
			Links       struct {
				Homepage   string `json:"homepage"`
				Repository string `json:"repository"`
			} `json:"links"`
		} `json:"package"`
	} `json:"objects"`
	Total int `json:"total"`
}

// NpmVersionDoc is GET /<name>/latest (a single version document).
type NpmVersionDoc struct {
	Name             string            `json:"name"`
	Version          string            `json:"version"`
	Description      string            `json:"description"`
	Bin              json.RawMessage   `json:"bin"`
	Dependencies     map[string]string `json:"dependencies"`
	PeerDependencies map[string]string `json:"peerDependencies"`
	Repository       json.RawMessage   `json:"repository"`
	Homepage         string            `json:"homepage"`
	License          json.RawMessage   `json:"license"`
	Scripts          map[string]string `json:"scripts"`
	Dist             struct {
		Tarball string `json:"tarball"`
	} `json:"dist"`
	Readme string `json:"readme"`
}

// Bins returns the executable names declared by the package.
func (d NpmVersionDoc) Bins() []string {
	if len(d.Bin) == 0 || string(d.Bin) == "null" {
		return nil
	}
	var s string
	if json.Unmarshal(d.Bin, &s) == nil && s != "" {
		name := d.Name
		if i := strings.LastIndex(name, "/"); i >= 0 {
			name = name[i+1:]
		}
		return []string{name}
	}
	var m map[string]string
	if json.Unmarshal(d.Bin, &m) == nil {
		out := make([]string, 0, len(m))
		for k := range m {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	return nil
}

// RepoInfo returns repository url and monorepo directory.
func (d NpmVersionDoc) RepoInfo() (string, string) {
	if len(d.Repository) == 0 {
		return "", ""
	}
	var s string
	if json.Unmarshal(d.Repository, &s) == nil {
		return s, ""
	}
	var o struct {
		URL       string `json:"url"`
		Directory string `json:"directory"`
	}
	_ = json.Unmarshal(d.Repository, &o)
	return o.URL, o.Directory
}

// MCPDep returns the MCP SDK dependency found, if any.
func (d NpmVersionDoc) MCPDep() string {
	for _, dep := range NpmMCPDeps {
		if _, ok := d.Dependencies[dep]; ok {
			return dep
		}
		if _, ok := d.PeerDependencies[dep]; ok {
			return dep
		}
	}
	return ""
}

// FetchNpmLatest loads the latest version document of a package.
func FetchNpmLatest(ctx context.Context, h *httpx.Client, registry, name string) (NpmVersionDoc, error) {
	var doc NpmVersionDoc
	err := h.GetJSON(ctx, registry+"/"+npmPath(name)+"/latest", nil, &doc)
	return doc, err
}

// FetchNpmVersion loads one version document.
func FetchNpmVersion(ctx context.Context, h *httpx.Client, registry, name, version string) (NpmVersionDoc, error) {
	var doc NpmVersionDoc
	if version == "" {
		version = "latest"
	}
	err := h.GetJSON(ctx, registry+"/"+npmPath(name)+"/"+url.PathEscape(version), nil, &doc)
	return doc, err
}

func npmPath(name string) string {
	if strings.HasPrefix(name, "@") {
		return "@" + url.PathEscape(strings.TrimPrefix(name, "@"))
	}
	return url.PathEscape(name)
}

// Crawl runs the search queries and keeps packages with an MCP SDK dependency and a bin.
func (n *Npm) Crawl(ctx context.Context, opt Options, emit func(Candidate) error) (string, error) {
	queries := n.Queries
	if len(queries) == 0 {
		queries = DefaultNpmQueries
	}
	seen := map[string]bool{}
	emitted := 0
	start := time.Now()
	if n.Log != nil {
		n.Log.Info("crawl start", "source", "npm", "queries", len(queries), "limit", opt.Limit)
	}
	for _, text := range queries {
		for from := 0; from < 5000; from += 250 {
			var res npmSearch
			u := fmt.Sprintf("%s/-/v1/search?text=%s&size=250&from=%d", n.Registry, url.QueryEscape(text), from)
			if err := n.HTTP.GetJSON(ctx, u, nil, &res); err != nil {
				return "", err
			}
			if len(res.Objects) == 0 {
				break
			}
			for _, o := range res.Objects {
				p := o.Package
				if seen[p.Name] {
					continue
				}
				seen[p.Name] = true
				doc, err := FetchNpmLatest(ctx, n.HTTP, n.Registry, p.Name)
				if err != nil {
					if n.Log != nil {
						n.Log.Warn("npm fetch failed", "name", p.Name, "err", err)
					}
					continue
				}
				dep := doc.MCPDep()
				bins := doc.Bins()
				if dep == "" || len(bins) == 0 {
					continue // not an MCP server we can run with npx
				}
				repoURL, dir := doc.RepoInfo()
				if repoURL == "" {
					repoURL = p.Links.Repository
				}
				published := p.Date
				c := Candidate{
					Source:        "npm",
					Name:          p.Name,
					Description:   firstNonEmpty(doc.Description, p.Description),
					Version:       doc.Version,
					RepoURL:       repoURL,
					RepoSubfolder: dir,
					WebsiteURL:    firstNonEmpty(doc.Homepage, p.Links.Homepage),
					Packages: []model.Package{{
						RegistryType: "npm", Identifier: p.Name, Version: doc.Version,
						Transport: model.Transport{Type: "stdio"},
					}},
					PublishedAt: &published,
					Raw: map[string]any{
						"name": p.Name, "version": doc.Version, "keywords": p.Keywords, "mcpDependency": dep,
						"bins": bins, "installScripts": installScripts(doc.Scripts), "date": p.Date,
					},
				}
				if err := emit(c); err != nil {
					return "", err
				}
				emitted++
				if opt.Limit > 0 && emitted >= opt.Limit {
					return "", nil
				}
			}
			if from+250 >= res.Total {
				break
			}
		}
	}
	if n.Log != nil {
		n.Log.Info("crawl done", "source", "npm", "emitted", emitted, "seen", len(seen), "ms", time.Since(start).Milliseconds())
	}
	return "", nil
}

func installScripts(s map[string]string) []string {
	var out []string
	for _, k := range []string{"preinstall", "install", "postinstall"} {
		if v, ok := s[k]; ok && strings.TrimSpace(v) != "" {
			out = append(out, k+": "+v)
		}
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
