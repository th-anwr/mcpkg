package config

import "testing"

func TestValidateRefusesDefaultDBInProduction(t *testing.T) {
	c := Config{SiteURL: "https://example.com", DatabaseURL: DefaultDatabaseURL}
	if c.Validate() == nil {
		t.Fatal("https site with the default database URL should be refused")
	}
	c.SiteURL = "http://localhost:8080"
	if err := c.Validate(); err != nil {
		t.Fatalf("dev config refused: %v", err)
	}
	c.SiteURL, c.DatabaseURL = "https://example.com", "postgres://mcpkg:x@db:5432/mcpkg"
	if err := c.Validate(); err != nil {
		t.Fatalf("production config refused: %v", err)
	}
}
