package sources

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mcpkg/mcpkg/internal/httpx"
	"github.com/mcpkg/mcpkg/internal/identity"
	"github.com/mcpkg/mcpkg/internal/model"
)

// PyPI finds Python distributions whose name mentions MCP and that depend on an MCP SDK.
type PyPI struct {
	BaseURL string // https://pypi.org
	HTTP    *httpx.Client
	// Names, when set, skips the simple-index scan (used by tests and `mcpkg add`).
	Names []string
}

// Name implements Source.
func (p *PyPI) Name() string { return "pypi" }

var pypiNameRe = regexp.MustCompile(`(?i)(^|[-_.])mcp([-_.]|$)|mcp[-_.]?server|server[-_.]?mcp`)

// LooksLikeMCPName is the cheap pre-filter applied to the ~700k PyPI project names.
func LooksLikeMCPName(name string) bool { return pypiNameRe.MatchString(name) }

// PyPIProject is the subset of GET /pypi/<name>/json we use.
type PyPIProject struct {
	Info struct {
		Name                   string            `json:"name"`
		Version                string            `json:"version"`
		Summary                string            `json:"summary"`
		Description            string            `json:"description"`
		DescriptionContentType string            `json:"description_content_type"`
		HomePage               string            `json:"home_page"`
		ProjectURLs            map[string]string `json:"project_urls"`
		RequiresDist           []string          `json:"requires_dist"`
		License                string            `json:"license"`
		Yanked                 bool              `json:"yanked"`
	} `json:"info"`
	URLs []struct {
		Filename      string    `json:"filename"`
		PackageType   string    `json:"packagetype"` // bdist_wheel | sdist
		URL           string    `json:"url"`
		Size          int64     `json:"size"`
		UploadTimeISO time.Time `json:"upload_time_iso_8601"`
	} `json:"urls"`
}

var reqName = regexp.MustCompile(`^\s*([A-Za-z0-9][A-Za-z0-9._-]*)`)

// MCPDep returns the MCP SDK requirement found in requires_dist, ignoring optional extras.
func (p PyPIProject) MCPDep() string {
	for _, r := range p.Info.RequiresDist {
		if strings.Contains(r, "extra ==") || strings.Contains(r, "extra==") {
			continue
		}
		m := reqName.FindStringSubmatch(r)
		if m == nil {
			continue
		}
		n := identity.PyPIName(m[1])
		for _, dep := range PyPIMCPDeps {
			if n == dep {
				return dep
			}
		}
	}
	return ""
}

// RepoURL picks a code-host URL from project_urls / home_page.
func (p PyPIProject) RepoURL() string {
	keys := make([]string, 0, len(p.Info.ProjectURLs))
	for k := range p.Info.ProjectURLs {
		keys = append(keys, k)
	}
	rank := func(k string) int {
		k = strings.ToLower(k)
		switch {
		case strings.Contains(k, "source"), strings.Contains(k, "repo"), strings.Contains(k, "code"), strings.Contains(k, "github"):
			return 0
		case strings.Contains(k, "home"):
			return 1
		}
		return 2
	}
	sort.SliceStable(keys, func(i, j int) bool { return rank(keys[i]) < rank(keys[j]) })
	for _, k := range keys {
		if _, ok := identity.ParseRepo(p.Info.ProjectURLs[k]); ok {
			return p.Info.ProjectURLs[k]
		}
	}
	if _, ok := identity.ParseRepo(p.Info.HomePage); ok {
		return p.Info.HomePage
	}
	return ""
}

// UploadTime returns the newest file upload time of the current version.
func (p PyPIProject) UploadTime() *time.Time {
	var newest time.Time
	for _, u := range p.URLs {
		if u.UploadTimeISO.After(newest) {
			newest = u.UploadTimeISO
		}
	}
	if newest.IsZero() {
		return nil
	}
	return &newest
}

// FetchPyPI loads project JSON (optionally for a specific version).
func FetchPyPI(ctx context.Context, h *httpx.Client, base, name, version string) (PyPIProject, error) {
	var p PyPIProject
	u := base + "/pypi/" + url.PathEscape(name) + "/json"
	if version != "" {
		u = base + "/pypi/" + url.PathEscape(name) + "/" + url.PathEscape(version) + "/json"
	}
	err := h.GetJSON(ctx, u, nil, &p)
	return p, err
}

// Crawl scans the simple index (incrementally by serial) and verifies candidates via the JSON API.
func (p *PyPI) Crawl(ctx context.Context, opt Options, emit func(Candidate) error) (string, error) {
	sinceSerial, _ := strconv.ParseInt(opt.Since, 10, 64)
	names := p.Names
	maxSerial := sinceSerial
	if len(names) == 0 {
		var err error
		names, maxSerial, err = p.scanIndex(ctx, sinceSerial)
		if err != nil {
			return opt.Since, err
		}
	}
	emitted := 0
	for _, name := range names {
		if ctx.Err() != nil {
			return opt.Since, ctx.Err()
		}
		proj, err := FetchPyPI(ctx, p.HTTP, p.BaseURL, name, "")
		if err != nil {
			continue
		}
		norm := identity.PyPIName(proj.Info.Name)
		if norm == "mcp" || norm == "fastmcp" {
			continue // the SDKs themselves
		}
		dep := proj.MCPDep()
		if dep == "" {
			continue
		}
		c := Candidate{
			Source:      "pypi",
			Name:        proj.Info.Name,
			Description: strings.TrimSpace(proj.Info.Summary),
			Version:     proj.Info.Version,
			RepoURL:     proj.RepoURL(),
			WebsiteURL:  proj.Info.HomePage,
			Packages: []model.Package{{
				RegistryType: "pypi", Identifier: proj.Info.Name, Version: proj.Info.Version,
				Transport: model.Transport{Type: "stdio"},
			}},
			PublishedAt: proj.UploadTime(),
			Raw:         map[string]any{"name": proj.Info.Name, "version": proj.Info.Version, "mcpDependency": dep, "license": trimTo(proj.Info.License, 120)},
		}
		if err := emit(c); err != nil {
			return opt.Since, err
		}
		emitted++
		if opt.Limit > 0 && emitted >= opt.Limit {
			break
		}
	}
	if len(p.Names) > 0 {
		return opt.Since, nil
	}
	return fmt.Sprint(maxSerial), nil
}

// scanIndex streams the PEP 691 JSON simple index and returns MCP-looking names
// whose _last-serial is newer than since.
func (p *PyPI) scanIndex(ctx context.Context, since int64) ([]string, int64, error) {
	data, _, err := p.HTTP.Do(ctx, httpx.Request{
		URL:     p.BaseURL + "/simple/",
		Header:  map[string]string{"Accept": "application/vnd.pypi.simple.v1+json"},
		MaxBody: 512 << 20,
	})
	if err != nil {
		return nil, since, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	maxSerial := since
	var names []string
	// Walk tokens until the "projects" array, then decode elements one by one.
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, since, err
		}
		if key, ok := tok.(string); ok && key == "projects" {
			if _, err := dec.Token(); err != nil { // '['
				return nil, since, err
			}
			for dec.More() {
				var proj struct {
					Name   string `json:"name"`
					Serial int64  `json:"_last-serial"`
				}
				if err := dec.Decode(&proj); err != nil {
					return nil, since, err
				}
				if proj.Serial > maxSerial {
					maxSerial = proj.Serial
				}
				if proj.Serial > since && LooksLikeMCPName(proj.Name) {
					names = append(names, proj.Name)
				}
			}
			break
		}
	}
	return names, maxSerial, nil
}

func trimTo(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}
