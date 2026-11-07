package sources

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"time"

	"github.com/mcpkg/mcpkg/internal/httpx"
	"github.com/mcpkg/mcpkg/internal/model"
)

// Registry crawls the official MCP Registry (registry.modelcontextprotocol.io).
// The registry explicitly supports aggregators: poll hourly with updated_since.
type Registry struct {
	BaseURL  string
	HTTP     *httpx.Client
	PageSize int
}

// Name implements Source.
func (r *Registry) Name() string { return "registry" }

// serverJSON mirrors the parts of server.json we use.
type serverJSON struct {
	Name        string `json:"name"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Version     string `json:"version"`
	WebsiteURL  string `json:"websiteUrl"`
	Repository  struct {
		URL       string `json:"url"`
		Source    string `json:"source"`
		Subfolder string `json:"subfolder"`
	} `json:"repository"`
	Packages []model.Package            `json:"packages"`
	Remotes  []model.Remote             `json:"remotes"`
	Meta     map[string]json.RawMessage `json:"_meta"`
}

type officialMeta struct {
	Status      string     `json:"status"`
	PublishedAt *time.Time `json:"publishedAt"`
	UpdatedAt   *time.Time `json:"updatedAt"`
	IsLatest    *bool      `json:"isLatest"`
}

const officialMetaKey = "io.modelcontextprotocol.registry/official"

// listResponse tolerates both the wrapped ({server, _meta}) and flat item shapes.
type listResponse struct {
	Servers  []json.RawMessage `json:"servers"`
	Metadata struct {
		NextCursor string `json:"nextCursor"`
		Count      int    `json:"count"`
	} `json:"metadata"`
}

// ParseRegistryItem decodes one item from GET /v0.1/servers.
func ParseRegistryItem(raw json.RawMessage) (serverJSON, officialMeta, error) {
	var wrapped struct {
		Server *serverJSON                `json:"server"`
		Meta   map[string]json.RawMessage `json:"_meta"`
	}
	var s serverJSON
	var meta officialMeta
	if err := json.Unmarshal(raw, &wrapped); err == nil && wrapped.Server != nil && wrapped.Server.Name != "" {
		s = *wrapped.Server
		if m, ok := wrapped.Meta[officialMetaKey]; ok {
			_ = json.Unmarshal(m, &meta)
		}
	} else if err := json.Unmarshal(raw, &s); err != nil {
		return s, meta, err
	}
	if meta.Status == "" {
		if m, ok := s.Meta[officialMetaKey]; ok {
			_ = json.Unmarshal(m, &meta)
		}
	}
	if s.Name == "" {
		return s, meta, fmt.Errorf("registry item without name")
	}
	return s, meta, nil
}

// Crawl pages through the registry. opt.Since is an RFC3339 updated_since cursor.
func (r *Registry) Crawl(ctx context.Context, opt Options, emit func(Candidate) error) (string, error) {
	pageSize := r.PageSize
	if pageSize <= 0 {
		pageSize = 100
	}
	newest := opt.Since
	cursor := ""
	emitted := 0
	for {
		q := url.Values{}
		q.Set("limit", fmt.Sprint(pageSize))
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		if opt.Since != "" {
			q.Set("updated_since", opt.Since)
		}
		var page listResponse
		if err := r.HTTP.GetJSON(ctx, r.BaseURL+"/v0.1/servers?"+q.Encode(), nil, &page); err != nil {
			return newest, err
		}
		for _, item := range page.Servers {
			s, meta, err := ParseRegistryItem(item)
			if err != nil {
				continue
			}
			if meta.IsLatest != nil && !*meta.IsLatest {
				continue // only the latest version of each server
			}
			if meta.UpdatedAt != nil {
				if ts := meta.UpdatedAt.UTC().Format(time.RFC3339); ts > newest {
					newest = ts
				}
			}
			c := Candidate{
				Source:        "registry",
				Name:          s.Name,
				Title:         s.Title,
				Description:   s.Description,
				Version:       s.Version,
				RepoURL:       s.Repository.URL,
				RepoSubfolder: s.Repository.Subfolder,
				WebsiteURL:    s.WebsiteURL,
				Packages:      s.Packages,
				Remotes:       s.Remotes,
				Status:        meta.Status,
				PublishedAt:   meta.PublishedAt,
				UpdatedAt:     meta.UpdatedAt,
				Raw: map[string]any{
					"name": s.Name, "version": s.Version, "status": meta.Status,
					"publishedAt": meta.PublishedAt, "updatedAt": meta.UpdatedAt,
				},
			}
			if err := emit(c); err != nil {
				return newest, err
			}
			emitted++
			if opt.Limit > 0 && emitted >= opt.Limit {
				return newest, nil
			}
		}
		if page.Metadata.NextCursor == "" || len(page.Servers) == 0 {
			return newest, nil
		}
		cursor = page.Metadata.NextCursor
	}
}
