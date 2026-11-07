package sources

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"github.com/mcpkg/mcpkg/internal/httpx"
	"github.com/mcpkg/mcpkg/internal/queue"
)

// GitHub searches repositories by MCP topics. Requires GITHUB_TOKEN.
// The search API returns at most 1000 results per query, so we slice by
// creation date and split any slice that is still too large.
type GitHub struct {
	API    string
	Token  string
	HTTP   *httpx.Client
	Topics []string
	Start  time.Time
}

// Name implements Source.
func (g *GitHub) Name() string { return "github" }

// DefaultGitHubTopics are the topics searched.
var DefaultGitHubTopics = []string{"mcp-server", "model-context-protocol", "mcp-servers", "mcp"}

type ghSearch struct {
	TotalCount int `json:"total_count"`
	Items      []struct {
		FullName    string    `json:"full_name"`
		HTMLURL     string    `json:"html_url"`
		Description string    `json:"description"`
		Homepage    string    `json:"homepage"`
		Stars       int       `json:"stargazers_count"`
		Archived    bool      `json:"archived"`
		Fork        bool      `json:"fork"`
		PushedAt    time.Time `json:"pushed_at"`
		Topics      []string  `json:"topics"`
		Language    string    `json:"language"`
	} `json:"items"`
}

// Crawl walks each topic in date windows.
func (g *GitHub) Crawl(ctx context.Context, opt Options, emit func(Candidate) error) (string, error) {
	if g.Token == "" {
		return "", fmt.Errorf("github source needs GITHUB_TOKEN")
	}
	topics := g.Topics
	if len(topics) == 0 {
		topics = DefaultGitHubTopics
	}
	start := g.Start
	if start.IsZero() {
		start = time.Date(2024, 11, 1, 0, 0, 0, 0, time.UTC) // MCP was announced Nov 2024
	}
	if opt.Since != "" {
		if t, err := time.Parse(time.RFC3339, opt.Since); err == nil {
			start = t.AddDate(0, 0, -2)
		}
	}
	end := time.Now().UTC()
	seen := map[string]bool{}
	emitted := 0
	for _, topic := range topics {
		windows := [][2]time.Time{{start, end}}
		for len(windows) > 0 {
			w := windows[0]
			windows = windows[1:]
			q := fmt.Sprintf("topic:%s pushed:%s..%s fork:false", topic, w[0].Format("2006-01-02"), w[1].Format("2006-01-02"))
			first, err := g.search(ctx, q, 1)
			if err != nil {
				return "", err
			}
			if first.TotalCount > 1000 && w[1].Sub(w[0]) > 48*time.Hour {
				mid := w[0].Add(w[1].Sub(w[0]) / 2)
				windows = append(windows, [2]time.Time{w[0], mid}, [2]time.Time{mid.AddDate(0, 0, 1), w[1]})
				continue
			}
			for page := 1; page <= 10; page++ {
				res := first
				if page > 1 {
					if res, err = g.search(ctx, q, page); err != nil {
						return "", err
					}
				}
				for _, it := range res.Items {
					if seen[it.FullName] || it.Archived {
						continue
					}
					seen[it.FullName] = true
					pushed := it.PushedAt
					c := Candidate{
						Source:      "github",
						Name:        it.FullName,
						Description: it.Description,
						RepoURL:     it.HTMLURL,
						WebsiteURL:  it.Homepage,
						Stars:       it.Stars,
						UpdatedAt:   &pushed,
						Raw:         map[string]any{"fullName": it.FullName, "stars": it.Stars, "topics": it.Topics, "language": it.Language},
					}
					if err := emit(c); err != nil {
						return "", err
					}
					emitted++
					if opt.Limit > 0 && emitted >= opt.Limit {
						return end.Format(time.RFC3339), nil
					}
				}
				if len(res.Items) < 100 {
					break
				}
			}
		}
	}
	return end.Format(time.RFC3339), nil
}

// search calls the search API politely: it is limited to 30 requests/minute, so
// we space calls ~2.1s apart and wait out rate limits instead of aborting the crawl.
func (g *GitHub) search(ctx context.Context, q string, page int) (ghSearch, error) {
	u := fmt.Sprintf("%s/search/repositories?q=%s&per_page=100&page=%d&sort=stars", g.API, url.QueryEscape(q), page)
	hdr := map[string]string{
		"Authorization":        "Bearer " + g.Token,
		"Accept":               "application/vnd.github+json",
		"X-GitHub-Api-Version": "2022-11-28",
	}
	for attempt := 0; ; attempt++ {
		if err := sleepCtx(ctx, 2100*time.Millisecond); err != nil {
			return ghSearch{}, err
		}
		var res ghSearch
		err := g.HTTP.GetJSON(ctx, u, hdr, &res)
		rl, limited := queue.AsRateLimited(err)
		if !limited || attempt >= 5 {
			return res, err
		}
		wait := time.Until(rl.Until) + 5*time.Second
		if wait > 15*time.Minute {
			wait = 15 * time.Minute
		}
		if err := sleepCtx(ctx, wait); err != nil {
			return ghSearch{}, err
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
