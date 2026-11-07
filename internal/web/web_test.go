package web

import (
	"strings"
	"testing"

	"github.com/mcpkg/mcpkg/internal/model"
)

func TestRenderReadmeIsSafe(t *testing.T) {
	tl := &model.Tool{RepoURL: "https://github.com/o/r", RepoSubfolder: "src/x", ReadmeURL: "https://github.com/o/r/blob/HEAD/src/x/README.md",
		Readme: "# Title\n\n<script>alert(1)</script>\n\n[docs](docs/usage.md) [bad](javascript:alert(1)) [ext](https://example.com)\n\n![logo](img/logo.png) ![plain](http://tracker.example/p.gif)\n\n| a | b |\n|---|---|\n| 1 | 2 |\n"}
	html := string(RenderReadme(tl))
	if strings.Contains(html, "<script>") {
		t.Error("raw HTML must not render")
	}
	if strings.Contains(html, "javascript:") {
		t.Error("javascript: links must be dropped")
	}
	if !strings.Contains(html, `href="https://github.com/o/r/blob/HEAD/src/x/docs/usage.md"`) {
		t.Errorf("relative link not rewritten:\n%s", html)
	}
	if !strings.Contains(html, `src="https://raw.githubusercontent.com/o/r/HEAD/src/x/img/logo.png"`) {
		t.Errorf("relative image not rewritten:\n%s", html)
	}
	if strings.Contains(html, "tracker.example") {
		t.Error("plain-http images must be dropped")
	}
	if !strings.Contains(html, `rel="nofollow ugc noopener"`) || !strings.Contains(html, "<table>") {
		t.Errorf("external link rel / GFM table missing:\n%s", html)
	}
}

func TestBadge(t *testing.T) {
	svg := BadgeSVG("mcpkg", "A 91 · works", "#2e7d32")
	if !strings.HasPrefix(svg, "<svg") || !strings.Contains(svg, "A 91 · works") || !strings.Contains(svg, `fill="#2e7d32"`) {
		t.Errorf("badge:\n%s", svg)
	}
	if strings.Contains(BadgeSVG("<x>", "y", "#000"), "<x>") {
		t.Error("badge text must be escaped")
	}
}

func TestSchemaParams(t *testing.T) {
	ps := schemaParams([]byte(`{"type":"object","properties":{"b":{"type":"string"},"a":{"type":["number","null"],"description":"A"}},"required":["b"]}`))
	if len(ps) != 2 || ps[0].Name != "b" || !ps[0].Required || ps[1].Type != "number | null" {
		t.Errorf("params = %+v", ps)
	}
}
