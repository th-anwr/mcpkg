package stages

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mcpkg/mcpkg/internal/httpx"
	"github.com/mcpkg/mcpkg/internal/identity"
	"github.com/mcpkg/mcpkg/internal/model"
	"github.com/mcpkg/mcpkg/internal/pipeline"
	"github.com/mcpkg/mcpkg/internal/queue"
	"github.com/mcpkg/mcpkg/internal/sources"
)

// Health is hook 1: repo/package health from public metadata. Nothing is executed.
// With GITHUB_TOKEN it batches up to 25+ repos per GraphQL call.
type Health struct{ D *Deps }

// Name implements pipeline.Stage.
func (h *Health) Name() string { return "health" }

// Run implements pipeline.Stage.
func (h *Health) Run(ctx context.Context, tools []*model.Tool) []pipeline.Result {
	results := make([]pipeline.Result, len(tools))
	repos := map[string]*ghRepo{}
	var ghErr error
	var reqs []GHRequest
	seen := map[string]bool{}
	for _, t := range tools {
		if r, ok := githubRepo(t); ok {
			req := GHRequest{Slug: r.Slug(), Subfolder: r.Subfolder}
			if !seen[req.key()] {
				seen[req.key()] = true
				reqs = append(reqs, req)
			}
		}
	}
	if len(reqs) > 0 {
		if h.D.Cfg.GitHubToken != "" {
			repos, ghErr = h.graphql(ctx, reqs)
		} else {
			repos, ghErr = h.rest(ctx, reqs)
		}
		if _, limited := queue.AsRateLimited(ghErr); limited {
			for i := range results {
				results[i] = pipeline.Result{Err: ghErr}
			}
			return results
		}
	}
	for i, t := range tools {
		results[i] = h.one(ctx, t, repos, ghErr)
	}
	return results
}

// GHRequest is one repository (plus monorepo folder) to look up.
type GHRequest struct {
	Slug      string
	Subfolder string
}

func (r GHRequest) key() string { return strings.ToLower(r.Slug) + "#" + r.Subfolder }

func (h *Health) one(ctx context.Context, t *model.Tool, repos map[string]*ghRepo, ghErr error) pipeline.Result {
	now := h.D.now()
	hl := &model.Health{Provider: "none", CheckedAt: now}
	var notes []string
	fields := map[string]any{}
	repo, isGH := githubRepo(t)
	var gh *ghRepo
	if isGH {
		hl.Provider = "github"
		hl.Repo = repo.Slug()
		if r, ok := repos[GHRequest{Slug: repo.Slug(), Subfolder: repo.Subfolder}.key()]; ok && r != nil {
			r.fill(hl)
			gh = r
		} else if ghErr != nil {
			return pipeline.Result{Err: fmt.Errorf("github: %w", ghErr)}
		} else {
			notes = append(notes, "repository not found on GitHub")
		}
	} else if t.RepoURL != "" {
		notes = append(notes, "non-GitHub repository: limited health data")
		hl.Partial = true
	}
	downloadsKnown := false
	if p, ok := firstPackage(t, "npm"); ok {
		if n, err := h.npmDownloads(ctx, p.Identifier); err == nil {
			hl.WeeklyDownloads, downloadsKnown = n, true
		}
	}
	// README: refetch when missing, when the head commit moved, or monthly.
	prevCommit := ""
	if t.Health != nil {
		prevCommit = t.Health.HeadCommit
	}
	if t.Readme == "" || (hl.HeadCommit != "" && hl.HeadCommit != prevCommit) || t.Health == nil || now.Sub(t.Health.CheckedAt) > 30*24*time.Hour {
		md, src := "", ""
		if gh != nil && gh.Readme != "" { // fetched in the same GraphQL batch: no extra requests
			md, src = gh.Readme, fmt.Sprintf("https://github.com/%s/%s/blob/HEAD/%s", repo.Owner, repo.Name, gh.ReadmePath)
		} else {
			md, src = h.fetchReadme(ctx, t, repo, isGH)
		}
		if md != "" {
			fields["readme"] = truncate(md, 256<<10)
			fields["readme_url"] = src
		}
	}
	fields["health"] = hl
	if hl.RepoFound {
		fields["stars"] = hl.Stars
	}
	if downloadsKnown {
		fields["weekly_downloads"] = hl.WeeklyDownloads
	}
	if hl.LastCommitAt != nil {
		fields["last_commit_at"] = *hl.LastCommitAt
	}

	g := h.D.Gates
	switch {
	case isGH && !hl.RepoFound && len(t.Packages) == 0 && len(t.Remotes) == 0:
		fields["status"] = "missing"
		fields["visibility"] = "hidden"
		return pipeline.Result{Decision: pipeline.Stop, Fields: fields, Notes: append(notes, "nothing to install: repo missing and no packages")}
	case (hl.Archived || hl.Disabled) && g.SkipHeavyIfArchived:
		fields["status"] = "unmaintained"
		return pipeline.Result{Decision: pipeline.SkipHeavy, Fields: fields, Notes: append(notes, "repository archived: skipping security/smoke")}
	case g.SkipHeavyIfNoCommitDays > 0 && hl.LastCommitAt != nil && now.Sub(*hl.LastCommitAt) > time.Duration(g.SkipHeavyIfNoCommitDays)*24*time.Hour:
		fields["status"] = "unmaintained"
		return pipeline.Result{Decision: pipeline.SkipHeavy, Fields: fields, Notes: append(notes, "no commits in a long time: skipping heavy checks")}
	}
	if t.Status == "new" || t.Status == "missing" || t.Status == "unmaintained" {
		fields["status"] = "active"
	}
	// Re-checks: skip expensive stages when nothing changed recently.
	if g.SkipUnchangedWithinDays > 0 && t.Smoke != nil && t.Smoke.Status != "" && t.Security != nil &&
		t.Smoke.Version == t.Version && (hl.HeadCommit == "" || hl.HeadCommit == prevCommit) &&
		now.Sub(t.Smoke.CheckedAt) < time.Duration(g.SkipUnchangedWithinDays)*24*time.Hour {
		return pipeline.Result{Decision: pipeline.SkipHeavy, Fields: fields, Notes: append(notes, "unchanged since last full check")}
	}
	return pipeline.Result{Decision: pipeline.Continue, Fields: fields, Notes: notes}
}

// ghRepo is the normalized GitHub data we need.
type ghRepo struct {
	NameWithOwner string
	Archived      bool
	Disabled      bool
	Fork          bool
	Stars         int
	Forks         int
	CreatedAt     time.Time
	PushedAt      time.Time
	License       string
	OwnerType     string
	OwnerVerified bool
	HeadOID       string
	HeadDate      time.Time
	Recent        int
	ReleaseTag    string
	ReleaseAt     time.Time
	OpenIssues    int
	ClosedIssues  int
	OpenPRs       int
	MergedPRs     int
	ClosedPRs     int
	OldestOpenPR  time.Time
	Contributors  int
	Topics        []string
	Partial       bool
	Readme        string
	ReadmePath    string
}

func (r *ghRepo) fill(h *model.Health) {
	h.RepoFound = true
	h.Archived, h.Disabled, h.Fork = r.Archived, r.Disabled, r.Fork
	h.Stars, h.Forks = r.Stars, r.Forks
	h.License = r.License
	h.OwnerType, h.OwnerVerified = r.OwnerType, r.OwnerVerified
	h.HeadCommit = r.HeadOID
	h.LastCommitAt = ptrTime(r.HeadDate)
	if h.LastCommitAt == nil {
		h.LastCommitAt = ptrTime(r.PushedAt)
	}
	h.CreatedAt = ptrTime(r.CreatedAt)
	h.RecentCommits90d = r.Recent
	h.LastReleaseTag = r.ReleaseTag
	h.LastReleaseAt = ptrTime(r.ReleaseAt)
	h.OpenIssues, h.ClosedIssues = r.OpenIssues, r.ClosedIssues
	h.OpenPRs, h.MergedPRs, h.ClosedPRs = r.OpenPRs, r.MergedPRs, r.ClosedPRs
	h.OldestOpenPRAt = ptrTime(r.OldestOpenPR)
	h.Contributors = r.Contributors
	h.Topics = r.Topics
	h.Partial = r.Partial
}

const ghFragment = `fragment F on Repository {
  nameWithOwner isArchived isDisabled isFork stargazerCount forkCount createdAt pushedAt
  licenseInfo { spdxId }
  owner { __typename login ... on Organization { isVerified } }
  defaultBranchRef { target { ... on Commit { oid committedDate recent: history(since: "%s") { totalCount } } } }
  latestRelease { tagName publishedAt }
  openIssues: issues(states: OPEN) { totalCount }
  closedIssues: issues(states: CLOSED) { totalCount }
  openPRs: pullRequests(states: OPEN) { totalCount }
  mergedPRs: pullRequests(states: MERGED) { totalCount }
  closedPRs: pullRequests(states: CLOSED) { totalCount }
  oldestPR: pullRequests(states: OPEN, first: 1, orderBy: {field: CREATED_AT, direction: ASC}) { nodes { createdAt } }
  mentionableUsers { totalCount }
  repositoryTopics(first: 10) { nodes { topic { name } } }
}`

type gqlRepo struct {
	NameWithOwner  string    `json:"nameWithOwner"`
	IsArchived     bool      `json:"isArchived"`
	IsDisabled     bool      `json:"isDisabled"`
	IsFork         bool      `json:"isFork"`
	StargazerCount int       `json:"stargazerCount"`
	ForkCount      int       `json:"forkCount"`
	CreatedAt      time.Time `json:"createdAt"`
	PushedAt       time.Time `json:"pushedAt"`
	LicenseInfo    *struct {
		SpdxID string `json:"spdxId"`
	} `json:"licenseInfo"`
	Owner struct {
		Typename   string `json:"__typename"`
		IsVerified bool   `json:"isVerified"`
	} `json:"owner"`
	DefaultBranchRef *struct {
		Target struct {
			OID           string    `json:"oid"`
			CommittedDate time.Time `json:"committedDate"`
			Recent        struct {
				TotalCount int `json:"totalCount"`
			} `json:"recent"`
		} `json:"target"`
	} `json:"defaultBranchRef"`
	LatestRelease *struct {
		TagName     string    `json:"tagName"`
		PublishedAt time.Time `json:"publishedAt"`
	} `json:"latestRelease"`
	OpenIssues   struct{ TotalCount int } `json:"openIssues"`
	ClosedIssues struct{ TotalCount int } `json:"closedIssues"`
	OpenPRs      struct{ TotalCount int } `json:"openPRs"`
	MergedPRs    struct{ TotalCount int } `json:"mergedPRs"`
	ClosedPRs    struct{ TotalCount int } `json:"closedPRs"`
	OldestPR     struct {
		Nodes []struct {
			CreatedAt time.Time `json:"createdAt"`
		} `json:"nodes"`
	} `json:"oldestPR"`
	MentionableUsers struct{ TotalCount int } `json:"mentionableUsers"`
	RepositoryTopics struct {
		Nodes []struct {
			Topic struct {
				Name string `json:"name"`
			} `json:"topic"`
		} `json:"nodes"`
	} `json:"repositoryTopics"`
	// README blobs requested per repo: subfolder first, then root.
	ReadmeSub      *gqlBlob `json:"readmeSub"`
	ReadmeSubLower *gqlBlob `json:"readmeSubLower"`
	ReadmeRoot     *gqlBlob `json:"readmeRoot"`
	ReadmeLower    *gqlBlob `json:"readmeLower"`
}

type gqlBlob struct {
	Text *string `json:"text"`
}

// BuildGraphQLQuery batches repos (and their README blobs) into one request. Exported for tests.
func BuildGraphQLQuery(reqs []GHRequest, since time.Time) string {
	var b strings.Builder
	b.WriteString("query {\n  rateLimit { remaining resetAt }\n")
	blob := func(alias, path string) string {
		return fmt.Sprintf(" %s: object(expression: %s) { ... on Blob { text } }", alias, strconv.Quote("HEAD:"+path))
	}
	for i, r := range reqs {
		owner, name, _ := strings.Cut(r.Slug, "/")
		fmt.Fprintf(&b, "  r%d: repository(owner: %s, name: %s) { ...F", i, strconv.Quote(owner), strconv.Quote(name))
		if r.Subfolder != "" {
			b.WriteString(blob("readmeSub", r.Subfolder+"/README.md"))
			b.WriteString(blob("readmeSubLower", r.Subfolder+"/readme.md"))
		}
		b.WriteString(blob("readmeRoot", "README.md"))
		b.WriteString(blob("readmeLower", "readme.md"))
		b.WriteString(" }\n")
	}
	b.WriteString("}\n")
	fmt.Fprintf(&b, ghFragment, since.UTC().Format(time.RFC3339))
	return b.String()
}

func (h *Health) graphql(ctx context.Context, reqs []GHRequest) (map[string]*ghRepo, error) {
	query := BuildGraphQLQuery(reqs, h.D.now().AddDate(0, 0, -90))
	var resp struct {
		Data   map[string]json.RawMessage `json:"data"`
		Errors []struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	err := h.D.HTTP.PostJSON(ctx, h.D.Cfg.GitHubAPI+"/graphql", map[string]string{
		"Authorization": "Bearer " + h.D.Cfg.GitHubToken,
	}, map[string]string{"query": query}, &resp)
	if err != nil {
		return nil, err
	}
	for _, e := range resp.Errors {
		if e.Type == "RATE_LIMITED" {
			return nil, &queue.ErrRateLimited{Until: time.Now().Add(15 * time.Minute)}
		}
	}
	if resp.Data == nil && len(resp.Errors) > 0 {
		return nil, fmt.Errorf("graphql: %s", resp.Errors[0].Message)
	}
	out := map[string]*ghRepo{}
	for i, req := range reqs {
		raw, ok := resp.Data[fmt.Sprintf("r%d", i)]
		if !ok || string(raw) == "null" {
			continue // NOT_FOUND
		}
		var g gqlRepo
		if err := json.Unmarshal(raw, &g); err != nil {
			continue
		}
		r := &ghRepo{
			NameWithOwner: g.NameWithOwner, Archived: g.IsArchived, Disabled: g.IsDisabled, Fork: g.IsFork,
			Stars: g.StargazerCount, Forks: g.ForkCount, CreatedAt: g.CreatedAt, PushedAt: g.PushedAt,
			OwnerType: g.Owner.Typename, OwnerVerified: g.Owner.IsVerified,
			OpenIssues: g.OpenIssues.TotalCount, ClosedIssues: g.ClosedIssues.TotalCount,
			OpenPRs: g.OpenPRs.TotalCount, MergedPRs: g.MergedPRs.TotalCount, ClosedPRs: g.ClosedPRs.TotalCount,
			Contributors: g.MentionableUsers.TotalCount,
		}
		if g.LicenseInfo != nil {
			r.License = g.LicenseInfo.SpdxID
		}
		if g.DefaultBranchRef != nil {
			r.HeadOID = g.DefaultBranchRef.Target.OID
			r.HeadDate = g.DefaultBranchRef.Target.CommittedDate
			r.Recent = g.DefaultBranchRef.Target.Recent.TotalCount
		}
		if g.LatestRelease != nil {
			r.ReleaseTag, r.ReleaseAt = g.LatestRelease.TagName, g.LatestRelease.PublishedAt
		}
		if len(g.OldestPR.Nodes) > 0 {
			r.OldestOpenPR = g.OldestPR.Nodes[0].CreatedAt
		}
		for _, n := range g.RepositoryTopics.Nodes {
			r.Topics = append(r.Topics, n.Topic.Name)
		}
		for _, cand := range []struct {
			b    *gqlBlob
			path string
		}{{g.ReadmeSub, req.Subfolder + "/README.md"}, {g.ReadmeSubLower, req.Subfolder + "/readme.md"}, {g.ReadmeRoot, "README.md"}, {g.ReadmeLower, "readme.md"}} {
			if cand.b != nil && cand.b.Text != nil && strings.TrimSpace(*cand.b.Text) != "" {
				r.Readme, r.ReadmePath = *cand.b.Text, cand.path
				break
			}
		}
		out[req.key()] = r
	}
	return out, nil
}

// rest is the token-less fallback (60 requests/hour!). Fine for development only.
func (h *Health) rest(ctx context.Context, reqs []GHRequest) (map[string]*ghRepo, error) {
	out := map[string]*ghRepo{}
	hdr := map[string]string{"Accept": "application/vnd.github+json"}
	for _, req := range reqs {
		s := req.Slug
		var repo struct {
			Archived bool      `json:"archived"`
			Disabled bool      `json:"disabled"`
			Fork     bool      `json:"fork"`
			Stars    int       `json:"stargazers_count"`
			Forks    int       `json:"forks_count"`
			Created  time.Time `json:"created_at"`
			Pushed   time.Time `json:"pushed_at"`
			License  *struct {
				SpdxID string `json:"spdx_id"`
			} `json:"license"`
			Owner struct {
				Type string `json:"type"`
			} `json:"owner"`
			OpenIssues int      `json:"open_issues_count"`
			Topics     []string `json:"topics"`
		}
		err := h.D.HTTP.GetJSON(ctx, h.D.Cfg.GitHubAPI+"/repos/"+s, hdr, &repo)
		if httpx.IsNotFound(err) {
			continue
		}
		if err != nil {
			return out, err
		}
		r := &ghRepo{Archived: repo.Archived, Disabled: repo.Disabled, Fork: repo.Fork, Stars: repo.Stars, Forks: repo.Forks,
			CreatedAt: repo.Created, PushedAt: repo.Pushed, OwnerType: repo.Owner.Type, OpenIssues: repo.OpenIssues,
			Topics: repo.Topics, Partial: true}
		if repo.License != nil && repo.License.SpdxID != "NOASSERTION" {
			r.License = repo.License.SpdxID
		}
		var commits []struct {
			SHA    string `json:"sha"`
			Commit struct {
				Committer struct {
					Date time.Time `json:"date"`
				} `json:"committer"`
			} `json:"commit"`
		}
		if err := h.D.HTTP.GetJSON(ctx, h.D.Cfg.GitHubAPI+"/repos/"+s+"/commits?per_page=1", hdr, &commits); err == nil && len(commits) > 0 {
			r.HeadOID, r.HeadDate = commits[0].SHA, commits[0].Commit.Committer.Date
		}
		var rel struct {
			Tag  string    `json:"tag_name"`
			Date time.Time `json:"published_at"`
		}
		if err := h.D.HTTP.GetJSON(ctx, h.D.Cfg.GitHubAPI+"/repos/"+s+"/releases/latest", hdr, &rel); err == nil {
			r.ReleaseTag, r.ReleaseAt = rel.Tag, rel.Date
		}
		out[req.key()] = r
	}
	return out, nil
}

func (h *Health) npmDownloads(ctx context.Context, pkg string) (int64, error) {
	var res struct {
		Downloads int64 `json:"downloads"`
	}
	err := h.D.HTTP.GetJSON(ctx, h.D.Cfg.NpmAPI+"/downloads/point/last-week/"+pkg, nil, &res)
	return res.Downloads, err
}

var readmeNames = []string{"README.md", "readme.md", "Readme.md", "README.rst"}

// fetchReadme tries the repo (monorepo subfolder first), then PyPI, then npm.
func (h *Health) fetchReadme(ctx context.Context, t *model.Tool, repo identity.Repo, isGH bool) (string, string) {
	if isGH {
		var dirs []string
		if repo.Subfolder != "" {
			dirs = append(dirs, repo.Subfolder+"/")
		}
		dirs = append(dirs, "")
		for _, dir := range dirs {
			for _, n := range readmeNames {
				u := fmt.Sprintf("%s/%s/%s/HEAD/%s%s", h.D.Cfg.RawGitHub, repo.Owner, repo.Name, dir, n)
				data, _, err := h.D.HTTP.Do(ctx, httpx.Request{URL: u, MaxBody: 1 << 20})
				if err == nil && len(strings.TrimSpace(string(data))) > 0 {
					page := fmt.Sprintf("https://github.com/%s/%s/blob/HEAD/%s%s", repo.Owner, repo.Name, dir, n)
					return string(data), page
				}
				if err != nil && !httpx.IsNotFound(err) {
					break
				}
			}
		}
	}
	if p, ok := firstPackage(t, "pypi"); ok {
		if proj, err := sources.FetchPyPI(ctx, h.D.HTTP, h.D.Cfg.PyPIURL, p.Identifier, ""); err == nil && strings.TrimSpace(proj.Info.Description) != "" {
			return proj.Info.Description, h.D.Cfg.PyPIURL + "/project/" + url.PathEscape(p.Identifier) + "/"
		}
	}
	if p, ok := firstPackage(t, "npm"); ok {
		ver := p.Version
		if ver == "" {
			ver = "latest"
		}
		u := fmt.Sprintf("%s/npm/%s@%s/README.md", h.D.Cfg.JsDelivr, p.Identifier, ver)
		if data, _, err := h.D.HTTP.Do(ctx, httpx.Request{URL: u, MaxBody: 1 << 20}); err == nil && len(data) > 0 {
			return string(data), "https://www.npmjs.com/package/" + p.Identifier
		}
	}
	return "", ""
}
