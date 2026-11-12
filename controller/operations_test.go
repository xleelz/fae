package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestOperationAuthorizationBindsEveryOptionAndInstance(t *testing.T) {
	root, err := os.MkdirTemp("/var/tmp", "fae-ctl-auth-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	if err := os.Mkdir(filepath.Join(root, "run"), 0700); err != nil {
		t.Fatal(err)
	}
	credential := []byte(strings.Repeat("k", 32))
	if err := os.WriteFile(filepath.Join(root, "run", "credential"), credential, 0600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "controller-maintenance.json")
	r := OperationRequest{Action: "roll", Options: Options{Message: "m", Note: "roll", Launcher: "/private/launcher.sock", Binary: "/private/fae"}}
	r.Authorization = SignOperation(r, credential)
	if err := authorizeOperationRequest(marker, r); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*OperationRequest){
		func(r *OperationRequest) { r.Action = "stop" },
		func(r *OperationRequest) { r.Options.Binary = "/different/fae" },
		func(r *OperationRequest) { r.Options.Message = "other-message" },
		func(r *OperationRequest) { r.Options.BuildRef = "refs/other" },
		func(r *OperationRequest) { r.Options.AckOverlap = true },
		func(r *OperationRequest) { r.Options.Launcher = "/other/launcher.sock" },
		func(r *OperationRequest) { r.Authorization = SignOperation(*r, []byte(strings.Repeat("x", 32))) },
	} {
		bad := r
		mutate(&bad)
		if err := authorizeOperationRequest(marker, bad); err == nil {
			t.Fatalf("tampered request accepted %+v", bad)
		}
	}
	bad := r
	bad.Authorization = ""
	if err := authorizeOperationRequest(marker, bad); err == nil {
		t.Fatal("launcher capability alone granted operation authority")
	}
	issued := strconv.FormatInt(time.Now().Add(-time.Minute).Unix(), 10)
	bad = r
	bad.Authorization = issued + "." + operationMAC(r, credential, issued)
	if err := authorizeOperationRequest(marker, bad); err == nil {
		t.Fatal("expired submission accepted")
	}
}

func TestOperationReceiptsSurviveLauncherRestartWithoutReplay(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "receipts")
	newService := func() *operationService {
		s := newOperationService(context.Background(), "/private/launcher.sock", "/private/maintenance.json")
		s.receiptDir = dir
		return s
	}
	s := newService()
	r := OperationRequest{Action: "stop", Options: Options{Message: "m", Note: "stop", Launcher: s.socket}}
	finish := make(chan struct{})
	s.run = func(context.Context, string, Options) (Health, *Marker, error) {
		<-finish
		return Health{PID: 123}, nil, nil
	}
	first, err := s.request(r)
	if err != nil {
		t.Fatal(err)
	}
	restarted := newService()
	restarted.run = func(context.Context, string, Options) (Health, *Marker, error) {
		t.Error("replayed unfinished operation")
		return Health{}, nil, nil
	}
	uncertain, err := restarted.request(r)
	if err != nil || !uncertain.Done || !strings.Contains(uncertain.Error, "interrupted") {
		t.Fatalf("uncertain %+v %v", uncertain, err)
	}
	close(finish)
	deadline := time.Now().Add(time.Second)
	for {
		v, err := s.request(OperationRequest{Status: true, ID: first.ID})
		if err != nil {
			t.Fatal(err)
		}
		if v.Done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("not complete")
		}
		time.Sleep(time.Millisecond)
	}
	completed, err := restarted.request(r)
	if err != nil || !completed.Done || completed.Error != "" || completed.Health.PID != 123 {
		t.Fatalf("completed %+v %v", completed, err)
	}
	if _, err = restarted.request(OperationRequest{Status: true, ID: "../../escape"}); err == nil {
		t.Fatal("unsafe receipt path accepted")
	}
	if err = os.WriteFile(filepath.Join(dir, first.ID+".json"), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = newService().request(r); err == nil {
		t.Fatal("corrupt receipt replayed")
	}
}

func TestFullOperationReceiptDeduplicatesAndSerializes(t *testing.T) {
	s := newOperationService(context.Background(), "/private/launcher.sock", "/private/maintenance.json")
	started, finish := make(chan struct{}), make(chan struct{})
	var runs atomic.Int32
	s.run = func(context.Context, string, Options) (Health, *Marker, error) {
		runs.Add(1)
		close(started)
		<-finish
		return Health{PID: 123}, nil, nil
	}
	r := OperationRequest{Action: "roll", Options: Options{Message: "m", Note: "roll", Launcher: s.socket, Binary: "/private/fae"}}
	first, err := s.request(r)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	second, err := s.request(r)
	if err != nil || second.ID != first.ID || second.Done {
		t.Fatalf("retry: %+v %v", second, err)
	}
	other := r
	other.Options.Message = "another"
	if _, err := s.request(other); err == nil {
		t.Fatal("concurrent mutation accepted")
	}
	close(finish)
	deadline := time.Now().Add(time.Second)
	for {
		v, err := s.request(OperationRequest{Status: true, ID: first.ID})
		if err != nil {
			t.Fatal(err)
		}
		if v.Done {
			if v.Health.PID != 123 {
				t.Fatal(v)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("receipt did not finish")
		}
		time.Sleep(time.Millisecond)
	}
	last, err := s.request(r)
	if err != nil || !last.Done || runs.Load() != 1 {
		t.Fatalf("completed retry: %+v %v runs=%d", last, err, runs.Load())
	}
	if _, err := s.request(OperationRequest{Status: true, ID: "missing"}); err == nil {
		t.Fatal("missing receipt accepted")
	}
}

func TestFullOperationRejectsInvalidRequests(t *testing.T) {
	s := newOperationService(context.Background(), "/private/launcher.sock", "/private/maintenance.json")
	s.run = func(context.Context, string, Options) (Health, *Marker, error) {
		t.Error("invalid request executed")
		return Health{}, nil, nil
	}
	valid := OperationRequest{Action: "stop", Options: Options{Message: "m", Note: "stop", Launcher: s.socket}}
	for _, mutate := range []func(*OperationRequest){
		func(r *OperationRequest) { r.Action = "shell" },
		func(r *OperationRequest) { r.Options.Message = "" },
		func(r *OperationRequest) { r.Options.Launcher = "/other.sock" },
		func(r *OperationRequest) { r.Options.Note = "" },
		func(r *OperationRequest) { r.Options.MaxWait = -time.Second },
		func(r *OperationRequest) { r.Options.OverlapWait = 25 * time.Hour },
		func(r *OperationRequest) { r.Action = "roll" },
	} {
		r := valid
		mutate(&r)
		if _, err := s.request(r); err == nil {
			t.Fatalf("accepted %+v", r)
		}
	}
}

func TestFullOperationTransportAuthenticationAndFailureReceipt(t *testing.T) {
	socket, ctx := launcherFixture(t)
	// The new full-operation branch shares the exact launcher authentication.
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	request := OperationRequest{Action: "start", Options: Options{Message: "m", Launcher: socket}}
	if err = json.NewEncoder(conn).Encode(launchRequest{Token: strings.Repeat("0", 64), Operation: &request}); err != nil {
		t.Fatal(err)
	}
	var response launchResponse
	if err = json.NewDecoder(conn).Decode(&response); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if !strings.Contains(response.Error, "unauthenticated") {
		t.Fatal(response)
	}
	result, err := OperationRPC(ctx, socket, request)
	if err != nil {
		t.Fatal(err)
	}
	bound, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	completed, err := WaitOperation(bound, socket, result)
	if err == nil || !strings.Contains(err.Error(), "maintenance marker") || !completed.Done {
		t.Fatalf("failure receipt %+v %v", completed, err)
	}
	duplicate, err := OperationRPC(ctx, socket, request)
	if err != nil || duplicate.ID != result.ID || !duplicate.Done {
		t.Fatalf("retry %+v %v", duplicate, err)
	}
	if _, err = OperationRPC(ctx, socket, OperationRequest{Status: true, ID: "missing"}); err == nil {
		t.Fatal("unknown receipt accepted")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = WaitOperation(cancelled, socket, OperationResult{ID: "active"}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
