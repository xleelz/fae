package api

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// FAE-ARC-004 additive evolution: an older client reads a newer server's
// response, whose envelope and result carry fields it does not know.
func TestOlderClientReadsAdditiveResponse(t *testing.T) {
	dir, err := os.MkdirTemp("/var/tmp", "fae-api-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "s")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"version":1,"result":{"ok":true,"added_in_a_later_release":{"x":1}},"added_envelope_field":"later"}`))
	})}
	go server.Serve(listener)
	defer server.Close()
	response, err := Call(context.Background(), socket, make([]byte, 32), Request{Version: Version, Method: "Status"})
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		OK bool `json:"ok"`
	}
	if err = json.Unmarshal(response.Result, &result); err != nil || !result.OK {
		t.Fatal(string(response.Result), err)
	}
}
