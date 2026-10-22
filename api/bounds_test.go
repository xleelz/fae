package api

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReplyBoundAndTypedGuidance(t *testing.T) {
	s := &Server{}
	for _, tc := range []struct {
		result any
		err    error
		code   string
	}{{strings.Repeat("x", MaxResponseBytes), nil, "response-too-large"}, {nil, &RPCError{Code: "response-too-large", Message: "use summary pagination"}, "response-too-large"}, {nil, errors.New(strings.Repeat("x", 2*MaxResponseBytes)), ""}} {
		w := httptest.NewRecorder()
		s.reply(w, tc.result, tc.err)
		if w.Body.Len() >= MaxResponseBytes {
			t.Fatal("oversized transport response")
		}
		var response Response
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if response.ErrorCode != tc.code || response.Error == "" || len(response.Result) > 0 {
			t.Fatalf("incorrect bounded error: %+v", response)
		}
	}
}
