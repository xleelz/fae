package api

import (
	"context"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAuthenticatedPrincipalCannotBeSpoofed(t *testing.T) {
	owner := make([]byte, 32)
	delegated := make([]byte, 32)
	delegated[0] = 1
	var got Principal
	s, _ := New(owner, func(ctx context.Context, r Request) (any, error) {
		got, _ = AuthenticatedPrincipal(ctx)
		return true, nil
	})
	if e := s.AddPrincipal(delegated, Principal{Actor: "prophet", Task: "t"}); e != nil {
		t.Fatal(e)
	}
	if e := s.AddPrincipal(owner, Principal{Actor: "controller"}); e == nil {
		t.Fatal("owner overwrite")
	}
	for _, tc := range []struct {
		token []byte
		body  string
		actor string
		code  int
	}{{owner, `{"version":1,"method":"Status"}`, "operator", 200}, {delegated, `{"version":1,"method":"Status"}`, "prophet", 200}, {delegated, `{"version":1,"method":"Status","actor":"operator"}`, "", 400}} {
		got = Principal{}
		r := httptest.NewRequest("POST", "/v1/rpc", strings.NewReader(tc.body))
		r.Header.Set("Authorization", hex.EncodeToString(tc.token))
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != tc.code || got.Actor != tc.actor {
			t.Fatalf("%d %+v", w.Code, got)
		}
	}
	s.SetEventStream(func(w http.ResponseWriter, r *http.Request) { t.Fatal("scope bypass") })
	r := httptest.NewRequest("GET", "/v1/events", nil)
	r.Header.Set("Authorization", hex.EncodeToString(delegated))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal(w.Code)
	}
}
