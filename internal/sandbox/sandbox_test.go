package sandbox

import (
	"strings"
	"testing"
)

func TestDockerArgsAreHardened(t *testing.T) {
	d := &DockerRunner{Image: "mcpkg-runner:latest", Runtime: "runsc", EgressNetwork: "mcpkg-egress", Proxy: "http://egress-proxy:3128", Memory: "1g", CPUs: "1"}
	job := Job{ID: "7-1", Kind: "npm", Package: "@x/y", Version: "1.0.0", Args: []string{"/tmp/mcpkg-smoke"}, Env: map[string]string{"B": "2", "A": "1"}}
	install := strings.Join(d.InstallArgs(job), " ")
	for _, want := range []string{"--network mcpkg-egress", "HTTPS_PROXY=http://egress-proxy:3128", "--cap-drop ALL", "no-new-privileges", "--runtime runsc", "MCPKG_SPEC=@x/y@1.0.0", "-v mcpkg-smoke-7-1:/work"} {
		if !strings.Contains(install, want) {
			t.Errorf("install args missing %q:\n%s", want, install)
		}
	}
	run := d.RunArgs(job, "y")
	joined := strings.Join(run, " ")
	for _, want := range []string{"--network none", "--read-only", "--cap-drop ALL", "--pids-limit 256", "mcpkg-smoke-7-1:/work:ro", "/work/app/node_modules/.bin/y /tmp/mcpkg-smoke"} {
		if !strings.Contains(joined, want) {
			t.Errorf("run args missing %q:\n%s", want, joined)
		}
	}
	if strings.Index(joined, "A=1") > strings.Index(joined, "B=2") {
		t.Error("env should be sorted for reproducible commands")
	}
	if strings.Contains(joined, "HTTPS_PROXY") {
		t.Error("run phase must not get network/proxy settings")
	}
	py := strings.Join(d.RunArgs(Job{ID: "1", Kind: "pypi", Package: "p"}, "p-cli"), " ")
	if !strings.Contains(py, "/work/venv/bin/p-cli") {
		t.Errorf("pypi run path: %s", py)
	}
}

func TestSpec(t *testing.T) {
	if s := (Job{Kind: "npm", Package: "a", Version: "1"}).Spec(); s != "a@1" {
		t.Error(s)
	}
	if s := (Job{Kind: "pypi", Package: "a", Version: "1"}).Spec(); s != "a==1" {
		t.Error(s)
	}
}

func TestLimitedBuffer(t *testing.T) {
	b := NewLimitedBuffer(5)
	_, _ = b.Write([]byte("hello world"))
	if b.String() != "world" {
		t.Errorf("got %q", b.String())
	}
}

func TestParseMarker(t *testing.T) {
	out := "npm WARN x\nMCPKG_BINS=[\"a\",\"b\"]\n"
	if got := parseMarker(out, "MCPKG_BINS"); got != `["a","b"]` {
		t.Errorf("got %q", got)
	}
}
