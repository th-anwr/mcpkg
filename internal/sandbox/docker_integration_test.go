package sandbox

import (
	"context"
	"log/slog"
	"net"
	"net/url"
	"os"
	"testing"
	"time"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// TestDockerInstallThroughProxy installs one real npm and one real PyPI
// package in the Docker sandbox, through the egress proxy, with the runtime in
// SANDBOX_RUNTIME ("" = runc, "runsc" = gVisor). It is the only test of the
// sandbox network path: under gVisor a proxy given by name does not resolve
// (issue N22), so the runner must hand the sandbox the proxy's address.
//
// Needs Docker, the runner image, and the proxy image running on the egress
// network under the alias in SANDBOX_PROXY. CI job: sandbox-e2e.
func TestDockerInstallThroughProxy(t *testing.T) {
	if os.Getenv("SANDBOX_DOCKER_TEST") == "" {
		t.Skip("set SANDBOX_DOCKER_TEST=1 (needs Docker, the runner image and the egress proxy)")
	}
	d := &DockerRunner{
		Image:         envOr("SANDBOX_IMAGE", "mcpkg-runner:latest"),
		Runtime:       os.Getenv("SANDBOX_RUNTIME"),
		EgressNetwork: envOr("SANDBOX_EGRESS_NETWORK", "mcpkg-egress"),
		Proxy:         envOr("SANDBOX_PROXY", "http://egress-proxy:3128"),
		Memory:        "1g", CPUs: "1",
		Log: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	proxy, err := d.sandboxProxy(ctx)
	if err != nil {
		t.Fatalf("resolve %s on %s: %v", d.Proxy, d.EgressNetwork, err)
	}
	u, err := url.Parse(proxy)
	if err != nil || net.ParseIP(u.Hostname()) == nil {
		t.Fatalf("sandbox proxy should be an address, got %q (%v)", proxy, err)
	}
	t.Logf("%s resolves to %s on %s", d.Proxy, proxy, d.EgressNetwork)

	// Control: the same install with the proxy by name. Under gVisor this is
	// the N22 failure, and it must be classified as ours, not the package's.
	// Informational under runc, where Docker's embedded DNS answers.
	t.Run("control-by-name", func(t *testing.T) {
		job := Job{ID: "it-ctl", Kind: "npm", Package: "semver", Version: "7.6.3"}
		defer d.Cleanup(job)
		if out, err := run(ctx, "docker", []string{"volume", "create", d.volume(job)}, nil, ""); err != nil {
			t.Fatalf("volume create: %v: %s", err, out)
		}
		out, err := run(ctx, "docker", d.installArgs(job, d.Proxy), nil, "")
		if err == nil {
			t.Logf("by name under %s: install works", d.Name())
			return
		}
		t.Logf("by name under %s: install fails (%v):\n%s", d.Name(), err, tail(out, 600))
		if !proxyUnreachable(out, proxy, d.Proxy) {
			t.Errorf("a failure to resolve the proxy must be reported as infrastructure, not as install_failed")
		}
	})

	for _, job := range []Job{
		{ID: "it-npm", Kind: "npm", Package: "semver", Version: "7.6.3"},
		{ID: "it-pypi", Kind: "pypi", Package: "wheel", Version: "0.45.1"},
	} {
		t.Run(job.Kind, func(t *testing.T) {
			defer d.Cleanup(job)
			bin, out, err := d.Install(ctx, job)
			if err != nil {
				t.Fatalf("install %s under %s: %v\n%s", job.Spec(), d.Name(), err, tail(out, 2000))
			}
			if bin == "" {
				t.Fatalf("install %s: no runnable bin found\n%s", job.Spec(), tail(out, 2000))
			}
			t.Logf("installed %s under %s, bin=%s", job.Spec(), d.Name(), bin)
		})
	}
}
