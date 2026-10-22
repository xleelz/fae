package api

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRequestDeadlineTyped(t *testing.T) {
	token := make([]byte, 32)
	server, err := New(token, func(ctx context.Context, r Request) (any, error) { <-ctx.Done(); return nil, ctx.Err() })
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(Request{Version: 1, Method: "History", Deadline: time.Now().Add(-time.Second)})
	request := httptest.NewRequest("POST", "/v1/rpc", strings.NewReader(string(raw)))
	request.Header.Set("Authorization", hex.EncodeToString(token))
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	var result Response
	if json.Unmarshal(response.Body.Bytes(), &result) != nil || result.ErrorCode != "deadline-exceeded" {
		t.Fatal(response.Body.String())
	}
}
