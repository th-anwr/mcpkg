package stages

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/mcpkg/mcpkg/internal/queue"
)

// fakeGitHub answers like the GraphQL batch call: one entry per request, an
// empty body (a *json.SyntaxError) when told to, or another error.
type fakeGitHub struct {
	calls      int
	emptyFirst int             // the first N calls get an empty answer
	poison     map[string]bool // batches containing these repos always get an empty answer
	otherErr   error           // returned for every call when set
}

func (f *fakeGitHub) query(_ context.Context, reqs []GHRequest) (map[string]*ghRepo, error) {
	f.calls++
	if f.otherErr != nil {
		return nil, f.otherErr
	}
	empty := json.Unmarshal(nil, &struct{}{}) // "unexpected end of JSON input"
	if f.calls <= f.emptyFirst {
		return nil, empty
	}
	for _, r := range reqs {
		if f.poison[r.Slug] {
			return nil, empty
		}
	}
	out := map[string]*ghRepo{}
	for _, r := range reqs {
		out[r.key()] = &ghRepo{NameWithOwner: r.Slug}
	}
	return out, nil
}

func batchOf(n int) []GHRequest {
	reqs := make([]GHRequest, n)
	for i := range reqs {
		reqs[i] = GHRequest{Slug: fmt.Sprintf("owner/repo%02d", i)}
	}
	return reqs
}

func TestQueryWithSplit(t *testing.T) {
	ctx := context.Background()

	t.Run("one empty answer is retried", func(t *testing.T) {
		f := &fakeGitHub{emptyFirst: 1}
		repos, failed := queryWithSplit(ctx, batchOf(25), f.query)
		if len(repos) != 25 || len(failed) != 0 {
			t.Fatalf("got %d repos, %d failed; want 25 and 0", len(repos), len(failed))
		}
		if f.calls != 2 {
			t.Errorf("calls = %d, want 2 (one retry)", f.calls)
		}
	})

	t.Run("one bad repository fails only itself", func(t *testing.T) {
		reqs := batchOf(25)
		bad := reqs[17]
		f := &fakeGitHub{poison: map[string]bool{bad.Slug: true}}
		repos, failed := queryWithSplit(ctx, reqs, f.query)
		if len(repos) != 24 || len(failed) != 1 || failed[bad.key()] == nil {
			t.Fatalf("got %d repos, failed=%v; want 24 repos and only %s failed", len(repos), failed, bad.Slug)
		}
		if !emptyGraphQL(failed[bad.key()]) {
			t.Errorf("the failed repository should keep the empty-answer error, got %v", failed[bad.key()])
		}
		if f.calls > 25 {
			t.Errorf("calls = %d; splitting should need far fewer than one call per repository", f.calls)
		}
	})

	t.Run("other errors fail the batch without splitting", func(t *testing.T) {
		f := &fakeGitHub{otherErr: errors.New("HTTP 502 from api.github.com")}
		repos, failed := queryWithSplit(ctx, batchOf(25), f.query)
		if len(repos) != 0 || len(failed) != 25 {
			t.Fatalf("got %d repos, %d failed; want 0 and 25", len(repos), len(failed))
		}
		if f.calls != 1 {
			t.Errorf("calls = %d, want 1: only empty answers are retried or split", f.calls)
		}
	})

	t.Run("rate limits are kept for the caller", func(t *testing.T) {
		f := &fakeGitHub{otherErr: &queue.ErrRateLimited{}}
		_, failed := queryWithSplit(ctx, batchOf(3), f.query)
		for k, err := range failed {
			if _, ok := queue.AsRateLimited(err); !ok {
				t.Errorf("%s: want the rate-limit error, got %v", k, err)
			}
		}
		if f.calls != 1 {
			t.Errorf("calls = %d, want 1", f.calls)
		}
	})
}
