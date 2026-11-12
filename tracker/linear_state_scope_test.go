package tracker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func stateIssue(id, team string) map[string]any {
	node := linearCompleteIssue()
	node["id"] = id
	node["identifier"] = strings.ToUpper(id) + "-1"
	node["team"] = map[string]any{"id": team, "key": strings.ToUpper(team)}
	node["state"] = map[string]any{"id": team + "-todo", "name": "Todo", "type": "unstarted"}
	return node
}
func stateNode(id, name, team string) map[string]any {
	return map[string]any{"id": id, "name": name, "type": "completed", "team": map[string]any{"id": team, "key": strings.ToUpper(team)}}
}
func TestLinearTransitionUsesActualIssueTeamAndTeamScopedCache(t *testing.T) {
	mutations := map[string]string{}
	catalogs := map[string]int{}
	a, close := newConnectionsAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		var req linearGraphQLRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch {
		case strings.Contains(req.Query, "GetLinearIssueForStateWrite"):
			id := req.Variables["id"].(string)
			team := "team-a"
			if id == "b" {
				team = "team-b"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"issue": stateIssue(id, team)}})
		case strings.Contains(req.Query, "ResolveWorkflowStates"):
			filter := req.Variables["filter"].(map[string]any)
			team := filter["team"].(map[string]any)["id"].(map[string]any)["eq"].(string)
			catalogs[team]++
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"workflowStates": linearConnection([]any{stateNode("team-b-done", "Done", "team-b"), stateNode("team-a-done", "Done", "team-a")}, false, nil)}})
		case strings.Contains(req.Query, "TransitionIssueState"):
			id := req.Variables["id"].(string)
			state := req.Variables["stateId"].(string)
			mutations[id] = state
			team := "team-a"
			if id == "b" {
				team = "team-b"
			}
			node := stateIssue(id, team)
			node["state"] = map[string]any{"id": state, "name": "Done", "type": "completed"}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"issueUpdate": map[string]any{"success": true, "issue": node}}})
		default:
			t.Error("unexpected query", req.Query)
		}
	})
	defer close()
	for _, id := range []string{"a", "b", "a"} {
		issue, err := a.TransitionIssueState(context.Background(), id, "Done")
		if err != nil || issue.State != "Done" {
			t.Fatal(issue, err)
		}
	}
	if mutations["a"] != "team-a-done" || mutations["b"] != "team-b-done" || catalogs["team-a"] != 1 || catalogs["team-b"] != 1 {
		t.Fatal(mutations, catalogs)
	}
}
func TestLinearTransitionStateAliasesAndLaterStatePage(t *testing.T) {
	writes := []string{}
	catalogCalls := 0
	a, close := newConnectionsAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		var req linearGraphQLRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch {
		case strings.Contains(req.Query, "GetLinearIssueForStateWrite"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"issue": stateIssue("a", "team-a")}})
		case strings.Contains(req.Query, "ResolveWorkflowStates"):
			catalogCalls++
			nodes := []any{stateNode("working", "Working", "team-a")}
			more := true
			var cursor any = "next-states"
			if req.Variables["after"] != nil {
				nodes = []any{stateNode("released", "Released", "team-a")}
				more = false
				cursor = nil
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"workflowStates": linearConnection(nodes, more, cursor)}})
		case strings.Contains(req.Query, "TransitionIssueState"):
			id := req.Variables["stateId"].(string)
			writes = append(writes, id)
			name := "Working"
			if id == "released" {
				name = "Released"
			}
			node := stateIssue("a", "team-a")
			node["state"] = map[string]any{"id": id, "name": name}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"issueUpdate": map[string]any{"success": true, "issue": node}}})
		}
	})
	defer close()
	a.stateMap["in_progress"] = "Working"
	a.stateMap["done"] = "Released"
	for _, target := range []string{"InProgress", "Done"} {
		if _, err := a.TransitionIssueState(context.Background(), "a", target); err != nil {
			t.Fatal(err)
		}
	}
	if fmt.Sprint(writes) != "[working released]" || catalogCalls != 2 {
		t.Fatal(writes, catalogCalls)
	}
}
func TestLinearTransitionOutOfScopeAndMalformedTeamCannotMutate(t *testing.T) {
	for _, kind := range []string{"foreign-project", "foreign-team", "missing-team", "wrong-identity"} {
		t.Run(kind, func(t *testing.T) {
			writes := 0
			node := stateIssue("a", "team-a")
			switch kind {
			case "foreign-project":
				node["project"] = map[string]any{"id": "foreign"}
			case "foreign-team":
				node["team"] = map[string]any{"id": "team-b", "key": "TEAM-B"}
			case "missing-team":
				node["team"] = nil
			case "wrong-identity":
				node["id"] = "other"
			}
			a, close := newConnectionsAdapter(t, func(w http.ResponseWriter, r *http.Request) {
				var req linearGraphQLRequest
				_ = json.NewDecoder(r.Body).Decode(&req)
				if strings.Contains(req.Query, "GetLinearIssueForStateWrite") {
					_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"issue": node}})
					return
				}
				writes++
				t.Error("unsafe request after invalid issue scope", req.Query)
			})
			defer close()
			if kind == "foreign-team" {
				a.teamID = "team-a"
			}
			if _, err := a.TransitionIssueState(context.Background(), "a", "Done"); err == nil || writes != 0 {
				t.Fatal("out-of-scope write", writes, err)
			}
		})
	}
}
func TestLinearTransitionIncompleteStateCatalogCannotMutateOrCache(t *testing.T) {
	for _, kind := range []string{"missing-info", "missing-cursor", "repeated-cursor", "missing-team", "duplicate-name"} {
		t.Run(kind, func(t *testing.T) {
			writes := 0
			node := stateIssue("a", "team-a")
			a, close := newConnectionsAdapter(t, func(w http.ResponseWriter, r *http.Request) {
				var req linearGraphQLRequest
				_ = json.NewDecoder(r.Body).Decode(&req)
				if strings.Contains(req.Query, "GetLinearIssueForStateWrite") {
					_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"issue": node}})
					return
				}
				if strings.Contains(req.Query, "TransitionIssueState") {
					writes++
					return
				}
				connection := linearConnection([]any{stateNode("done", "Done", "team-a")}, false, nil)
				switch kind {
				case "missing-info":
					delete(connection, "pageInfo")
				case "missing-cursor":
					connection["pageInfo"] = map[string]any{"hasNextPage": true}
				case "repeated-cursor":
					connection["pageInfo"] = map[string]any{"hasNextPage": true, "endCursor": "repeat"}
				case "missing-team":
					connection["nodes"] = []any{map[string]any{"id": "done", "name": "Done"}}
				case "duplicate-name":
					connection["nodes"] = []any{stateNode("one", "Done", "team-a"), stateNode("two", "Done", "team-a")}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"workflowStates": connection}})
			})
			defer close()
			if _, err := a.TransitionIssueState(context.Background(), "a", "Done"); err == nil || writes != 0 {
				t.Fatal("truncated or ambiguous state catalog mutated", writes, err)
			}
			if len(a.stateCache) != 0 {
				t.Fatal("partial state catalog cached", a.stateCache)
			}
		})
	}
}
