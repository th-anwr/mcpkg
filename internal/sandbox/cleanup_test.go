package sandbox

import (
	"strings"
	"testing"
)

// TestCleanupRemovesOCIImage: every smoke test of an OCI server pulls that
// server's image; Cleanup must remove it or the worker's disk fills (N28).
func TestCleanupRemovesOCIImage(t *testing.T) {
	d := &DockerRunner{Image: "ghcr.io/th-anwr/mcpkg-runner:v0.1.4"}
	removesImage := func(job Job) bool {
		for _, c := range d.cleanupCommands(job) {
			if strings.Join(c[:2], " ") == "image rm" {
				if c[len(c)-1] != job.Package {
					t.Fatalf("removes %q, want the job's image %q", c[len(c)-1], job.Package)
				}
				return true
			}
		}
		return false
	}
	cases := []struct {
		job  Job
		want bool
	}{
		{Job{ID: "1", Kind: "oci", Package: "docker.io/acme/mcp-server:1.2.0"}, true},
		{Job{ID: "2", Kind: "npm", Package: "semver", Version: "7.6.3"}, false},
		{Job{ID: "3", Kind: "pypi", Package: "wheel", Version: "0.45.1"}, false},
		{Job{ID: "4", Kind: "oci", Package: "ghcr.io/th-anwr/mcpkg-runner:v0.1.4"}, false}, // never the runner image
		{Job{ID: "5", Kind: "oci", Package: ""}, false},
	}
	for _, c := range cases {
		if got := removesImage(c.job); got != c.want {
			t.Errorf("%s %q: removes image = %v, want %v", c.job.Kind, c.job.Package, got, c.want)
		}
		cmds := d.cleanupCommands(c.job)
		if len(cmds) < 2 || cmds[0][0] != "rm" || cmds[1][0] != "volume" {
			t.Errorf("containers and volume must always be removed: %v", cmds)
		}
	}
	for _, c := range d.cleanupCommands(Job{ID: "1", Kind: "oci", Package: "docker.io/acme/mcp-server:1.2.0"}) {
		if c[0] == "image" {
			for _, a := range c {
				if a == "-f" || a == "--force" {
					t.Error("image removal must not force: an image still in use by another job has to stay")
				}
			}
		}
	}
}
