package web

import (
	"encoding/json"
	"fmt"
	"html/template"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/mcpkg/mcpkg/internal/model"
)

func funcs() template.FuncMap {
	return template.FuncMap{
		"ago":  ago,
		"agoT": func(t time.Time) string { return ago(&t) },
		"f1": func(f *float64) string {
			if f == nil {
				return "—"
			}
			return fmt.Sprintf("%.1f", *f)
		},
		"date": func(t *time.Time) string {
			if t == nil {
				return "—"
			}
			return t.Format("Jan 2, 2006")
		},
		"gradeClass": func(g string) string {
			if g == "" {
				return "grade-none"
			}
			return "grade-" + strings.ToLower(g)
		},
		"smokeLabel": smokeLabel,
		"smokeClass": smokeClass,
		"num":        humanNum,
		"join":       strings.Join,
		"lower":      strings.ToLower,
		"upper":      strings.ToUpper,
		"capLabel":   capLabel,
		"catLabel":   catLabel,
		"add":        func(a, b int) int { return a + b },
		"sub":        func(a, b int) int { return a - b },
		"pct":        func(v int) int { return max(0, min(100, v)) },
		"params":     schemaParams,
		"pretty":     prettyJSON,
		"sevClass":   func(s string) string { return "sev-" + s },
		"deref": func(p *int) int {
			if p == nil {
				return 0
			}
			return *p
		},
		"hasPrefix": strings.HasPrefix,
		"trim":      strings.TrimSpace,
		"truncate": func(s string, n int) string {
			if len(s) <= n {
				return s
			}
			return strings.TrimSpace(s[:n]) + "…"
		},
		"firstLine": func(s string) string { s, _, _ = strings.Cut(strings.TrimSpace(s), "\n"); return s },
		"short":     shortName,
		"seq": func(n int) []int {
			out := make([]int, n)
			for i := range out {
				out[i] = i
			}
			return out
		},
		"stageLabel": stageLabel,
		"envHelp": func(t *model.Tool, name string) string {
			if t.Enrich == nil {
				return ""
			}
			return t.Enrich.EnvHelp[name]
		},
		"jsonAttr": func(v any) string { b, _ := json.Marshal(v); return string(b) },
	}
}

func ago(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "—"
	}
	d := time.Since(*t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d.Minutes()), "minute") + " ago"
	case d < 24*time.Hour:
		return plural(int(d.Hours()), "hour") + " ago"
	case d < 30*24*time.Hour:
		return plural(int(d.Hours()/24), "day") + " ago"
	case d < 365*24*time.Hour:
		return plural(int(d.Hours()/24/30), "month") + " ago"
	}
	return plural(int(d.Hours()/24/365), "year") + " ago"
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

func humanNum(v any) string {
	var f float64
	switch n := v.(type) {
	case int:
		f = float64(n)
	case int64:
		f = float64(n)
	case float64:
		f = n
	}
	switch {
	case f >= 1e6:
		return trimZero(fmt.Sprintf("%.1f", f/1e6)) + "M"
	case f >= 1e3:
		return trimZero(fmt.Sprintf("%.1f", f/1e3)) + "k"
	}
	return fmt.Sprintf("%d", int64(math.Round(f)))
}

func trimZero(s string) string { return strings.TrimSuffix(s, ".0") }

func smokeLabel(s string) string {
	switch s {
	case "works":
		return "Works"
	case "needs_credentials":
		return "Needs API key"
	case "auth_required":
		return "Sign-in required"
	case "broken":
		return "Fails to start"
	case "install_failed":
		return "Install fails"
	case "skipped":
		return "Not smoke-tested"
	}
	return "Pending"
}

func smokeClass(s string) string {
	switch s {
	case "works":
		return "ok"
	case "needs_credentials", "auth_required":
		return "key"
	case "broken", "install_failed":
		return "bad"
	}
	return "muted"
}

func capLabel(c string) string {
	return map[string]string{
		"shell": "Runs shell commands", "network": "Makes network requests", "fs_write": "Writes or deletes files",
		"env": "Reads environment variables", "browser": "Controls a web browser",
	}[c]
}

func catLabel(c string) string {
	special := map[string]string{"ai-ml": "AI & ML", "e-commerce": "E-commerce", "devops": "DevOps", "maps-location": "Maps & location",
		"knowledge-memory": "Knowledge & memory", "video-audio": "Video & audio", "data-analytics": "Data & analytics"}
	if v, ok := special[c]; ok {
		return v
	}
	s := strings.ReplaceAll(c, "-", " ")
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func stageLabel(s string) string {
	switch s {
	case "health":
		return "Repo health"
	case "security":
		return "Security scan"
	case "smoke":
		return "Smoke test"
	case "enrich":
		return "Description"
	case "render":
		return "Score & page"
	}
	if strings.HasPrefix(s, "hook:") {
		return "Hook: " + strings.TrimPrefix(s, "hook:")
	}
	return s
}

func shortName(name string) string {
	if i := strings.LastIndex(name, "/"); i >= 0 && i < len(name)-1 {
		return name[i+1:]
	}
	return name
}

// Param is one input parameter of a tool, derived from its JSON schema.
type Param struct {
	Name        string
	Type        string
	Required    bool
	Description string
}

func schemaParams(raw json.RawMessage) []Param {
	if len(raw) == 0 {
		return nil
	}
	var s struct {
		Properties map[string]struct {
			Type        any    `json:"type"`
			Description string `json:"description"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if json.Unmarshal(raw, &s) != nil {
		return nil
	}
	req := map[string]bool{}
	for _, r := range s.Required {
		req[r] = true
	}
	var out []Param
	for name, p := range s.Properties {
		typ := ""
		switch t := p.Type.(type) {
		case string:
			typ = t
		case []any:
			var parts []string
			for _, x := range t {
				parts = append(parts, fmt.Sprint(x))
			}
			typ = strings.Join(parts, " | ")
		}
		out = append(out, Param{Name: name, Type: typ, Required: req[name], Description: p.Description})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Required != out[j].Required {
			return out[i].Required
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func prettyJSON(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return ""
	}
	return string(b)
}
