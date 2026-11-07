package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"

	"github.com/mcpkg/mcpkg/internal/launch"
)

// DockerRunner isolates each smoke test in throwaway containers.
type DockerRunner struct {
	Image         string // mcpkg-runner image (node + python + uv, non-root user)
	Runtime       string // "" or "runsc" (gVisor)
	EgressNetwork string // network whose only exit is the allowlist proxy
	Proxy         string // http://egress-proxy:3128
	Memory        string
	CPUs          string
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

const pypiInstallScript = `set -e
uv venv --quiet /work/venv
uv pip install --quiet --python /work/venv/bin/python "$MCPKG_SPEC"
/work/venv/bin/python -c '
import importlib.metadata as m, json, re, os
norm=lambda s: re.sub(r"[-_.]+","-",s).lower()
want=norm(os.environ["MCPKG_PKG"])
for d in m.distributions():
    if norm(d.metadata["Name"] or "")==want:
        print("MCPKG_BINS="+json.dumps(sorted(e.name for e in d.entry_points if e.group=="console_scripts")))
        break
'`

// InstallArgs builds the `docker run` argv for the install phase (exported for tests).
func (d *DockerRunner) InstallArgs(job Job) []string {
	script := npmInstallScript
	if job.Kind == "pypi" {
		script = pypiInstallScript
	}
	args := []string{"run", "--rm", "--name", "mcpkg-install-" + job.ID,
		"--network", d.EgressNetwork,
		"-e", "HTTPS_PROXY=" + d.Proxy, "-e", "HTTP_PROXY=" + d.Proxy, "-e", "NO_PROXY=localhost",
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
	out, err := run(ctx, "docker", d.InstallArgs(job), nil, "")
	if err != nil {
		if infraProblem(out) {
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
	shown := bin
	if job.Kind == "oci" {
		shown = "docker run -i " + job.Package
	}
	return &Process{
		Stdin: stdin, Stdout: stdout, stderr: stderr, Display: display(shown, job.Args),
		// Killing the docker CLI does not stop the container; remove it explicitly.
		stop: func() { _ = exec.Command("docker", "rm", "-f", name).Run(); _ = cmd.Process.Kill() },
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
