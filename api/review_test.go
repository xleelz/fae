package api

import (
	"context"
	"encoding/hex"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDuplicateRequestKeysNeverReachHandler(t *testing.T) {
	token := make([]byte, 32)
	calls := 0
	server, err := New(token, func(context.Context, Request) (any, error) { calls++; return nil, nil })
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{"version":1,"method":"Status","method":"Admit"}`, `{"version":1,"method":"Admit","params":{"mode":"Local","mode":"Normal"}}`} {
		request := httptest.NewRequest("POST", "/v1/rpc", strings.NewReader(body))
		request.Header.Set("Authorization", hex.EncodeToString(token))
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != 400 {
			t.Fatalf("ambiguous request status %d: %s", response.Code, response.Body)
		}
	}
	if calls != 0 {
		t.Fatalf("ambiguous requests reached handler %d times", calls)
	}
}
