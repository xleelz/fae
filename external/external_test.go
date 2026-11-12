package external

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func fixture() Tool {
	return Tool{Endpoint: "http://127.0.0.1:1", Server: "fixture", Name: "review_create", Role: "orchestrator", Arguments: map[string]string{"draft": "true", "branch": "fae/fixture", "destination": "main"}}
}
func TestExactNamesAndPins(t *testing.T) {
	for _, name := range []string{"review_create", "mcp__fixture__review_create"} {
		if _, e := CanonicalTool("fixture", "review_create", name); e != nil {
			t.Fatal(e)
		}
	}
	for _, name := range []string{"mcp__other__review_create", "review_create_more", "mcp__fixture__other", "*"} {
		if _, e := CanonicalTool("fixture", "review_create", name); e == nil {
			t.Fatal(name)
		}
	}
	tool := fixture()
	i, e := NewIntent(tool, "task", "stage", "key", "orchestrator", "tree", "session", tool.Arguments)
	if e != nil {
		t.Fatal(e)
	}
	other, _ := NewIntent(tool, "task", "stage", "key", "orchestrator", "tree", "second-session", tool.Arguments)
	if i.Key != other.Key {
		t.Fatal("key depends on session")
	}
	other, _ = NewIntent(tool, "other-task", "stage", "key", "orchestrator", "tree", "session", tool.Arguments)
	if i.Key == other.Key {
		t.Fatal("task collision")
	}
	for _, mutate := range []func(*Tool){func(x *Tool) { x.Endpoint = "http://remote.example" }, func(x *Tool) { x.Role = "*" }, func(x *Tool) { x.Arguments["draft"] = "false" }, func(x *Tool) { x.Arguments["auto_publish"] = "true" }, func(x *Tool) { x.Endpoint = "https://user:password@example.org" }} {
		bad := fixture()
		mutate(&bad)
		if bad.Validate() == nil {
			t.Fatal("accepted invalid config", bad)
		}
	}
	if _, e = NewIntent(tool, "task", "stage", "key", "other", "tree", "session", tool.Arguments); e == nil {
		t.Fatal("role widened")
	}
	if _, e = NewIntent(tool, "task", "stage", "key", "orchestrator", "tree", "session", map[string]string{"draft": "true"}); e == nil {
		t.Fatal("pins changed")
	}
}
func TestReceiptGuard(t *testing.T) {
	tool := fixture()
	i, _ := NewIntent(tool, "task", "stage", "key", tool.Role, "tree", "session", tool.Arguments)
	r := Receipt{Key: i.Key, Task: i.Task, Tool: i.Tool, Digest: i.Digest, ExternalID: "review-1", State: "performed", Tree: i.Tree}
	if e := Match(tool, i, r); e != nil {
		t.Fatal(e)
	}
	for _, change := range []func(*Receipt){func(r *Receipt) { r.Key = "other" }, func(r *Receipt) { r.Task = "other" }, func(r *Receipt) { r.Tool = "mcp__other__review_create" }, func(r *Receipt) { r.Digest = "other" }, func(r *Receipt) { r.ExternalID = "" }, func(r *Receipt) { r.State = "pending" }, func(r *Receipt) { r.Tree = "other" }} {
		bad := r
		change(&bad)
		if Match(tool, i, bad) == nil {
			t.Fatal("accepted receipt", bad)
		}
	}
	for bits := 0; bits < 16; bits++ {
		if CanAdopt(bits&1 != 0, bits&2 != 0, bits&4 != 0, bits&8 != 0) != (bits == 15) {
			t.Fatal(bits)
		}
	}
}
func TestHTTPQueryAndNoRetry(t *testing.T) {
	calls := 0
	var saved *Receipt
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			if saved == nil {
				w.WriteHeader(404)
				return
			}
			json.NewEncoder(w).Encode(saved)
			return
		}
		calls++
		var i Intent
		json.NewDecoder(r.Body).Decode(&i)
		saved = &Receipt{Key: i.Key, Task: i.Task, Tool: i.Tool, Digest: i.Digest, ExternalID: "review-1", State: "performed", Tree: i.Tree}
		json.NewEncoder(w).Encode(saved)
	}))
	defer server.Close()
	tool := fixture()
	tool.Endpoint = server.URL
	i, _ := NewIntent(tool, "task", "stage", "key", tool.Role, "tree", "session", tool.Arguments)
	if _, found, e := Query(context.Background(), tool, i); e != nil || found {
		t.Fatal(found, e)
	}
	r, e := Perform(context.Background(), tool, i)
	if e != nil || Match(tool, i, r) != nil {
		t.Fatal(r, e)
	}
	r, found, e := Query(context.Background(), tool, i)
	if e != nil || !found || Match(tool, i, r) != nil || calls != 1 {
		t.Fatal(r, found, e, calls)
	}
}
func TestHTTPRejectsRedirectAndMalformed(t *testing.T) {
	for _, body := range []string{"redirect", "{\"external_id\":\"retained\",\"unknown\":true}", "{} {}"} {
		t.Run(body, func(t *testing.T) {
			calls := 0
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if body == "redirect" {
					http.Redirect(w, r, "/different", 302)
					return
				}
				w.Write([]byte(body))
			}))
			defer s.Close()
			tool := fixture()
			tool.Endpoint = s.URL
			i, _ := NewIntent(tool, "task", "stage", "key", tool.Role, "tree", "session", tool.Arguments)
			r, e := Perform(context.Background(), tool, i)
			if e == nil || calls != 1 {
				t.Fatal(e, calls)
			}
			if body != "redirect" && body != "{} {}" && r.ExternalID != "retained" {
				t.Fatal("lost reported id")
			}
		})
	}
}

func TestReviewAdoptionStableAcrossRevisionAndCommit(t *testing.T) {
	tool := fixture()
	a, e := NewIntent(tool, "task", "workflow-v1/review", "old-key", tool.Role, "same-tree", "first", tool.Arguments)
	if e != nil {
		t.Fatal(e)
	}
	b, e := NewIntent(tool, "task", "workflow-v2/renamed-review", "new-key", tool.Role, "same-tree", "second", tool.Arguments)
	if e != nil {
		t.Fatal(e)
	}
	if a.Key != b.Key {
		t.Fatal("review key depends on workflow revision")
	}
	receipt := Receipt{Key: a.Key, Task: a.Task, Tool: a.Tool, Digest: a.Digest, Tree: a.Tree, State: "performed", ExternalID: "existing-review", CommitSHA: "different-commit-with-identical-tree"}
	if e = Match(tool, b, receipt); e != nil {
		t.Fatal("same tree review not adopted", e)
	}
	tool.Arguments["branch"] = "another-branch"
	c, _ := NewIntent(tool, "task", "workflow-v1/review", "old-key", tool.Role, "same-tree", "first", tool.Arguments)
	if c.Key == a.Key {
		t.Fatal("branch collision")
	}
	ticket := fixture()
	ticket.Name = "ticket_create"
	a, _ = NewIntent(ticket, "task", "v1/stage", "outbox-1", ticket.Role, "tree", "s1", ticket.Arguments)
	b, _ = NewIntent(ticket, "task", "v2/stage", "outbox-1", ticket.Role, "tree", "s2", ticket.Arguments)
	if a.Key != b.Key {
		t.Fatal("ticket key not stable")
	}
	c, _ = NewIntent(ticket, "task", "v2/stage", "outbox-2", ticket.Role, "tree", "s2", ticket.Arguments)
	if a.Key == c.Key {
		t.Fatal("outbox collision")
	}
}

func TestLostPOSTResponseNeverAutomaticallyReplayed(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.WriteHeader(404)
			return
		}
		calls.Add(1)
		conn, _, e := w.(http.Hijacker).Hijack()
		if e != nil {
			t.Error(e)
			return
		}
		conn.Close()
	}))
	defer server.Close()
	tool := fixture()
	tool.Endpoint = server.URL
	i, _ := NewIntent(tool, "task", "stage", "key", tool.Role, "tree", "session", tool.Arguments)
	if _, _, e := Query(context.Background(), tool, i); e != nil {
		t.Fatal(e)
	}
	if _, e := Perform(context.Background(), tool, i); e == nil {
		t.Fatal("lost response not uncertain")
	}
	if calls.Load() != 1 {
		t.Fatal("automatically retried mutation", calls.Load())
	}
	changed := tool
	changed.Endpoint = "http://127.0.0.1:1"
	if _, e := Perform(context.Background(), changed, i); e == nil {
		t.Fatal("changed endpoint accepted")
	}
}

// Appendix B ExternalEffectRefused: a definitive client-error response to a
// performed call is a typed refusal carrying the reported id; timeouts, rate
// limits, server errors and query responses stay untyped (uncertain).
func TestPerformRefusalIsTyped(t *testing.T) {
	for status, refused := range map[int]bool{403: true, 422: true, 409: true, 408: false, 429: false, 500: false, 503: false} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			json.NewEncoder(w).Encode(Receipt{ExternalID: "reported-1", State: "refused"})
		}))
		tool := fixture()
		tool.Endpoint = s.URL
		i, _ := NewIntent(tool, "task", "stage", "key", tool.Role, "tree", "session", tool.Arguments)
		_, e := Perform(context.Background(), tool, i)
		var typed *StatusError
		if !errors.As(e, &typed) || typed.Refused() != refused || typed.ExternalID != "reported-1" || !strings.Contains(e.Error(), "reported id reported-1") {
			t.Error(status, e)
		}
		if _, _, e = Query(context.Background(), tool, i); !errors.As(e, &typed) || typed.Refused() {
			t.Error("query response typed as a refused call", status, e)
		}
		s.Close()
	}
}
