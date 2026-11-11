package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mcpkg/mcpkg/internal/launch"
)

// DockerRunner isolates each smoke test in throwaway containers.
type DockerRunner struct {
	Image         string // mcpkg-runner image (node + python + uv, non-root user)
	Runtime       string // "" or "runsc" (gVisor)
	EgressNetwork string // network whose only exit is the allowlist proxy
	Proxy         string // http://egress-proxy:3128; a name is resolved to its address on EgressNetwork
	Memory        string
	CPUs          string
	Log           *slog.Logger // optional: container lifecycle

	proxyMu  sync.Mutex
	proxyURL string // Proxy with the name replaced by the proxy container's address
	proxyAt  time.Time
}

// proxyTTL is how long a resolved proxy address is reused before asking Docker again.
const proxyTTL = time.Minute

// sandboxProxy returns the proxy URL to hand to install containers.
//
// A proxy given by name (the default, "egress-proxy") only resolves through
// Docker's embedded DNS server at 127.0.0.11. gVisor's network stack cannot
// reach that server, so under runsc every install failed with
// "getaddrinfo EAI_AGAIN egress-proxy". The name is therefore replaced by the
// address the proxy container has on the egress network, which needs no DNS.
func (d *DockerRunner) sandboxProxy(ctx context.Context) (string, error) {
	u, err := url.Parse(d.Proxy)
	if err != nil || u.Hostname() == "" {
		return "", fmt.Errorf("SANDBOX_PROXY %q is not a URL", d.Proxy)
	}
	host := u.Hostname()
	if net.ParseIP(host) != nil {
		return d.Proxy, nil
	}
	d.proxyMu.Lock()
	defer d.proxyMu.Unlock()
	if d.proxyURL != "" && time.Since(d.proxyAt) < proxyTTL {
		return d.proxyURL, nil
	}
	ids, err := run(ctx, "docker", []string{"ps", "-q", "--filter", "network=" + d.EgressNetwork}, nil, "")
	if err != nil {
		return "", fmt.Errorf("list containers on %s: %v: %s", d.EgressNetwork, err, tail(ids, 200))
	}
	if len(strings.Fields(ids)) == 0 {
		return "", fmt.Errorf("no container is running on network %s (is %s up?)", d.EgressNetwork, host)
	}
	args := append([]string{"inspect", "--format", "{{json .NetworkSettings.Networks}}"}, strings.Fields(ids)...)
	out, err := run(ctx, "docker", args, nil, "")
	if err != nil {
		return "", fmt.Errorf("inspect containers on %s: %v: %s", d.EgressNetwork, err, tail(out, 200))
	}
	ip := proxyIP(out, d.EgressNetwork, host)
	if ip == "" {
		return "", fmt.Errorf("no container on network %s answers to %q", d.EgressNetwork, host)
	}
	d.proxyURL, d.proxyAt = withHost(u, ip), time.Now()
	d.info("sandbox proxy resolved", "name", host, "address", ip, "network", d.EgressNetwork)
	return d.proxyURL, nil
}

func (d *DockerRunner) forgetProxy() {
	d.proxyMu.Lock()
	d.proxyURL = ""
	d.proxyMu.Unlock()
}

// proxyIP finds, in `docker inspect --format '{{json .NetworkSettings.Networks}}'`
// output (one JSON object per container), the address on network of the
// container that answers to host there (a compose service name or alias).
func proxyIP(inspectOut, network, host string) string {
	for _, line := range strings.Split(inspectOut, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var nets map[string]struct {
			IPAddress string
			Aliases   []string
			DNSNames  []string
		}
		if err := json.Unmarshal([]byte(line), &nets); err != nil {
			continue
		}
		n, ok := nets[network]
		if !ok || net.ParseIP(n.IPAddress) == nil {
			continue
		}
		names := append(append([]string{}, n.Aliases...), n.DNSNames...)
		for _, name := range names {
			if strings.EqualFold(name, host) {
				return n.IPAddress
			}
		}
	}
	return ""
}

// withHost returns u with its host replaced by ip, keeping scheme, port and credentials.
func withHost(u *url.URL, ip string) string {
	c := *u
	switch port := u.Port(); {
	case port != "":
		c.Host = net.JoinHostPort(ip, port)
	case strings.Contains(ip, ":"):
		c.Host = "[" + ip + "]"
	default:
		c.Host = ip
	}
	return c.String()
}

// proxyUnreachable reports install output that shows the sandbox could not
// reach the egress proxy itself. That is our infrastructure, not the package.
func proxyUnreachable(out string, proxies ...string) bool {
	for _, p := range proxies {
		u, err := url.Parse(p)
		if err != nil || u.Hostname() == "" {
			continue
		}
		for _, sig := range []string{"EAI_AGAIN " + u.Hostname(), "ENOTFOUND " + u.Hostname(),
			"ECONNREFUSED " + u.Host, "EHOSTUNREACH " + u.Host, "ETIMEDOUT " + u.Host} {
			if strings.Contains(out, sig) {
				return true
			}
		}
	}
	return false
}

func (d *DockerRunner) info(msg string, args ...any) {
	if d.Log != nil {
		d.Log.Info(msg, append([]any{"image", d.Image, "runtime", d.Name()}, args...)...)
	}
}

// exitCode is the container's exit status (-1 when it did not run).
func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	if err != nil {
		return -1
	}
	return 0
}

// Name implements Runner.
func (d *DockerRunner) Name() string {
	if d.Runtime != "" {
		return "docker+" + d.Runtime
	}
	return "docker"
}

// PathPlaceholder implements Runner.
func (d *DockerRunner) PathPlaceholder(job Job) string { return "/tmp/mcpkg-smoke" }

func (d *DockerRunner) volume(job Job) string { return "mcpkg-smoke-" + job.ID }

// hardening flags shared by both phases.
func (d *DockerRunner) hardening(pids string) []string {
	args := []string{
		"--memory", d.Memory, "--memory-swap", d.Memory, "--cpus", d.CPUs, "--pids-limit", pids,
		"--security-opt", "no-new-privileges", "--cap-drop", "ALL", "--ulimit", "nofile=1024:1024",
	}
	if d.Runtime != "" {
		args = append(args, "--runtime", d.Runtime)
	}
	return args
}

const npmInstallScript = `set -e
mkdir -p /work/app /work/home
npm install --no-audit --no-fund --no-save --prefix /work/app "$MCPKG_SPEC"
node -e 'const p=require("/work/app/node_modules/"+process.env.MCPKG_PKG+"/package.json");const b=p.bin;let out=[];if(typeof b==="string"){out=[process.env.MCPKG_PKG.split("/").pop()]}else if(b){out=Object.keys(b)}console.log("MCPKG_BINS="+JSON.stringify(out))'`

// pypiInstallScript installs with the system Python 3.11 first, named
// explicitly because uv otherwise prefers its own (newer) builds and would
// change results for packages that work today. Only when uv reports that the
// package needs a newer Python does it retry with 3.13, then 3.12, which the
// runner image ships (issue N25).
const pypiInstallScript = `set -e
pyinstall() {
  rm -rf /work/venv &&
  uv venv --quiet --python "$1" /work/venv &&
  uv pip install --quiet --python /work/venv/bin/python "$MCPKG_SPEC"
}
if ! out=$(pyinstall 3.11 2>&1); then
  printf '%s\n' "$out" >&2
  case "$out" in
  *"does not satisfy Python"*)
    echo "mcpkg: package needs a newer Python; retrying with 3.13, then 3.12" >&2
    pyinstall 3.13 || pyinstall 3.12 ;;
  *) exit 1 ;;
  esac
fi
/work/venv/bin/python -c '
import importlib.metadata as m, json, re, os
norm=lambda s: re.sub(r"[-_.]+","-",s).lower()
want=norm(os.environ["MCPKG_PKG"])
for d in m.distributions():
    if norm(d.metadata["Name"] or "")==want:
        print("MCPKG_BINS="+json.dumps(sorted(e.name for e in d.entry_points if e.group=="console_scripts")))
        break
'`

// InstallArgs builds the `docker run` argv for the install phase with the
// proxy exactly as configured (exported for tests).
func (d *DockerRunner) InstallArgs(job Job) []string { return d.installArgs(job, d.Proxy) }

// installArgs is InstallArgs with the proxy URL the sandbox should use.
func (d *DockerRunner) installArgs(job Job, proxy string) []string {
	script := npmInstallScript
	if job.Kind == "pypi" {
		script = pypiInstallScript
	}
	args := []string{"run", "--rm", "--name", "mcpkg-install-" + job.ID,
		"--network", d.EgressNetwork,
		"-e", "HTTPS_PROXY=" + proxy, "-e", "HTTP_PROXY=" + proxy, "-e", "NO_PROXY=localhost",
		"-e", "npm_config_cache=/work/npm-cache", "-e", "npm_config_update_notifier=false",
		"-e", "UV_CACHE_DIR=/work/uv-cache", "-e", "UV_PYTHON_DOWNLOADS=never",
		"-e", "HOME=/work/home", "-e", "MCPKG_SPEC=" + job.Spec(), "-e", "MCPKG_PKG=" + job.Package,
		"-v", d.volume(job) + ":/work",
	}
	args = append(args, d.hardening("512")...)
	return append(args, d.Image, "sh", "-c", script)
}

// RunArgs builds the `docker run` argv for the no-network run phase (exported for tests).
func (d *DockerRunner) RunArgs(job Job, bin string) []string {
	args := []string{"run", "-i", "--rm", "--name", "mcpkg-run-" + job.ID, "--network", "none"}
	args = append(args, d.hardening("256")...)
	keys := make([]string, 0, len(job.Env))
	for k := range job.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	switch job.Kind {
	case "oci":
		for _, k := range keys {
			args = append(args, "-e", k+"="+job.Env[k])
		}
		args = append(args, job.Package)
		return append(args, job.Args...)
	default:
		args = append(args, "--read-only", "--tmpfs", "/tmp:rw,size=256m", "--tmpfs", "/home/runner:rw,size=256m",
			"-e", "HOME=/home/runner", "-v", d.volume(job)+":/work:ro", "--workdir", "/tmp")
		for _, k := range keys {
			args = append(args, "-e", k+"="+job.Env[k])
		}
		path := "/work/app/node_modules/.bin/" + bin
		if job.Kind == "pypi" {
			path = "/work/venv/bin/" + bin
		}
		args = append(args, "--entrypoint", "", d.Image, "sh", "-c", `mkdir -p /tmp/mcpkg-smoke && exec "$0" "$@"`, path)
		return append(args, job.Args...)
	}
}

// Install implements Runner.
func (d *DockerRunner) Install(ctx context.Context, job Job) (string, string, error) {
	if out, err := run(ctx, "docker", []string{"volume", "create", d.volume(job)}, nil, ""); err != nil {
		return "", out, fmt.Errorf("%w: docker volume create: %v", ErrInfra, err)
	}
	if job.Kind == "oci" {
		out, err := run(ctx, "docker", []string{"pull", "--quiet", job.Package}, nil, "")
		if err != nil {
			if infraProblem(out) {
				return "", out, fmt.Errorf("%w: %v", ErrInfra, err)
			}
			return "", out, fmt.Errorf("%w: docker pull: %v", ErrInstall, err)
		}
		return "", out, nil
	}
	proxy, perr := d.sandboxProxy(ctx)
	if perr != nil {
		if d.Runtime != "" {
			// gVisor cannot resolve the proxy by name: retry later instead of
			// recording a failure the package did not cause.
			return "", "", fmt.Errorf("%w: egress proxy: %v", ErrInfra, perr)
		}
		// Plain runc resolves the name through Docker's embedded DNS.
		if d.Log != nil {
			d.Log.Warn("sandbox proxy not resolved, using the configured name", "proxy", d.Proxy, "err", perr)
		}
		proxy = d.Proxy
	}
	start := time.Now()
	out, err := run(ctx, "docker", d.installArgs(job, proxy), nil, "")
	d.info("sandbox install", "job", job.ID, "package", job.Package, "kind", job.Kind, "exit", exitCode(err), "ms", time.Since(start).Milliseconds())
	if err != nil {
		d.forgetProxy() // the proxy may have moved: ask Docker again next time
		if infraProblem(out) || proxyUnreachable(out, proxy, d.Proxy) {
			return "", out, fmt.Errorf("%w: %v: %s", ErrInfra, err, tail(out, 300))
		}
		return "", out, fmt.Errorf("%w: %v", ErrInstall, err)
	}
	var bins []string
	_ = json.Unmarshal([]byte(parseMarker(out, "MCPKG_BINS")), &bins)
	if len(bins) == 0 {
		return "", out, fmt.Errorf("%w: package declares no runnable bin/entry point", ErrInstall)
	}
	return launch.PickBin(job.Package, bins, job.PreferBin, job.EntryPoints), out, nil
}

// Start implements Runner.
func (d *DockerRunner) Start(ctx context.Context, job Job, bin string) (*Process, error) {
	cmd := exec.Command("docker", d.RunArgs(job, bin)...)
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
		return nil, fmt.Errorf("%w: %v", ErrInfra, err)
	}
	name := "mcpkg-run-" + job.ID
	d.info("sandbox start", "job", job.ID, "container", name, "bin", bin)
	started := time.Now()
	shown := bin
	if job.Kind == "oci" {
		shown = "docker run -i " + job.Package
	}
	return &Process{
		Stdin: stdin, Stdout: stdout, stderr: stderr, Display: display(shown, job.Args),
		// Killing the docker CLI does not stop the container; remove it explicitly.
		stop: func() {
			_ = exec.Command("docker", "rm", "-f", name).Run()
			_ = cmd.Process.Kill()
			d.info("sandbox stop", "job", job.ID, "container", name, "ms", time.Since(started).Milliseconds())
		},
		wait: cmd.Wait,
	}, nil
}

// Cleanup implements Runner.
func (d *DockerRunner) Cleanup(job Job) {
	_ = exec.Command("docker", "rm", "-f", "mcpkg-run-"+job.ID, "mcpkg-install-"+job.ID).Run()
	_ = exec.Command("docker", "volume", "rm", "-f", d.volume(job)).Run()
}

// infraProblem recognizes sandbox misconfiguration (not the tool's fault): the job is
// retried later instead of marking the server "install failed".
func infraProblem(out string) bool {
	for _, sig := range []string{"Cannot connect to the Docker daemon", "Unable to find image 'mcpkg-runner", "No such image: mcpkg-runner",
		"unknown or invalid runtime name", "network mcpkg-egress not found", "permission denied while trying to connect to the Docker daemon",
		"no space left on device"} {
		if strings.Contains(out, sig) {
			return true
		}
	}
	return false
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}
