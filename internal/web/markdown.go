package web

import (
	"bytes"
	"html/template"
	"net/url"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"

	"github.com/mcpkg/mcpkg/internal/identity"
	"github.com/mcpkg/mcpkg/internal/model"
)

// readmeTransformer rewrites relative links/images to the source repo and
// hardens external links. Raw HTML is never rendered (goldmark default).
type readmeTransformer struct {
	linkBase  string // https://github.com/o/r/blob/HEAD/sub/
	imageBase string // https://raw.githubusercontent.com/o/r/HEAD/sub/
}

func (rt *readmeTransformer) Transform(doc *ast.Document, reader text.Reader, pc parser.Context) {
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch v := n.(type) {
		case *ast.Link:
			v.Destination = rt.resolve(v.Destination, rt.linkBase)
			if isExternal(v.Destination) {
				v.SetAttributeString("rel", []byte("nofollow ugc noopener"))
			}
		case *ast.Image:
			v.Destination = rt.resolve(v.Destination, rt.imageBase)
			if strings.HasPrefix(string(v.Destination), "http://") {
				v.Destination = nil // no mixed content / plain-http trackers
			}
			v.SetAttributeString("loading", []byte("lazy"))
			v.SetAttributeString("referrerpolicy", []byte("no-referrer"))
		case *ast.AutoLink:
			// rendered as-is; goldmark filters dangerous schemes
		}
		return ast.WalkContinue, nil
	})
}

func isExternal(dest []byte) bool {
	s := string(dest)
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

func (rt *readmeTransformer) resolve(dest []byte, base string) []byte {
	s := strings.TrimSpace(string(dest))
	if s == "" || base == "" || strings.HasPrefix(s, "#") || strings.HasPrefix(s, "mailto:") {
		return dest
	}
	u, err := url.Parse(s)
	if err != nil {
		return nil
	}
	if u.IsAbs() {
		if u.Scheme != "http" && u.Scheme != "https" {
			return nil
		}
		return dest
	}
	b, err := url.Parse(base)
	if err != nil {
		return dest
	}
	return []byte(b.ResolveReference(u).String())
}

// RenderReadme converts README markdown to safe HTML.
func RenderReadme(t *model.Tool) template.HTML {
	if strings.TrimSpace(t.Readme) == "" {
		return ""
	}
	rt := &readmeTransformer{}
	if repo, ok := identity.ParseRepo(t.RepoURL); ok && repo.IsGitHub() {
		sub := identity.CleanSubfolder(t.RepoSubfolder)
		if sub != "" && !strings.Contains(t.ReadmeURL, "/"+sub+"/") {
			sub = "" // README came from the repo root
		}
		if sub != "" {
			sub += "/"
		}
		rt.linkBase = "https://github.com/" + repo.Owner + "/" + repo.Name + "/blob/HEAD/" + sub
		rt.imageBase = "https://raw.githubusercontent.com/" + repo.Owner + "/" + repo.Name + "/HEAD/" + sub
	}
	md := goldmark.New(
		goldmark.WithExtensions(extension.GFM),
		goldmark.WithParserOptions(parser.WithAutoHeadingID(), parser.WithASTTransformers(util.Prioritized(rt, 100))),
	)
	var buf bytes.Buffer
	src := t.Readme
	if strings.HasSuffix(strings.ToLower(t.ReadmeURL), ".rst") || strings.HasSuffix(strings.ToLower(t.ReadmeURL), ".txt") {
		return template.HTML("<pre class=\"plain-readme\">" + template.HTMLEscapeString(src) + "</pre>")
	}
	if err := md.Convert([]byte(src), &buf); err != nil {
		return template.HTML("<pre class=\"plain-readme\">" + template.HTMLEscapeString(src) + "</pre>")
	}
	return template.HTML(buf.String()) // safe: goldmark omits raw HTML unless WithUnsafe()
}
