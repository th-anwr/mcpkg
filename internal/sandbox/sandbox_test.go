package sandbox

import (
	"context"
	"net/url"
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

// Shapes copied from `docker inspect --format '{{json .NetworkSettings.Networks}}'`:
// the compose proxy (two networks), an install container, and noise on stderr.
const inspectFixture = `WARNING: something on stderr
{"mcpkg-egress":{"IPAMConfig":null,"Links":null,"Aliases":["mcpkg-egress-proxy-1","egress-proxy"],"MacAddress":"02:42:ac:12:00:02","NetworkID":"n1","EndpointID":"e1","Gateway":"","IPAddress":"172.18.0.2","IPPrefixLen":16,"DNSNames":["mcpkg-egress-proxy-1","egress-proxy","3f2a1b4c5d6e"]},"mcpkg_default":{"Aliases":["mcpkg-egress-proxy-1","egress-proxy"],"IPAddress":"172.19.0.3","DNSNames":["mcpkg-egress-proxy-1","egress-proxy"]}}
{"mcpkg-egress":{"Aliases":null,"IPAddress":"172.18.0.5","DNSNames":["mcpkg-install-7-1","aa11bb22cc33"]}}
not json
`

func TestProxyIP(t *testing.T) {
	// The address must be the one on the egress network, not on the default network.
	if got := proxyIP(inspectFixture, "mcpkg-egress", "egress-proxy"); got != "172.18.0.2" {
		t.Errorf("proxy address on the egress network: got %q", got)
	}
	if got := proxyIP(inspectFixture, "mcpkg-egress", "Egress-Proxy"); got != "172.18.0.2" {
		t.Errorf("names are case-insensitive: got %q", got)
	}
	if got := proxyIP(inspectFixture, "mcpkg-egress", "squid"); got != "" {
		t.Errorf("unknown name must not match: got %q", got)
	}
	if got := proxyIP(inspectFixture, "other-net", "egress-proxy"); got != "" {
		t.Errorf("unknown network must not match: got %q", got)
	}
	// Docker older than 25 has no DNSNames; the compose alias is enough.
	old := `{"mcpkg-egress":{"Aliases":["egress-proxy","3f2a1b4c5d6e"],"IPAddress":"10.9.0.7"}}`
	if got := proxyIP(old, "mcpkg-egress", "egress-proxy"); got != "10.9.0.7" {
		t.Errorf("alias-only inspect output: got %q", got)
	}
	if got := proxyIP(`{"mcpkg-egress":{"Aliases":["egress-proxy"],"IPAddress":""}}`, "mcpkg-egress", "egress-proxy"); got != "" {
		t.Errorf("a container without an address must not match: got %q", got)
	}
}

func TestSandboxProxyURL(t *testing.T) {
	for in, want := range map[string]string{
		"http://egress-proxy:3128":     "http://172.18.0.2:3128",
		"http://egress-proxy":          "http://172.18.0.2",
		"http://u:p@egress-proxy:3128": "http://u:p@172.18.0.2:3128",
	} {
		u, err := url.Parse(in)
		if err != nil {
			t.Fatal(err)
		}
		if got := withHost(u, "172.18.0.2"); got != want {
			t.Errorf("withHost(%s) = %s, want %s", in, got, want)
		}
	}
	u, err := url.Parse("http://egress-proxy:3128")
	if err != nil {
		t.Fatal(err)
	}
	if got := withHost(u, "fd00::2"); got != "http://[fd00::2]:3128" {
		t.Errorf("IPv6 address: got %s", got)
	}

	// An address literal needs no lookup (and no Docker daemon).
	d := &DockerRunner{Image: "mcpkg-runner:latest", Runtime: "runsc", EgressNetwork: "mcpkg-egress", Proxy: "http://172.18.0.2:3128", Memory: "1g", CPUs: "1"}
	got, err := d.sandboxProxy(context.Background())
	if err != nil || got != "http://172.18.0.2:3128" {
		t.Errorf("literal proxy: got %q, %v", got, err)
	}
	if _, err := (&DockerRunner{Proxy: "egress-proxy:3128"}).sandboxProxy(context.Background()); err == nil {
		t.Error("a proxy without a scheme must be rejected")
	}

	// The sandbox gets the resolved address in both proxy variables, never the name.
	d.Proxy = "http://egress-proxy:3128"
	install := strings.Join(d.installArgs(Job{ID: "7-1", Kind: "npm", Package: "y", Version: "1"}, "http://172.18.0.2:3128"), " ")
	for _, want := range []string{"HTTPS_PROXY=http://172.18.0.2:3128", "HTTP_PROXY=http://172.18.0.2:3128", "--runtime runsc", "--network mcpkg-egress"} {
		if !strings.Contains(install, want) {
			t.Errorf("install args missing %q:\n%s", want, install)
		}
	}
	if strings.Contains(install, "egress-proxy") {
		t.Errorf("install args still name the proxy:\n%s", install)
	}
}

func TestProxyUnreachable(t *testing.T) {
	name, addr := "http://egress-proxy:3128", "http://172.18.0.2:3128"
	for _, out := range []string{
		"npm error request to https://registry.npmjs.org/exa-mcp-server failed, reason: getaddrinfo EAI_AGAIN egress-proxy",
		"npm error connect ECONNREFUSED 172.18.0.2:3128",
		"npm error connect ETIMEDOUT 172.18.0.2:3128",
	} {
		if !proxyUnreachable(out, addr, name) {
			t.Errorf("should be an infrastructure problem: %s", out)
		}
	}
	for _, out := range []string{
		"npm error 404 Not Found - GET https://registry.npmjs.org/nope",
		"npm error getaddrinfo EAI_AGAIN downloads.example.com", // the package's own download: its problem
		"npm error connect ECONNREFUSED 127.0.0.1:9000",
	} {
		if proxyUnreachable(out, addr, name) {
			t.Errorf("should count against the package: %s", out)
		}
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
