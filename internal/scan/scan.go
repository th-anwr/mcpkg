// Package scan contains the built-in static checks (no code is executed) and
// safe archive extraction used by the security stage.
package scan

import (
	"bufio"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mcpkg/mcpkg/internal/model"
)

// Rule is one regex check.
type Rule struct {
	ID         string
	Severity   string // high | medium | low | info
	Capability string // when set, a match records a capability instead of a finding
	Message    string
	Re         *regexp.Regexp
	Exts       map[string]bool // nil = all scanned extensions
}

var codeExts = map[string]bool{".js": true, ".mjs": true, ".cjs": true, ".ts": true, ".mts": true, ".cts": true,
	".py": true, ".go": true, ".rs": true, ".rb": true, ".java": true, ".kt": true, ".cs": true, ".php": true, ".sh": true, ".bash": true}

var scanExts = map[string]bool{".js": true, ".mjs": true, ".cjs": true, ".ts": true, ".mts": true, ".cts": true, ".jsx": true, ".tsx": true,
	".py": true, ".go": true, ".rs": true, ".rb": true, ".java": true, ".kt": true, ".cs": true, ".php": true, ".sh": true, ".bash": true,
	".json": true, ".toml": true, ".yaml": true, ".yml": true, ".cfg": true, ".ini": true, ".txt": true, ".env": true}

// Rules are deliberately conservative: high = "a human should look", not "malware".
// Only OSV malicious-package advisories and registry moderation auto-block.
var Rules = []Rule{
	// Capabilities (shown on the page like app permissions).
	{ID: "cap-shell", Capability: "shell", Re: regexp.MustCompile(`child_process|\bexecSync\(|\bspawn\(|subprocess\.(run|Popen|call|check_output)|os\.system\(|exec\.Command\(|std::process::Command|Runtime\.getRuntime\(\)\.exec`), Exts: codeExts},
	{ID: "cap-network", Capability: "network", Re: regexp.MustCompile(`\bfetch\(|axios|node-fetch|undici|requests\.(get|post|put|request)|httpx\.|aiohttp|urllib\.request|http\.(Get|Post|NewRequest)|reqwest::|got\(`), Exts: codeExts},
	{ID: "cap-fs-write", Capability: "fs_write", Re: regexp.MustCompile(`fs\.(writeFile|appendFile|unlink|rm|rmdir|rename|mkdir)|writeFileSync|open\([^)]*['"][wa]\+?['"]|os\.(Remove|RemoveAll|WriteFile|Rename)|shutil\.(rmtree|move)|std::fs::(write|remove)`), Exts: codeExts},
	{ID: "cap-env", Capability: "env", Re: regexp.MustCompile(`process\.env|os\.environ|os\.getenv|os\.Getenv|std::env::var`), Exts: codeExts},
	{ID: "cap-browser", Capability: "browser", Re: regexp.MustCompile(`puppeteer|playwright|selenium|chromedp`), Exts: codeExts},

	// Findings.
	{ID: "sensitive-path", Severity: "high", Message: "reads credential or key material paths",
		Re: regexp.MustCompile(`\.ssh/(id_|authorized_keys|known_hosts)|\bid_rsa\b|\.aws/credentials|\.git-credentials|\.npmrc['"]|\.pypirc|Login Data|Local State['"]|\.config/gcloud|wallet\.dat|Keychains?/`), Exts: codeExts},
	{ID: "exfil-endpoint", Severity: "high", Message: "sends data to a paste/webhook/tunnel service",
		Re: regexp.MustCompile(`discord(app)?\.com/api/webhooks|api\.telegram\.org/bot|pastebin\.com/(raw|api)|webhook\.site|requestbin|ngrok(-free)?\.(io|app)|pipedream\.net|interact\.sh|burpcollaborator`), Exts: codeExts},
	{ID: "hardcoded-ip", Severity: "medium", Message: "contacts a hard-coded public IP address",
		Re: regexp.MustCompile(`https?://(?:(?:25[0-5]|2[0-4]\d|1?\d?\d)\.){3}(?:25[0-5]|2[0-4]\d|1?\d?\d)`), Exts: codeExts},
	{ID: "obfuscated-exec", Severity: "high", Message: "executes decoded/obfuscated code",
		Re: regexp.MustCompile(`eval\(\s*(atob|Buffer\.from|unescape|String\.fromCharCode)|new Function\(\s*(atob|Buffer\.from)|exec\(\s*(base64\.b64decode|zlib\.decompress|marshal\.loads|bytes\.fromhex)|eval\(\s*compile\(`), Exts: codeExts},
	{ID: "prompt-injection-text", Severity: "high", Message: "contains instructions aimed at the AI agent (tool poisoning)",
		Re: regexp.MustCompile(`(?i)ignore (all |any )?(previous|prior|above) instructions|do not (tell|inform|mention|reveal)[^.\n]{0,30}\buser\b|<\s*important\s*>|before (using|calling) this tool[^.\n]{0,60}\b(read|send|include|pass|cat)\b|(read|send|include|exfiltrate)[^.\n]{0,40}(~/\.ssh|id_rsa|\.env\b|mcp\.json|credentials)`)},
	{ID: "committed-secret", Severity: "medium", Message: "a credential appears to be committed",
		Re: regexp.MustCompile(`AKIA[0-9A-Z]{16}|-----BEGIN (RSA |EC |OPENSSH |DSA )?PRIVATE KEY-----|ghp_[A-Za-z0-9]{36}|github_pat_[A-Za-z0-9_]{60,}|xox[baprs]-[A-Za-z0-9-]{10,}|sk-ant-[A-Za-z0-9_-]{20,}|AIza[0-9A-Za-z_-]{35}`)},
}

// Report is the result of scanning a directory.
type Report struct {
	Files        int
	Findings     []model.Finding
	Capabilities []string
}

// Options bound the scan.
type Options struct {
	MaxFiles    int
	MaxFileSize int64
	MaxFindings int
	Subdir      string
	Log         *slog.Logger // optional: Debug summary per scan
}

var skipDirs = map[string]bool{".git": true, "node_modules": true, "vendor": true, "__pycache__": true, ".venv": true, "venv": true,
	"test": true, "tests": true, "__tests__": true, "fixtures": true, "testdata": true, "examples": true, "example": true, "docs": true, ".github": true}

// Dir scans a directory tree.
func Dir(root string, opt Options) Report {
	if opt.MaxFiles == 0 {
		opt.MaxFiles = 4000
	}
	if opt.MaxFileSize == 0 {
		opt.MaxFileSize = 1 << 20
	}
	if opt.MaxFindings == 0 {
		opt.MaxFindings = 50
	}
	base := root
	if opt.Subdir != "" {
		if st, err := os.Stat(filepath.Join(root, opt.Subdir)); err == nil && st.IsDir() {
			base = filepath.Join(root, opt.Subdir)
		}
	}
	var rep Report
	caps := map[string]bool{}
	start := time.Now()
	_ = filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if opt.Log != nil {
				opt.Log.Debug("scan walk error", "path", p, "err", err)
			}
			return nil
		}
		if d.IsDir() {
			if p != base && skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || rep.Files >= opt.MaxFiles {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(p))
		if !scanExts[ext] || strings.HasSuffix(p, ".min.js") || strings.HasSuffix(p, ".map") || d.Name() == "package-lock.json" {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > opt.MaxFileSize {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rep.Files++
		fileFindings(p, rel, ext, caps, &rep, opt.MaxFindings)
		return nil
	})
	for c := range caps {
		rep.Capabilities = append(rep.Capabilities, c)
	}
	sort.Strings(rep.Capabilities)
	if opt.Log != nil {
		opt.Log.Debug("scan done", "root", root, "files", rep.Files, "findings", len(rep.Findings),
			"capabilities", len(rep.Capabilities), "ms", time.Since(start).Milliseconds())
	}
	return rep
}

func fileFindings(path, rel, ext string, caps map[string]bool, rep *Report, maxFindings int) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 2<<20)
	line := 0
	seen := map[string]bool{}
	for sc.Scan() {
		line++
		text := sc.Text()
		if hidden := HiddenUnicode(text); hidden != "" && !seen["hidden-unicode"] {
			seen["hidden-unicode"] = true
			add(rep, maxFindings, model.Finding{Rule: "hidden-unicode", Severity: "high", File: rel, Line: line,
				Message: "invisible/bidi Unicode characters (" + hidden + ") can hide instructions or code"})
		}
		for _, r := range Rules {
			if r.Exts != nil && !r.Exts[ext] {
				continue
			}
			if r.Capability != "" {
				if !caps[r.Capability] && r.Re.MatchString(text) {
					caps[r.Capability] = true
				}
				continue
			}
			if seen[r.ID] {
				continue
			}
			if loc := r.Re.FindStringIndex(text); loc != nil {
				if r.ID == "hardcoded-ip" && privateIP(text[loc[0]:loc[1]]) {
					continue
				}
				seen[r.ID] = true
				add(rep, maxFindings, model.Finding{Rule: r.ID, Severity: r.Severity, File: rel, Line: line,
					Snippet: snippet(text, loc[0], loc[1]), Message: r.Message})
			}
		}
	}
}

func add(rep *Report, maxFindings int, f model.Finding) {
	if len(rep.Findings) < maxFindings {
		rep.Findings = append(rep.Findings, f)
	}
}

func snippet(s string, a, b int) string {
	from := max(a-40, 0)
	to := min(b+40, len(s))
	for from > 0 && !utf8.RuneStart(s[from]) {
		from--
	}
	for to < len(s) && !utf8.RuneStart(s[to]) {
		to++
	}
	out := strings.TrimSpace(s[from:to])
	if len(out) > 160 {
		out = out[:160]
	}
	return out
}

var private172 = regexp.MustCompile(`^172\.(1[6-9]|2\d|3[01])\.`)

func privateIP(u string) bool {
	h := strings.TrimPrefix(strings.TrimPrefix(u, "http://"), "https://")
	return strings.HasPrefix(h, "127.") || strings.HasPrefix(h, "0.") || strings.HasPrefix(h, "10.") ||
		strings.HasPrefix(h, "192.168.") || private172.MatchString(h) || strings.HasPrefix(h, "169.254.")
}

// HiddenUnicode reports suspicious invisible characters (zero-width, bidi, tag chars).
func HiddenUnicode(s string) string {
	var kinds []string
	seen := map[string]bool{}
	for i, r := range s {
		k := ""
		switch {
		case r == 0x200B || r == 0x200C || r == 0x200D || r == 0x2060 || r == 0x2062 || r == 0x2063:
			k = "zero-width"
		case r == 0xFEFF && i > 0:
			k = "zero-width"
		case r >= 0x202A && r <= 0x202E, r >= 0x2066 && r <= 0x2069:
			k = "bidi-override"
		case r >= 0xE0000 && r <= 0xE007F:
			k = "tag-characters"
		}
		if k != "" && !seen[k] {
			seen[k] = true
			kinds = append(kinds, k)
		}
	}
	return strings.Join(kinds, ", ")
}

// Text checks free text such as tool descriptions from tools/list.
func Text(where, s string) []model.Finding {
	var out []model.Finding
	if h := HiddenUnicode(s); h != "" {
		out = append(out, model.Finding{Rule: "hidden-unicode", Severity: "high", File: where,
			Message: "tool description contains invisible characters (" + h + ")"})
	}
	for _, r := range Rules {
		if r.ID != "prompt-injection-text" {
			continue
		}
		if loc := r.Re.FindStringIndex(s); loc != nil {
			out = append(out, model.Finding{Rule: r.ID, Severity: r.Severity, File: where, Snippet: snippet(s, loc[0], loc[1]), Message: r.Message})
		}
	}
	return out
}
