// Package sources discovers MCP servers from public registries and code hosts.
// Every source emits Candidates; the discover package dedupes and merges them.
package sources

import (
	"context"
	"time"

	"github.com/mcpkg/mcpkg/internal/model"
)

// Candidate is one discovered server, as seen by a single source.
type Candidate struct {
	Source        string // registry | npm | pypi | github | crates | submit
	Name          string // primary name within that source
	Title         string
	Description   string
	Version       string
	RepoURL       string
	RepoSubfolder string
	WebsiteURL    string
	Packages      []model.Package
	Remotes       []model.Remote
	Status        string // active | deprecated | deleted (registry only)
	PublishedAt   *time.Time
	UpdatedAt     *time.Time
	Stars         int
	Raw           map[string]any // stored under tools.sources[Source]
}

// Options limit a crawl (useful for tests and first runs).
type Options struct {
	Limit int    // max candidates to emit (0 = unlimited)
	Since string // source-specific incremental cursor
}

// Source is implemented by every crawler.
type Source interface {
	Name() string
	// Crawl streams candidates to emit. It returns the next cursor to persist.
	Crawl(ctx context.Context, opt Options, emit func(Candidate) error) (cursor string, err error)
}

// MCP SDK packages: a dependency on one of these marks a package as MCP-related.
var NpmMCPDeps = []string{
	"@modelcontextprotocol/sdk", "fastmcp", "mcp-framework", "xmcp", "mcp-handler",
	"@vercel/mcp-adapter", "tmcp", "@mastra/mcp", "mcp-use", "@modelcontextprotocol/server",
}

// PyPIMCPDeps are Python distributions that indicate an MCP server.
var PyPIMCPDeps = []string{"mcp", "fastmcp", "mcp-server", "fastapi-mcp"}
