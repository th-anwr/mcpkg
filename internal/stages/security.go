package stages

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mcpkg/mcpkg/internal/httpx"
	"github.com/mcpkg/mcpkg/internal/model"
	"github.com/mcpkg/mcpkg/internal/pipeline"
	"github.com/mcpkg/mcpkg/internal/scan"
	"github.com/mcpkg/mcpkg/internal/sources"
)

// Security is hook 2: static analysis of what users actually install
// (the npm tarball / PyPI wheel; git clone as fallback). Nothing is executed.
type Security struct{ D *Deps }

// Name implements pipeline.Stage.
func (s *Security) Name() string { return "security" }

// Run implements pipeline.Stage.
func (s *Security) Run(ctx context.Context, tools []*model.Tool) []pipeline.Result {
	out := make([]pipeline.Result, len(tools))
	for i, t := range tools {
		out[i] = s.one(ctx, t)
	}
	return out
}

type artifact struct {
	kind           string // npm-tarball | pypi-dist | git | none
	target         string
	root           string
	subdir         string
	installScripts []string
	entryPoints    []string
	ecosystem      string // npm | PyPI (for OSV)
	pkg, version   string
}

func (s *Security) one(ctx context.Context, t *model.Tool) pipeline.Result {
	sec := &model.Security{CheckedAt: s.D.now(), Counts: map[string]int{}}
	var notes []string
	if t.RegistryStatus() == "deleted" {
		sec.Blocked, sec.BlockReason = true, "removed by MCP Registry moderation"
		return s.finish(t, sec, notes)
	}
	dir, err := os.MkdirTemp(s.D.Cfg.WorkDir, "mcpkg-sec-*")
	if err != nil {
		return pipeline.Result{Err: err}
	}
	defer os.RemoveAll(dir)

	art, err := s.fetch(ctx, t, dir)
	if err != nil {
		return pipeline.Result{Err: err}
	}
	sec.Scanned, sec.Target = art.kind, art.target
	sec.InstallScripts, sec.EntryPoints = art.installScripts, art.entryPoints
	if art.kind != "none" {
		rep := scan.Dir(art.root, scan.Options{Subdir: art.subdir})
		sec.Files, sec.Findings, sec.Capabilities = rep.Files, rep.Findings, rep.Capabilities
		sec.Scanners = append(sec.Scanners, "builtin")
		for _, sc := range art.installScripts {
			sec.Findings = append(sec.Findings, model.Finding{Rule: "install-script", Severity: "medium",
				Message: "runs code at install time: " + truncate(sc, 120)})
		}
		if n, err := s.osvScanner(ctx, art.root, sec); err == nil && n {
			sec.Scanners = append(sec.Scanners, "osv-scanner")
		}
		if n, err := s.semgrep(ctx, art.root, sec); err == nil && n {
			sec.Scanners = append(sec.Scanners, "semgrep")
		}
	} else {
		notes = append(notes, "no installable artifact or repository to scan")
	}
	if art.pkg != "" && art.ecosystem != "" {
		if err := s.osvQuery(ctx, art.ecosystem, art.pkg, art.version, sec); err != nil {
			notes = append(notes, "osv lookup failed: "+truncate(err.Error(), 120))
		} else {
			sec.Scanners = append(sec.Scanners, "osv-api")
		}
	}
	if repo, ok := githubRepo(t); ok {
		if sc, err := s.scorecard(ctx, repo.Slug()); err == nil {
			sec.Scorecard = &sc
			sec.Scanners = append(sec.Scanners, "openssf-scorecard")
		}
	}
	if len(sec.Vulns.Malicious) > 0 {
		sec.Blocked, sec.BlockReason = true, "known malicious package: "+strings.Join(sec.Vulns.Malicious, ", ")
	}
	return s.finish(t, sec, notes)
}

func (s *Security) finish(t *model.Tool, sec *model.Security, notes []string) pipeline.Result {
	for _, f := range sec.Findings {
		sec.Counts[f.Severity]++
	}
	sec.NeedsReview = sec.Counts["high"] > 0 || sec.Counts["critical"] > 0
	sec.Notes = notes
	fields := map[string]any{"security": sec}
	res := pipeline.Result{Decision: pipeline.Continue, Fields: fields, Notes: notes}
	if sec.Blocked && s.D.Gates.StopOnMalicious {
		fields["status"] = "blocked"
		fields["visibility"] = "hidden"
		res.Decision = pipeline.Stop
		res.Notes = append(res.Notes, "blocked: "+sec.BlockReason)
		res.Review = "blocked"
		return res
	}
	if sec.NeedsReview && s.D.Gates.ReviewOnHighFindings {
		res.Review = "high-severity-findings"
		res.Notes = append(res.Notes, fmt.Sprintf("%d high-severity findings sent to review", sec.Counts["high"]))
	}
	return res
}

// fetch downloads and extracts the artifact to scan.
func (s *Security) fetch(ctx context.Context, t *model.Tool, dir string) (artifact, error) {
	if p, ok := firstPackage(t, "npm"); ok {
		doc, err := sources.FetchNpmVersion(ctx, s.D.HTTP, s.D.Cfg.NpmRegistry, p.Identifier, p.Version)
		if err == nil && doc.Dist.Tarball != "" {
			data, _, err := s.D.HTTP.Do(ctx, httpx.Request{URL: doc.Dist.Tarball, MaxBody: 200 << 20})
			if err != nil && !httpx.IsNotFound(err) {
				return artifact{}, err
			}
			if err == nil {
				root := filepath.Join(dir, "npm")
				if err := scan.ExtractTarGz(bytes.NewReader(data), root, scan.DefaultLimits); err != nil {
					return artifact{}, fmt.Errorf("extract npm tarball: %w", err)
				}
				var scripts []string
				for _, k := range []string{"preinstall", "install", "postinstall"} {
					if v := strings.TrimSpace(doc.Scripts[k]); v != "" {
						scripts = append(scripts, k+": "+v)
					}
				}
				return artifact{kind: "npm-tarball", target: p.Identifier + "@" + doc.Version, root: root,
					installScripts: scripts, ecosystem: "npm", pkg: p.Identifier, version: doc.Version}, nil
			}
		} else if err != nil && !httpx.IsNotFound(err) {
			return artifact{}, err
		}
	}
	if p, ok := firstPackage(t, "pypi"); ok {
		proj, err := sources.FetchPyPI(ctx, s.D.HTTP, s.D.Cfg.PyPIURL, p.Identifier, p.Version)
		if err != nil && !httpx.IsNotFound(err) {
			return artifact{}, err
		}
		if err == nil {
			if a, ok, err := s.fetchPyPI(ctx, proj, dir); err != nil {
				return artifact{}, err
			} else if ok {
				return a, nil
			}
		}
	}
	if t.RepoURL != "" {
		root := filepath.Join(dir, "repo")
		cctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(cctx, "git", "clone", "--depth", "1", "--single-branch", "--no-tags",
			"--filter=blob:limit=2m", t.RepoURL, root)
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_LFS_SKIP_SMUDGE=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			msg := string(out)
			if strings.Contains(msg, "not found") || strings.Contains(msg, "Repository not found") || strings.Contains(msg, "could not read Username") {
				return artifact{kind: "none"}, nil
			}
			return artifact{}, fmt.Errorf("git clone: %v: %s", err, tailString(msg, 300))
		}
		return artifact{kind: "git", target: t.RepoURL, root: root, subdir: t.RepoSubfolder}, nil
	}
	return artifact{kind: "none"}, nil
}

func (s *Security) fetchPyPI(ctx context.Context, proj sources.PyPIProject, dir string) (artifact, bool, error) {
	files := proj.URLs
	sort.SliceStable(files, func(i, j int) bool {
		rank := func(f string, kind string) int {
			switch {
			case kind == "bdist_wheel" && strings.Contains(f, "py3-none-any"):
				return 0
			case kind == "bdist_wheel":
				return 1
			case kind == "sdist":
				return 2
			}
			return 3
		}
		return rank(files[i].Filename, files[i].PackageType) < rank(files[j].Filename, files[j].PackageType)
	})
	for _, f := range files {
		if f.Size > 150<<20 {
			continue
		}
		data, _, err := s.D.HTTP.Do(ctx, httpx.Request{URL: f.URL, MaxBody: 160 << 20})
		if err != nil {
			if httpx.IsNotFound(err) {
				continue
			}
			return artifact{}, false, err
		}
		root := filepath.Join(dir, "pypi")
		switch {
		case strings.HasSuffix(f.Filename, ".whl") || strings.HasSuffix(f.Filename, ".zip"):
			tmp := filepath.Join(dir, f.Filename)
			if err := os.WriteFile(tmp, data, 0o644); err != nil {
				return artifact{}, false, err
			}
			if err := scan.ExtractZip(tmp, root, scan.DefaultLimits); err != nil {
				return artifact{}, false, fmt.Errorf("extract wheel: %w", err)
			}
		case strings.HasSuffix(f.Filename, ".tar.gz") || strings.HasSuffix(f.Filename, ".tgz"):
			if err := scan.ExtractTarGz(bytes.NewReader(data), root, scan.DefaultLimits); err != nil {
				return artifact{}, false, fmt.Errorf("extract sdist: %w", err)
			}
		default:
			continue
		}
		return artifact{kind: "pypi-dist", target: f.Filename, root: root, entryPoints: PythonEntryPoints(root),
			ecosystem: "PyPI", pkg: proj.Info.Name, version: proj.Info.Version}, true, nil
	}
	return artifact{}, false, nil
}

var tomlScript = regexp.MustCompile(`^\s*["']?([A-Za-z0-9_.-]+)["']?\s*=\s*["']([^"']+)["']`)

// PythonEntryPoints finds console scripts in a wheel (entry_points.txt) or sdist (pyproject.toml/setup.cfg).
func PythonEntryPoints(root string) []string {
	var eps []string
	seen := map[string]bool{}
	addEP := func(n string) {
		if n != "" && !seen[n] {
			seen[n] = true
			eps = append(eps, n)
		}
	}
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		name := info.Name()
		var section string
		switch {
		case name == "entry_points.txt" && strings.HasSuffix(filepath.Dir(p), ".dist-info"):
			section = "[console_scripts]"
		case name == "pyproject.toml":
			section = "[project.scripts]"
		case name == "setup.cfg":
			section = "console_scripts"
		default:
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return nil
		}
		defer f.Close()
		in := false
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if strings.HasPrefix(line, "[") {
				in = line == section
				continue
			}
			if name == "setup.cfg" && strings.HasPrefix(line, "console_scripts") {
				in = true
				continue
			}
			if !in || line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if m := tomlScript.FindStringSubmatch(line); m != nil {
				addEP(m[1])
			} else if k, _, ok := strings.Cut(line, "="); ok {
				addEP(strings.TrimSpace(k))
			}
		}
		return nil
	})
	sort.Strings(eps)
	return eps
}

// osvScanner runs `osv-scanner` if installed (lockfile vulnerabilities, malicious packages).
func (s *Security) osvScanner(ctx context.Context, root string, sec *model.Security) (bool, error) {
	bin, err := exec.LookPath("osv-scanner")
	if err != nil {
		return false, err
	}
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	out, _ := exec.CommandContext(cctx, bin, "scan", "source", "--format", "json", "--recursive", root).Output()
	if len(bytes.TrimSpace(out)) == 0 {
		out, _ = exec.CommandContext(cctx, bin, "--format", "json", "-r", root).Output() // v1 syntax
	}
	var res struct {
		Results []struct {
			Packages []struct {
				Vulnerabilities []struct {
					ID               string `json:"id"`
					DatabaseSpecific struct {
						Severity string `json:"severity"`
					} `json:"database_specific"`
				} `json:"vulnerabilities"`
				Groups []struct {
					IDs         []string `json:"ids"`
					MaxSeverity string   `json:"max_severity"`
				} `json:"groups"`
			} `json:"packages"`
		} `json:"results"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return false, err
	}
	for _, r := range res.Results {
		for _, p := range r.Packages {
			counted := map[string]bool{}
			for _, g := range p.Groups {
				for _, id := range g.IDs {
					counted[id] = true
				}
				bucketCVSS(g.MaxSeverity, &sec.Vulns)
			}
			for _, v := range p.Vulnerabilities {
				if strings.HasPrefix(v.ID, "MAL-") {
					sec.Vulns.Malicious = append(sec.Vulns.Malicious, v.ID)
				}
				if !counted[v.ID] {
					bucketLabel(v.DatabaseSpecific.Severity, &sec.Vulns)
				}
			}
		}
	}
	return true, nil
}

// semgrep runs semgrep with our MCP rules if installed.
func (s *Security) semgrep(ctx context.Context, root string, sec *model.Security) (bool, error) {
	bin, err := exec.LookPath("semgrep")
	if err != nil {
		return false, err
	}
	rules := os.Getenv("SEMGREP_RULES")
	if rules == "" {
		rules = "scanners/semgrep/mcp-rules.yml"
	}
	if _, err := os.Stat(rules); err != nil {
		return false, err
	}
	cctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	out, _ := exec.CommandContext(cctx, bin, "scan", "--config", rules, "--json", "--quiet", "--metrics=off", "--timeout", "30", root).Output()
	var res struct {
		Results []struct {
			CheckID string `json:"check_id"`
			Path    string `json:"path"`
			Start   struct {
				Line int `json:"line"`
			} `json:"start"`
			Extra struct {
				Severity string `json:"severity"`
				Message  string `json:"message"`
			} `json:"extra"`
		} `json:"results"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return false, err
	}
	for i, r := range res.Results {
		if i >= 30 {
			break
		}
		sev := map[string]string{"ERROR": "high", "WARNING": "medium", "INFO": "low"}[r.Extra.Severity]
		if sev == "" {
			sev = "low"
		}
		rel, _ := filepath.Rel(root, r.Path)
		sec.Findings = append(sec.Findings, model.Finding{Rule: "semgrep:" + lastSegment(r.CheckID), Severity: sev,
			File: rel, Line: r.Start.Line, Message: truncate(r.Extra.Message, 200)})
	}
	return true, nil
}

func lastSegment(s string) string {
	if i := strings.LastIndex(s, "."); i >= 0 {
		return s[i+1:]
	}
	return s
}

// osvQuery asks OSV about the exact package version (catches malicious packages: MAL-*).
func (s *Security) osvQuery(ctx context.Context, ecosystem, pkg, version string, sec *model.Security) error {
	var res struct {
		Vulns []struct {
			ID               string `json:"id"`
			DatabaseSpecific struct {
				Severity string `json:"severity"`
			} `json:"database_specific"`
		} `json:"vulns"`
	}
	body := map[string]any{"package": map[string]string{"name": pkg, "ecosystem": ecosystem}}
	if version != "" {
		body["version"] = version
	}
	if err := s.D.HTTP.PostJSON(ctx, s.D.Cfg.OSVAPI+"/v1/query", nil, body, &res); err != nil {
		return err
	}
	for _, v := range res.Vulns {
		if strings.HasPrefix(v.ID, "MAL-") {
			if !contains(sec.Vulns.Malicious, v.ID) {
				sec.Vulns.Malicious = append(sec.Vulns.Malicious, v.ID)
			}
			continue
		}
		bucketLabel(v.DatabaseSpecific.Severity, &sec.Vulns)
	}
	return nil
}

func (s *Security) scorecard(ctx context.Context, slug string) (float64, error) {
	var res struct {
		Score float64 `json:"score"`
	}
	err := s.D.HTTP.GetJSON(ctx, s.D.Cfg.ScorecardAPI+"/projects/github.com/"+slug, nil, &res)
	return res.Score, err
}

func bucketCVSS(v string, c *model.VulnCounts) {
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil {
		bucketLabel(v, c)
		return
	}
	switch {
	case f >= 9:
		c.Critical++
	case f >= 7:
		c.High++
	case f >= 4:
		c.Medium++
	default:
		c.Low++
	}
}

func bucketLabel(v string, c *model.VulnCounts) {
	switch strings.ToUpper(strings.TrimSpace(v)) {
	case "CRITICAL":
		c.Critical++
	case "HIGH":
		c.High++
	case "MODERATE", "MEDIUM":
		c.Medium++
	case "LOW":
		c.Low++
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
