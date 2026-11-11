package stages

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/mcpkg/mcpkg/internal/config"
	"github.com/mcpkg/mcpkg/internal/httpx"
)

// fakeNpm answers like api.npmjs.org: a comma list gets the bulk shape (one
// entry per name, null when unknown), a single name gets the plain shape.
// Downloads are 10 × the name length so results are checkable.
func fakeNpm(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		names := strings.TrimPrefix(r.URL.Path, "/downloads/point/last-week/")
		if !strings.Contains(names, ",") {
			fmt.Fprintf(w, `{"downloads":%d,"package":%q}`, 10*len(names), names)
			return
		}
		var parts []string
		for _, n := range strings.Split(names, ",") {
			if n == "ghost" {
				parts = append(parts, `"ghost":null`)
				continue
			}
			parts = append(parts, strconv.Quote(n)+`:{"downloads":`+strconv.Itoa(10*len(n))+`,"package":`+strconv.Quote(n)+`}`)
		}
		_, _ = w.Write([]byte("{" + strings.Join(parts, ",") + "}"))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), paths...)
	}
}

func TestNpmDownloadsBatch(t *testing.T) {
	srv, requests := fakeNpm(t)
	h := &Health{D: &Deps{Cfg: config.Config{NpmAPI: srv.URL}, HTTP: httpx.New("mcpkg-test")}}

	got := h.npmDownloadsBatch(context.Background(), []string{"ab", "abc", "ghost", "@scope/pkg"})
	want := map[string]int64{"ab": 20, "abc": 30, "@scope/pkg": 100}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: got %d, want %d", k, got[k], v)
		}
	}
	if _, ok := got["ghost"]; ok {
		t.Error("unknown package must stay unknown, not 0")
	}
	if r := requests(); len(r) != 2 {
		t.Errorf("want 1 bulk + 1 scoped request, got %d: %v", len(r), r)
	}
}

func TestNpmDownloadsBatchChunks(t *testing.T) {
	srv, requests := fakeNpm(t)
	h := &Health{D: &Deps{Cfg: config.Config{NpmAPI: srv.URL}, HTTP: httpx.New("mcpkg-test")}}

	pkgs := make([]string, npmBulkMax+1) // one full bulk request, then a single leftover name
	for i := range pkgs {
		pkgs[i] = fmt.Sprintf("p%03d", i)
	}
	got := h.npmDownloadsBatch(context.Background(), pkgs)
	if len(got) != len(pkgs) {
		t.Fatalf("got %d results, want %d", len(got), len(pkgs))
	}
	for _, p := range pkgs {
		if got[p] != 40 {
			t.Fatalf("%s: got %d, want 40", p, got[p])
		}
	}
	r := requests()
	if len(r) != 2 {
		t.Fatalf("want 2 requests, got %d", len(r))
	}
	if strings.Count(r[0], ",") != npmBulkMax-1 || strings.Contains(r[1], ",") {
		t.Errorf("want a %d-name bulk request then a single-name request, got %q and %q", npmBulkMax, r[0][:40], r[1])
	}

	if got := h.npmDownloadsBatch(context.Background(), nil); len(got) != 0 {
		t.Errorf("no packages: got %v", got)
	}
}
