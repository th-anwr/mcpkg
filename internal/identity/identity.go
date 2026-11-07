// Package identity normalizes repo URLs and package names into stable keys
// used to deduplicate the same tool discovered from different sources.
package identity

import (
	"net/url"
	"regexp"
	"strings"
)

// Repo is a normalized source repository reference.
type Repo struct {
	Host      string // github.com
	Owner     string
	Name      string
	Subfolder string // monorepo path, if known
}

// Slug returns "owner/name".
func (r Repo) Slug() string { return r.Owner + "/" + r.Name }

// URL returns the canonical https URL.
func (r Repo) URL() string { return "https://" + r.Host + "/" + r.Owner + "/" + r.Name }

// Key returns the dedupe key, including the subfolder for monorepos.
func (r Repo) Key() string {
	k := r.Host + "/" + r.Owner + "/" + r.Name
	if r.Subfolder != "" {
		k += "#" + r.Subfolder
	}
	return k
}

// IsGitHub reports whether the repo is on github.com.
func (r Repo) IsGitHub() bool { return r.Host == "github.com" }

var scpLike = regexp.MustCompile(`^(?:[\w.-]+@)?([\w.-]+\.[a-z]{2,}):(.+)$`)

// ParseRepo normalizes the many forms repo URLs take in package metadata:
// git+https://github.com/o/r.git, git@github.com:o/r.git, ssh://git@github.com/o/r,
// github:o/r, o/r (npm shorthand), https://github.com/o/r/tree/main/packages/x ...
func ParseRepo(raw string) (Repo, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return Repo{}, false
	}
	s = strings.TrimPrefix(s, "git+")
	switch {
	case strings.HasPrefix(s, "github:"):
		s = "https://github.com/" + strings.TrimPrefix(s, "github:")
	case strings.HasPrefix(s, "gitlab:"):
		s = "https://gitlab.com/" + strings.TrimPrefix(s, "gitlab:")
	case strings.HasPrefix(s, "bitbucket:"):
		s = "https://bitbucket.org/" + strings.TrimPrefix(s, "bitbucket:")
	}
	if !strings.Contains(s, "://") {
		if m := scpLike.FindStringSubmatch(s); m != nil {
			s = "https://" + m[1] + "/" + m[2]
		} else if ownerRepo.MatchString(s) {
			s = "https://github.com/" + s // npm shorthand "owner/repo"
		} else if strings.HasPrefix(s, "github.com/") || strings.HasPrefix(s, "www.github.com/") {
			s = "https://" + s
		} else {
			return Repo{}, false
		}
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return Repo{}, false
	}
	host := strings.ToLower(strings.TrimPrefix(u.Hostname(), "www."))
	switch host {
	case "github.com", "gitlab.com", "bitbucket.org", "codeberg.org":
	default:
		return Repo{}, false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return Repo{}, false
	}
	r := Repo{Host: host, Owner: strings.ToLower(parts[0]), Name: strings.ToLower(strings.TrimSuffix(parts[1], ".git"))}
	if r.Name == "" || r.Owner == "orgs" || r.Owner == "sponsors" {
		return Repo{}, false
	}
	// https://github.com/o/r/tree/<branch>/<path...>  or /blob/<branch>/<path>
	if len(parts) > 4 && (parts[2] == "tree" || parts[2] == "blob") {
		r.Subfolder = CleanSubfolder(strings.Join(parts[4:], "/"))
	}
	if f := u.Fragment; f != "" && r.Subfolder == "" && !strings.Contains(f, "readme") {
		// npm sometimes uses "#path" style fragments; ignore anchors like #readme.
		if strings.Contains(f, "/") {
			r.Subfolder = CleanSubfolder(f)
		}
	}
	return r, true
}

// CleanSubfolder normalizes a monorepo directory ("./packages/x/" -> "packages/x").
func CleanSubfolder(p string) string {
	p = strings.TrimSpace(p)
	p = strings.TrimPrefix(p, "./")
	p = strings.Trim(p, "/")
	if p == "." || strings.Contains(p, "..") {
		return ""
	}
	if strings.HasSuffix(strings.ToLower(p), "readme.md") {
		p = strings.Trim(p[:len(p)-len("readme.md")], "/")
	}
	return p
}

var pep503 = regexp.MustCompile(`[-_.]+`)

// PyPIName normalizes a Python distribution name per PEP 503.
func PyPIName(n string) string {
	return pep503.ReplaceAllString(strings.ToLower(strings.TrimSpace(n)), "-")
}

// NpmName normalizes an npm package name (npm names are already lowercase).
func NpmName(n string) string { return strings.ToLower(strings.TrimSpace(n)) }

// OCIName strips the tag/digest from an image reference.
func OCIName(ref string) string {
	ref = strings.ToLower(strings.TrimSpace(ref))
	if i := strings.Index(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	// a ':' after the last '/' is a tag separator (a ':' before it is a registry port)
	if slash := strings.LastIndex(ref, "/"); strings.LastIndex(ref, ":") > slash {
		ref = ref[:strings.LastIndex(ref, ":")]
	}
	ref = strings.TrimPrefix(ref, "docker.io/")
	return ref
}

// RemoteURL normalizes an endpoint URL for dedupe.
func RemoteURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return strings.ToLower(strings.TrimSpace(raw))
	}
	u.Host = strings.ToLower(u.Host)
	u.Scheme = strings.ToLower(u.Scheme)
	u.RawQuery = ""
	u.Fragment = ""
	return strings.TrimRight(u.String(), "/")
}

var (
	slugInvalid = regexp.MustCompile(`[^a-z0-9.-]+`)
	multiDash   = regexp.MustCompile(`-{2,}`)
	multiDot    = regexp.MustCompile(`\.{2,}`)
	keyInvalid  = regexp.MustCompile(`[^a-z0-9-]+`)
	ownerRepo   = regexp.MustCompile(`^[\w.-]+/[\w.-]+$`)
)

// Slugify makes a URL-safe slug from a tool name:
// "io.github.foo/youtube-mcp" -> "io.github.foo.youtube-mcp", "@scope/pkg" -> "scope.pkg".
func Slugify(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	s = strings.TrimPrefix(s, "@")
	s = strings.ReplaceAll(s, "/", ".")
	s = slugInvalid.ReplaceAllString(s, "-")
	s = multiDash.ReplaceAllString(s, "-")
	s = multiDot.ReplaceAllString(s, ".")
	s = strings.Trim(s, "-.")
	if len(s) > 120 {
		s = strings.Trim(s[:120], "-.")
	}
	if s == "" {
		s = "tool"
	}
	return s
}

// ServerKey is the short name used as the key in client configs ("youtube-mcp").
func ServerKey(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	if i := strings.LastIndex(n, "/"); i >= 0 {
		n = n[i+1:]
	}
	n = keyInvalid.ReplaceAllString(n, "-")
	n = strings.Trim(multiDash.ReplaceAllString(n, "-"), "-")
	if n == "" {
		n = "mcp-server"
	}
	if len(n) > 48 {
		n = strings.Trim(n[:48], "-")
	}
	return n
}
