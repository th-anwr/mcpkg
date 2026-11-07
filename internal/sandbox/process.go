package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"

	"github.com/mcpkg/mcpkg/internal/identity"
	"github.com/mcpkg/mcpkg/internal/launch"
)

// ProcessRunner runs servers directly on the host. DEVELOPMENT ONLY: no isolation.
type ProcessRunner struct {
	BaseDir string
}

// Name implements Runner.
func (p *ProcessRunner) Name() string { return "process" }

func (p *ProcessRunner) dir(job Job) string {
	return filepath.Join(p.BaseDir, "mcpkg-smoke-"+job.ID)
}

// PathPlaceholder implements Runner.
func (p *ProcessRunner) PathPlaceholder(job Job) string { return filepath.Join(p.dir(job), "run") }

func (p *ProcessRunner) baseEnv(wd string) []string {
	env := []string{
		"PATH=" + filepath.Join(wd, "app", "node_modules", ".bin") + ":" + filepath.Join(wd, "venv", "bin") + ":" + os.Getenv("PATH"),
		"HOME=" + filepath.Join(wd, "home"),
		"TMPDIR=" + filepath.Join(wd, "tmp"),
		"LANG=C.UTF-8",
	}
	return env
}

// Install implements Runner.
func (p *ProcessRunner) Install(ctx context.Context, job Job) (string, string, error) {
	wd := p.dir(job)
	for _, d := range []string{"app", "home", "tmp", "run"} {
		if err := os.MkdirAll(filepath.Join(wd, d), 0o755); err != nil {
			return "", "", fmt.Errorf("%w: %v", ErrInfra, err)
		}
	}
	env := append(p.baseEnv(wd), passthroughEnv()...)
	switch job.Kind {
	case "npm":
		env = append(env, "npm_config_cache="+filepath.Join(wd, "npm-cache"), "npm_config_update_notifier=false", "npm_config_fund=false")
		out, err := run(ctx, "npm", []string{"install", "--no-audit", "--no-fund", "--no-save", "--prefix", filepath.Join(wd, "app"), job.Spec()}, env, wd)
		if err != nil {
			return "", out, fmt.Errorf("%w: npm install: %v", ErrInstall, err)
		}
		bins, err := npmBins(filepath.Join(wd, "app", "node_modules", job.Package, "package.json"), job.Package)
		if err != nil || len(bins) == 0 {
			return "", out, fmt.Errorf("%w: package declares no bin", ErrInstall)
		}
		return launch.PickBin(job.Package, bins, job.PreferBin, nil), out, nil
	case "pypi":
		env = append(env, "UV_CACHE_DIR="+filepath.Join(wd, "uv-cache"), "UV_PYTHON_DOWNLOADS=never")
		venv := filepath.Join(wd, "venv")
		out, err := run(ctx, "uv", []string{"venv", "--quiet", venv}, env, wd)
		if err != nil {
			return "", out, fmt.Errorf("%w: uv venv: %v", ErrInfra, err)
		}
		out2, err := run(ctx, "uv", []string{"pip", "install", "--quiet", "--python", filepath.Join(venv, "bin", "python"), job.Spec()}, env, wd)
		out += out2
		if err != nil {
			return "", out, fmt.Errorf("%w: pip install: %v", ErrInstall, err)
		}
		bins := pythonConsoleScripts(venv, job.Package)
		if len(bins) == 0 {
			return "", out, fmt.Errorf("%w: distribution declares no console script (library, not a runnable server?)", ErrInstall)
		}
		return launch.PickBin(job.Package, bins, job.PreferBin, job.EntryPoints), out, nil
	default:
		return "", "", fmt.Errorf("%w: %s packages need the docker sandbox", ErrInstall, job.Kind)
	}
}

// Start implements Runner.
func (p *ProcessRunner) Start(ctx context.Context, job Job, bin string) (*Process, error) {
	wd := p.dir(job)
	var path string
	switch job.Kind {
	case "npm":
		path = filepath.Join(wd, "app", "node_modules", ".bin", bin)
	case "pypi":
		path = filepath.Join(wd, "venv", "bin", bin)
	default:
		return nil, fmt.Errorf("%w: unsupported kind %s", ErrInfra, job.Kind)
	}
	cmd := exec.Command(path, job.Args...)
	cmd.Dir = filepath.Join(wd, "run")
	cmd.Env = append(p.baseEnv(wd), envList(job.Env)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr := NewLimitedBuffer(16 << 10)
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start: %w", err)
	}
	pid := cmd.Process.Pid
	return &Process{
		Stdin: stdin, Stdout: stdout, stderr: stderr, Display: display(filepath.Base(path), job.Args),
		stop: func() { _ = syscall.Kill(-pid, syscall.SIGKILL) },
		wait: cmd.Wait,
	}, nil
}

// Cleanup implements Runner.
func (p *ProcessRunner) Cleanup(job Job) { _ = os.RemoveAll(p.dir(job)) }

func npmBins(pkgJSON, name string) ([]string, error) {
	b, err := os.ReadFile(pkgJSON)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Name string          `json:"name"`
		Bin  json.RawMessage `json:"bin"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	return BinsFromJSON(doc.Bin, name), nil
}

// BinsFromJSON reads package.json "bin" (string or object).
func BinsFromJSON(raw json.RawMessage, name string) []string {
	var s string
	if json.Unmarshal(raw, &s) == nil && s != "" {
		short := name
		if i := strings.LastIndex(short, "/"); i >= 0 {
			short = short[i+1:]
		}
		return []string{short}
	}
	var m map[string]string
	if json.Unmarshal(raw, &m) == nil {
		out := make([]string, 0, len(m))
		for k := range m {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	return nil
}

var sectionRe = regexp.MustCompile(`^\s*\[(.+)\]\s*$`)

// pythonConsoleScripts reads entry_points.txt of the installed distribution.
func pythonConsoleScripts(venv, pkg string) []string {
	want := identity.PyPIName(pkg)
	matches, _ := filepath.Glob(filepath.Join(venv, "lib", "python*", "site-packages", "*.dist-info"))
	for _, m := range matches {
		base := strings.TrimSuffix(filepath.Base(m), ".dist-info")
		name := base
		if i := strings.LastIndex(base, "-"); i > 0 {
			name = base[:i] // strip version
		}
		if identity.PyPIName(name) != want {
			continue
		}
		data, err := os.ReadFile(filepath.Join(m, "entry_points.txt"))
		if err != nil {
			return nil
		}
		var out []string
		in := false
		for _, line := range strings.Split(string(data), "\n") {
			if sm := sectionRe.FindStringSubmatch(line); sm != nil {
				in = strings.TrimSpace(sm[1]) == "console_scripts"
				continue
			}
			if k, _, ok := strings.Cut(line, "="); in && ok && strings.TrimSpace(k) != "" {
				out = append(out, strings.TrimSpace(k))
			}
		}
		sort.Strings(out)
		return out
	}
	return nil
}
