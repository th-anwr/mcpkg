package sources

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"time"

	"github.com/mcpkg/mcpkg/internal/httpx"
	"github.com/mcpkg/mcpkg/internal/model"
)

// Crates finds Rust crates that depend on an MCP SDK (reverse dependencies).
// crates.io asks crawlers for 1 request/second and a contact User-Agent; httpx enforces both.
type Crates struct {
	API  string // https://crates.io
	HTTP *httpx.Client
	SDKs []string
	Log  *slog.Logger // optional: crawl progress
}

// Name implements Source.
func (c *Crates) Name() string { return "crates" }

// DefaultRustSDKs are the MCP SDK crates.
var DefaultRustSDKs = []string{"rmcp", "rust-mcp-sdk"}

type crateRevDeps struct {
	Versions []struct {
		Crate string `json:"crate"`
		Num   string `json:"num"`
	} `json:"versions"`
	Meta struct {
		Total int `json:"total"`
	} `json:"meta"`
}

type crateInfo struct {
	Crate struct {
		Name            string    `json:"name"`
		Description     string    `json:"description"`
		Repository      string    `json:"repository"`
		Homepage        string    `json:"homepage"`
		MaxStable       string    `json:"max_stable_version"`
		MaxVersion      string    `json:"max_version"`
		Downloads       int64     `json:"downloads"`
		RecentDownloads int64     `json:"recent_downloads"`
		UpdatedAt       time.Time `json:"updated_at"`
	} `json:"crate"`
}

// Crawl lists reverse dependencies of each SDK crate.
func (c *Crates) Crawl(ctx context.Context, opt Options, emit func(Candidate) error) (string, error) {
	sdks := c.SDKs
	if len(sdks) == 0 {
		sdks = DefaultRustSDKs
	}
	seen := map[string]bool{}
	emitted := 0
	start := time.Now()
	if c.Log != nil {
		c.Log.Info("crawl start", "source", "crates", "sdks", len(sdks), "limit", opt.Limit)
	}
	for _, sdk := range sdks {
		for page := 1; page <= 100; page++ {
			var rd crateRevDeps
			u := fmt.Sprintf("%s/api/v1/crates/%s/reverse_dependencies?per_page=100&page=%d", c.API, url.PathEscape(sdk), page)
			if err := c.HTTP.GetJSON(ctx, u, nil, &rd); err != nil {
				if httpx.IsNotFound(err) {
					break
				}
				return "", err
			}
			if len(rd.Versions) == 0 {
				break
			}
			for _, v := range rd.Versions {
				if seen[v.Crate] {
					continue
				}
				seen[v.Crate] = true
				var info crateInfo
				if err := c.HTTP.GetJSON(ctx, fmt.Sprintf("%s/api/v1/crates/%s", c.API, url.PathEscape(v.Crate)), nil, &info); err != nil {
					if c.Log != nil {
						c.Log.Warn("crate fetch failed", "crate", v.Crate, "err", err)
					}
					continue
				}
				ver := info.Crate.MaxStable
				if ver == "" {
					ver = info.Crate.MaxVersion
				}
				updated := info.Crate.UpdatedAt
				cand := Candidate{
					Source:      "crates",
					Name:        info.Crate.Name,
					Description: info.Crate.Description,
					Version:     ver,
					RepoURL:     info.Crate.Repository,
					WebsiteURL:  info.Crate.Homepage,
					Packages: []model.Package{{
						RegistryType: "crate", Identifier: info.Crate.Name, Version: ver,
						Transport: model.Transport{Type: "stdio"},
					}},
					UpdatedAt: &updated,
					Raw:       map[string]any{"name": info.Crate.Name, "sdk": sdk, "downloads": info.Crate.Downloads, "recentDownloads": info.Crate.RecentDownloads},
				}
				if err := emit(cand); err != nil {
					return "", err
				}
				emitted++
				if opt.Limit > 0 && emitted >= opt.Limit {
					return "", nil
				}
			}
			if page*100 >= rd.Meta.Total {
				break
			}
		}
	}
	if c.Log != nil {
		c.Log.Info("crawl done", "source", "crates", "emitted", emitted, "ms", time.Since(start).Milliseconds())
	}
	return "", nil
}
