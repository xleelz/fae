package tracker

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/xleelz/fae/symphony"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func linearConnection(nodes any, more bool, cursor any) map[string]any {
	return map[string]any{"nodes": nodes, "pageInfo": map[string]any{"hasNextPage": more, "endCursor": cursor}}
}
func linearCompleteIssue() map[string]any {
	return map[string]any{"id": "candidate", "identifier": "ENG-1", "title": "Candidate", "state": map[string]any{"name": "Todo", "type": "unstarted"}, "team": map[string]any{"id": "team", "key": "ENG"}, "project": map[string]any{"id": "our-project", "slugId": "ours"}, "labels": linearConnection([]any{}, false, nil), "children": linearConnection([]any{}, false, nil), "inverseRelations": linearConnection([]any{}, false, nil)}
}
func linearBlocker(id, name, typ string) map[string]any {
	return map[string]any{"type": "blocks", "issue": map[string]any{"id": id, "identifier": "OTHER-" + id, "state": map[string]any{"name": name, "type": typ}}}
}
func issueListResponse(issue map[string]any) map[string]any {
	return map[string]any{"data": map[string]any{"issues": map[string]any{"nodes": []any{issue}, "pageInfo": map[string]any{"hasNextPage": false}}}}
}
func newConnectionsAdapter(t *testing.T, handler http.HandlerFunc) (*LinearAdapter, func()) {
	t.Helper()
	srv := httptest.NewServer(handler)
	a, err := NewLinearAdapter(symphony.TrackerConfig{Kind: "linear", Provider: map[string]any{"endpoint": srv.URL, "api_key": "lin_private_token", "project_id": "our-project"}}, nil, srv.Client())
	if err != nil {
		srv.Close()
		t.Fatal(err)
	}
	return a, srv.Close
}
func TestLinearIncomingBlockersBeyondFirstPageAndOtherProjectTerminal(t *testing.T) {
	for _, active := range []bool{true, false} {
		t.Run(fmt.Sprint(active), func(t *testing.T) {
			issue := linearCompleteIssue()
			initial := []any{}
			for i := 0; i < 50; i++ {
				initial = append(initial, linearBlocker(fmt.Sprint(i), "Released", "completed"))
			}
			issue["inverseRelations"] = linearConnection(initial, true, "blocker-page-1")
			// Outgoing blocks point away from the candidate and must never hold it.
			issue["relations"] = linearConnection([]any{map[string]any{"type": "blocks", "relatedIssue": map[string]any{"id": "outgoing", "state": map[string]any{"name": "In Progress", "type": "started"}}}}, false, nil)
			pages := 0
			a, close := newConnectionsAdapter(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "lin_private_token" {
					t.Error("missing credential")
				}
				var req linearGraphQLRequest
				_ = json.NewDecoder(r.Body).Decode(&req)
				if strings.Contains(req.Query, "FetchIssueConnection") {
					pages++
					if req.Variables["id"] != "candidate" || req.Variables["after"] != "blocker-page-1" {
						t.Error(req.Variables)
					}
					if _, filtered := req.Variables["filter"]; filtered {
						t.Error("external blockers filtered by candidate project")
					}
					name, typ := "Released", "completed"
					if active {
						name, typ = "In Progress", "started"
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"issue": map[string]any{"id": "candidate", "inverseRelations": linearConnection([]any{linearBlocker("last", name, typ)}, false, nil)}}})
					return
				}
				_ = json.NewEncoder(w).Encode(issueListResponse(issue))
			})
			defer close()
			issues, err := a.FetchCandidateIssues(context.Background())
			if err != nil || len(issues) != 1 {
				t.Fatal(issues, err)
			}
			if pages != 1 || len(issues[0].BlockedBy) != 51 || issues[0].Dispatchable == active {
				t.Fatal(pages, issues[0])
			}
			first := issues[0].BlockedBy[0]
			if first.State == nil || *first.State != "Released" || first.Terminal == nil || !*first.Terminal {
				t.Fatal("provider spelling or terminal type lost", first)
			}
		})
	}
}
func TestLinearPaginatedLabelsAndChildrenComplete(t *testing.T) {
	issue := linearCompleteIssue()
	issue["labels"] = linearConnection([]any{map[string]any{"name": "First"}}, true, "labels-1")
	issue["children"] = linearConnection([]any{map[string]any{"id": "c1", "identifier": "ENG-2", "title": "one", "state": map[string]any{"name": "Done"}}}, true, "children-1")
	a, close := newConnectionsAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		var req linearGraphQLRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if strings.Contains(req.Query, "FetchIssueConnection") {
			field := "children"
			nodes := []any{map[string]any{"id": "c2", "identifier": "ENG-3", "title": "two", "state": map[string]any{"name": "Todo"}}}
			if strings.Contains(req.Query, "labels(first:") {
				field = "labels"
				nodes = []any{map[string]any{"name": "Second"}}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"issue": map[string]any{"id": "candidate", field: linearConnection(nodes, false, nil)}}})
			return
		}
		_ = json.NewEncoder(w).Encode(issueListResponse(issue))
	})
	defer close()
	issues, err := a.FetchTrackerIssuesByStates(context.Background(), []string{"Todo"})
	if err != nil || len(issues) != 1 || len(issues[0].Labels) != 2 || len(issues[0].Children) != 2 || !issues[0].Dispatchable {
		t.Fatal(issues, err)
	}
}
func TestLinearMalformedUnknownAndMissingConnectionNeverDispatch(t *testing.T) {
	for _, kind := range []string{"nil-blocker", "missing-state", "missing-id", "unknown-relation", "malformed-connection", "missing-page-info"} {
		t.Run(kind, func(t *testing.T) {
			issue := linearCompleteIssue()
			switch kind {
			case "nil-blocker":
				issue["inverseRelations"] = linearConnection([]any{map[string]any{"type": "blocks", "issue": nil}}, false, nil)
			case "missing-state":
				issue["inverseRelations"] = linearConnection([]any{linearBlocker("unknown", "", "")}, false, nil)
			case "missing-id":
				issue["inverseRelations"] = linearConnection([]any{linearBlocker("", "Done", "completed")}, false, nil)
			case "unknown-relation":
				issue["inverseRelations"] = linearConnection([]any{map[string]any{"type": "new-provider-kind"}}, false, nil)
			case "malformed-connection":
				issue["inverseRelations"] = "not-a-connection"
			case "missing-page-info":
				issue["inverseRelations"] = map[string]any{"nodes": []any{}}
			}
			a, close := newConnectionsAdapter(t, func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(issueListResponse(issue)) })
			defer close()
			issues, err := a.FetchCandidateIssues(context.Background())
			if err != nil || len(issues) != 1 || issues[0].Dispatchable {
				t.Fatal(issues, err)
			}
		})
	}
}
func TestLinearNestedPaginationRefusesMissingRepeatedCursorAndPartialHTTP(t *testing.T) {
	for _, kind := range []string{"missing-cursor", "repeated-cursor", "missing-continuation", "http-error", "graphql-partial"} {
		t.Run(kind, func(t *testing.T) {
			issue := linearCompleteIssue()
			var cursor any = "page-1"
			if kind == "missing-cursor" {
				cursor = nil
			}
			issue["inverseRelations"] = linearConnection([]any{}, true, cursor)
			a, close := newConnectionsAdapter(t, func(w http.ResponseWriter, r *http.Request) {
				var req linearGraphQLRequest
				_ = json.NewDecoder(r.Body).Decode(&req)
				if strings.Contains(req.Query, "FetchIssueConnection") {
					switch kind {
					case "http-error":
						w.WriteHeader(500)
						fmt.Fprint(w, "lin_private_token")
						return
					case "graphql-partial":
						fmt.Fprint(w, `{"data":{"issue":{"id":"candidate"}},"errors":[{"message":"lin_private_token"}]}`)
						return
					case "missing-continuation":
						fmt.Fprint(w, `{"data":{"issue":{"id":"candidate"}}}`)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"issue": map[string]any{"id": "candidate", "inverseRelations": linearConnection([]any{}, true, "page-1")}}})
					return
				}
				_ = json.NewEncoder(w).Encode(issueListResponse(issue))
			})
			defer close()
			issues, err := a.FetchCandidateIssues(context.Background())
			if err == nil || issues != nil {
				t.Fatal("partial dependency graph accepted", issues, err)
			}
			if strings.Contains(err.Error(), "lin_private_token") {
				t.Fatal("credential leaked", err)
			}
		})
	}
}

func TestLinearConflictingBlockerPagesStayUnknown(t *testing.T) {
	issue := linearCompleteIssue()
	issue["inverseRelations"] = linearConnection([]any{linearBlocker("same", "Done", "completed")}, true, "next")
	a, close := newConnectionsAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		var req linearGraphQLRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if strings.Contains(req.Query, "FetchIssueConnection") {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"issue": map[string]any{"id": "candidate", "inverseRelations": linearConnection([]any{linearBlocker("same", "In Progress", "started")}, false, nil)}}})
			return
		}
		_ = json.NewEncoder(w).Encode(issueListResponse(issue))
	})
	defer close()
	issues, err := a.FetchCandidateIssues(context.Background())
	if err != nil || len(issues) != 1 || issues[0].Dispatchable || len(issues[0].BlockedBy) != 1 || issues[0].BlockedBy[0].Terminal != nil {
		t.Fatal(issues, err)
	}
}
