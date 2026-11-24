package e2e

import (
	"context"
	"strings"
	"testing"

	"github.com/mcpkg/mcpkg/internal/discover"
	"github.com/mcpkg/mcpkg/internal/model"
	"github.com/mcpkg/mcpkg/internal/queue"
	"github.com/mcpkg/mcpkg/internal/sources"
	"github.com/mcpkg/mcpkg/internal/store"
)

// N31: some READMEs on GitHub contain NUL bytes. Postgres rejected the whole
// save ("invalid byte sequence for encoding UTF8: 0x00"), so the health job
// died after its retries. Saving must drop NULs and replace invalid UTF-8.
func TestSaveStageCleansText(t *testing.T) {
	conn := testDB(t)
	ctx := context.Background()
	st := store.New(conn)
	d := &discover.Discoverer{Store: st, Queue: queue.New(conn), EntryStage: "health", Log: quietLog()}
	r, err := d.Upsert(ctx, sources.Candidate{Source: "pypi", Name: "acme-nul", Version: "0.1", RepoURL: "https://github.com/acme/nul",
		Packages: []model.Package{{RegistryType: "pypi", Identifier: "acme-nul", Version: "0.1"}}})
	if err != nil {
		t.Fatal(err)
	}

	err = st.SaveStage(ctx, store.StageRecord{ToolID: r.ToolID, Stage: "health", Decision: "pass",
		Notes:  []string{"stderr: a\x00b"},
		Result: map[string]any{"text": "c\x00d", "literal": `\u0000`, "n": uint64(12345678901234567890)},
		Fields: map[string]any{
			"readme":      "# Title\x00\x00 body",
			"description": "bad \xff byte",
			"categories":  []string{"dev\x00tools"},
		}})
	if err != nil {
		t.Fatalf("SaveStage with NUL bytes and invalid UTF-8: %v", err)
	}

	tool, err := st.GetTool(ctx, r.ToolID)
	if err != nil {
		t.Fatal(err)
	}
	if tool.Readme != "# Title body" {
		t.Errorf("readme = %q, want NUL bytes dropped", tool.Readme)
	}
	if tool.Description != "bad � byte" {
		t.Errorf("description = %q, want the invalid byte replaced", tool.Description)
	}
	if len(tool.Categories) != 1 || tool.Categories[0] != "devtools" {
		t.Errorf("categories = %q", tool.Categories)
	}

	var result, notes string
	if err := conn.QueryRowContext(ctx, `SELECT result::text, array_to_string(notes, '|') FROM check_runs
		WHERE tool_id = $1 AND stage = 'health' ORDER BY id DESC LIMIT 1`, r.ToolID).Scan(&result, &notes); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"text": "cd"`, `"literal": "\\u0000"`, `"n": 12345678901234567890`} {
		if !strings.Contains(result, want) {
			t.Errorf("check_runs.result = %s, want it to contain %s", result, want)
		}
	}
	if notes != "stderr: ab" {
		t.Errorf("check_runs.notes = %q", notes)
	}
}
