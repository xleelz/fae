package external

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func greenReview() ReviewStatus {
	return ReviewStatus{ReviewID: "R1", Tree: "tree", Build: ReviewAnalyzer{Name: "fleet", Status: "Pass", RequiredFor: []string{"publish"}}, Analyzers: []ReviewAnalyzer{{Name: "novel-check", Status: "Pass", RequiredFor: []string{"merge"}}}, Comments: []ReviewComment{}}
}
func TestReviewDeclaredGates(t *testing.T) {
	for _, tc := range []struct {
		name                           string
		change                         func(*ReviewStatus)
		publish, merge, remediate, err bool
	}{
		{"green", func(*ReviewStatus) {}, true, true, false, false},
		{"failed build", func(s *ReviewStatus) { s.Build.Status = "Fail" }, false, true, false, false},
		{"build cannot opt out", func(s *ReviewStatus) { s.Build.Status = "Fail"; s.Build.RequiredFor = []string{} }, false, true, false, false},
		{"blocked merge check", func(s *ReviewStatus) { s.Analyzers[0].Status = "Blocked" }, true, false, false, false},
		{"skipped merge check", func(s *ReviewStatus) { s.Analyzers[0].Status = "Skipped" }, true, false, false, false},
		{"change failure", func(s *ReviewStatus) { s.Analyzers[0].Status = "Fail"; s.Analyzers[0].Cause = "change" }, true, false, true, false},
		{"blocking comment", func(s *ReviewStatus) { s.Comments = []ReviewComment{{ID: "C1", Blocking: true}} }, true, false, true, false},
		{"addressed comment", func(s *ReviewStatus) { s.Comments = []ReviewComment{{ID: "C1", Blocking: true, Addressed: true}} }, true, true, false, false},
		{"unknown status", func(s *ReviewStatus) { s.Analyzers[0].Status = "green" }, false, false, false, true},
		{"missing status", func(s *ReviewStatus) { s.Analyzers[0].Status = "" }, false, false, false, true},
		{"missing declarations", func(s *ReviewStatus) { s.Analyzers[0].RequiredFor = nil }, false, false, false, true},
		{"missing checks", func(s *ReviewStatus) { s.Analyzers = nil }, false, false, false, true},
		{"missing comments", func(s *ReviewStatus) { s.Comments = nil }, false, false, false, true},
		{"unknown requirement", func(s *ReviewStatus) { s.Analyzers[0].RequiredFor = []string{"ship"} }, false, false, false, true},
		{"duplicate analyzer", func(s *ReviewStatus) { s.Analyzers[0].Name = s.Build.Name }, false, false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := greenReview()
			tc.change(&s)
			d, e := EvaluateReview(s)
			if (e != nil) != tc.err || d.Publish != tc.publish || d.Merge != tc.merge || d.Remediate != tc.remediate {
				t.Fatal(d, e)
			}
		})
	}
}
func TestReviewMissingCommentSeverityFailsClosed(t *testing.T) {
	for _, comment := range []string{`{"id":"C1","addressed":true}`, `{"id":"C1","blocking":true}`, `{"id":"C1","blocking":null,"addressed":true}`, `{"id":"C1","blocking":"false","addressed":true}`} {
		s := greenReview()
		raw, _ := json.Marshal(s)
		var fields map[string]json.RawMessage
		json.Unmarshal(raw, &fields)
		fields["comments"] = json.RawMessage("[" + comment + "]")
		raw, _ = json.Marshal(fields)
		if _, e := DecodeReviewStatus(raw); e == nil {
			t.Fatal("accepted", comment)
		}
	}
}

func TestReadPreservesLargeIntegerSchemaEnums(t *testing.T) {
	for _, value := range []string{"9007199254740993", "9007199254740992"} {
		t.Run(value, func(t *testing.T) {
			schema := ToolSchema{Name: "read", InputSchema: json.RawMessage(`{"type":"object","properties":{"pin":{"type":"string"}},"additionalProperties":false}`), OutputSchema: json.RawMessage(`{"type":"object","properties":{"sequence":{"type":"integer","enum":[9007199254740993]}},"required":["sequence"]}`)}
			var intent Intent
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/schemas" {
					json.NewEncoder(w).Encode(SchemaCatalog{Server: "fixture", Tools: []ToolSchema{schema}})
					return
				}
				json.NewEncoder(w).Encode(Receipt{Key: intent.Key, Task: intent.Task, Tool: intent.Tool, Digest: intent.Digest, ExternalID: "R1", State: "performed", Tree: intent.Tree, Data: json.RawMessage(`{"sequence":` + value + `}`)})
			}))
			defer server.Close()
			tool := Tool{Endpoint: server.URL, Server: "fixture", Name: "read", Role: "role", Arguments: map[string]string{"pin": "exact"}, DiscoveredSchema: &schema}
			var e error
			intent, e = NewIntent(tool, "task", "stage", "key", "role", "tree", "session", tool.Arguments)
			if e != nil {
				t.Fatal(e)
			}
			_, e = Read(context.Background(), tool, intent)
			if (e == nil) != (value == "9007199254740993") {
				t.Fatal("numeric schema mismatch", value, e)
			}
		})
	}
}
