package discover

import (
	"testing"
	"time"

	"github.com/mcpkg/mcpkg/internal/model"
	"github.com/mcpkg/mcpkg/internal/sources"
)

func TestKeysStrongWeak(t *testing.T) {
	c := sources.Candidate{Source: "npm", Name: "@x/y", RepoURL: "https://github.com/o/mono",
		Packages: []model.Package{{RegistryType: "npm", Identifier: "@X/Y"}}}
	keys := Keys(c)
	var npm, repo *Key
	for i := range keys {
		switch keys[i].Kind {
		case "npm":
			npm = &keys[i]
		case "repo":
			repo = &keys[i]
		}
	}
	if npm == nil || npm.Value != "@x/y" || !npm.Strong {
		t.Fatalf("npm key = %+v", npm)
	}
	if repo == nil || repo.Strong {
		t.Fatalf("bare repo key must be weak: %+v", repo)
	}
	c.RepoSubfolder = "packages/y"
	for _, k := range Keys(c) {
		if k.Kind == "repo" && (!k.Strong || k.Value != "github.com/o/mono#packages/y") {
			t.Fatalf("subfolder repo key = %+v", k)
		}
	}
}

func TestComposePrecedence(t *testing.T) {
	pub := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	views := map[string]sourceView{
		"npm": {Name: "@x/y", Description: "npm desc", Version: "1.0.0", RepoURL: "https://github.com/o/r",
			Packages: []model.Package{{RegistryType: "npm", Identifier: "@x/y", Version: "1.0.0"}}, PublishedAt: &pub},
		"registry": {Name: "io.github.o/y", Title: "Y Server", Description: "", Version: "1.1.0", Status: "active",
			Packages: []model.Package{{RegistryType: "npm", Identifier: "@x/y", Version: "1.1.0",
				EnvironmentVariables: []model.EnvVar{{Name: "Y_KEY", IsRequired: true, IsSecret: true}}}}},
		"github": {Name: "o/r", Stars: 42},
	}
	c := compose(views)
	if c.Name != "io.github.o/y" || c.Title != "Y Server" {
		t.Errorf("registry should win name/title: %+v", c)
	}
	if c.Description != "npm desc" {
		t.Errorf("empty registry description should fall through to npm: %q", c.Description)
	}
	if c.Version != "1.1.0" || len(c.Packages) != 1 || len(c.Packages[0].EnvironmentVariables) != 1 {
		t.Errorf("registry package (with env vars) should win: %+v", c.Packages)
	}
	if c.Stars != 42 || c.Status != "active" || c.PublishedAt == nil {
		t.Errorf("stars/status/published not merged: %+v", c)
	}
}

func TestMaterialChange(t *testing.T) {
	a := &composed{Version: "1", Packages: []model.Package{{RegistryType: "npm", Identifier: "x"}}}
	b := &composed{Version: "1", Packages: []model.Package{{RegistryType: "npm", Identifier: "x"}}, Description: "changed"}
	if a.materialChange(b) {
		t.Error("description-only change should not trigger re-checks")
	}
	b.Version = "2"
	if !a.materialChange(b) {
		t.Error("version change must trigger re-checks")
	}
}
