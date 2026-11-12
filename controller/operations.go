package controller

// Full lifecycle operations run in the authenticated launcher, whose lifetime
// spans a daemon replacement. Clients submit once and read the same receipt;
// they never execute the individual stop/start/accept phases themselves.
import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/xleelz/fae/api"
)

type OperationRequest struct {
	Action        string  `json:"action"`
	Options       Options `json:"options"`
	ID            string  `json:"id,omitempty"`
	Status        bool    `json:"status,omitempty"`
	Authorization string  `json:"authorization,omitempty"`
}

// SignOperation binds a submission to the daemon's validated authority. Merely
// possessing the launcher's launch-only capability must not grant roll/build
// authority. No credential value is sent to the launcher or recorded in audit.
func SignOperation(r OperationRequest, credential []byte) string {
	issued := strconv.FormatInt(time.Now().Unix(), 10)
	return issued + "." + operationMAC(r, credential, issued)
}

func operationMAC(r OperationRequest, credential []byte, issued string) string {
	raw, _ := json.Marshal(struct {
		Action  string
		Options Options
	}{r.Action, r.Options})
	mac := hmac.New(sha256.New, credential)
	mac.Write([]byte("fae-controller-operation-v1\x00"))
	mac.Write([]byte(issued + "\x00"))
	mac.Write(raw)
	return hex.EncodeToString(mac.Sum(nil))
}

func authorizeOperationRequest(markerPath string, r OperationRequest) error {
	if r.Status || r.Action == "start" {
		return nil
	} // offline start checks the exact daemon-sealed marker
	credential, err := api.RepositoryFreeCredential(filepath.Join(filepath.Dir(markerPath), "run", "credential"))
	if err != nil {
		return err
	}
	issued, signature, ok := strings.Cut(r.Authorization, ".")
	stamp, err := strconv.ParseInt(issued, 10, 64)
	if !ok || err != nil || time.Since(time.Unix(stamp, 0)) > 30*time.Second || time.Unix(stamp, 0).After(time.Now().Add(5*time.Second)) {
		return errors.New("expired controller operation authorization; resubmit through daemon")
	}
	proof, err := hex.DecodeString(signature)
	if err != nil {
		return errors.New("invalid controller operation authorization")
	}
	expected, _ := hex.DecodeString(operationMAC(r, credential, issued))
	if !hmac.Equal(proof, expected) {
		return errors.New("controller operation was not authorized by daemon")
	}
	return nil
}

type OperationResult struct {
	ID     string  `json:"id"`
	Done   bool    `json:"done"`
	Health Health  `json:"health"`
	Marker *Marker `json:"marker,omitempty"`
	Error  string  `json:"error,omitempty"`
}

type operationService struct {
	ctx                context.Context
	socket, markerPath string
	mu                 sync.Mutex
	active             bool
	results            map[string]OperationResult
	run                func(context.Context, string, Options) (Health, *Marker, error)
	receiptDir         string
}

func (s *operationService) readReceipt(id string) (OperationResult, bool, error) {
	if s.receiptDir == "" {
		return OperationResult{}, false, nil
	}
	if len(id) != 64 {
		return OperationResult{}, false, errors.New("invalid controller receipt id")
	}
	if _, err := hex.DecodeString(id); err != nil {
		return OperationResult{}, false, errors.New("invalid controller receipt id")
	}
	f, err := os.OpenFile(filepath.Join(s.receiptDir, id+".json"), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) {
		return OperationResult{}, false, nil
	}
	if err != nil {
		return OperationResult{}, false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return OperationResult{}, false, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > 1<<20 {
		return OperationResult{}, false, errors.New("invalid private controller receipt")
	}
	var v OperationResult
	if err = json.NewDecoder(io.LimitReader(f, 1<<20)).Decode(&v); err != nil {
		return v, false, err
	}
	if v.ID != id {
		return v, false, errors.New("controller receipt identity mismatch")
	}
	if !v.Done {
		v.Done = true
		v.Error = "controller operation interrupted by launcher restart; inspect maintenance marker before using new authority"
	}
	return v, true, nil
}

func (s *operationService) writeReceipt(v OperationResult, create bool) error {
	if s.receiptDir == "" {
		return nil
	}
	if err := os.MkdirAll(s.receiptDir, 0700); err != nil {
		return err
	}
	physical, err := filepath.EvalSymlinks(s.receiptDir)
	if err != nil || physical != s.receiptDir {
		return errors.New("controller receipt directory must be physical")
	}
	info, err := os.Stat(s.receiptDir)
	if err != nil {
		return err
	}
	if info.Mode().Perm() != 0700 {
		return errors.New("controller receipt directory must be private")
	}
	// Publish the receipt directory itself durably before any worker starts.
	if err := syncReceiptDirectory(filepath.Dir(s.receiptDir)); err != nil {
		return err
	}
	path := filepath.Join(s.receiptDir, v.ID+".json")
	if create {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
		if err != nil {
			return err
		}
		err = json.NewEncoder(f).Encode(v)
		if err = errors.Join(err, f.Sync(), f.Close()); err != nil {
			return err
		}
		return syncReceiptDirectory(s.receiptDir)
	}
	f, err := os.CreateTemp(s.receiptDir, ".receipt-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	err = json.NewEncoder(f).Encode(v)
	err = errors.Join(err, f.Sync(), f.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	return syncReceiptDirectory(s.receiptDir)
}

func syncReceiptDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

func newOperationService(ctx context.Context, socket, markerPath string) *operationService {
	s := &operationService{ctx: ctx, socket: socket, markerPath: markerPath, results: map[string]OperationResult{}}
	s.run = s.execute
	return s
}

func ValidateOperation(r OperationRequest, socket string) error {
	if r.Action != "roll" && r.Action != "stop" && r.Action != "start" {
		return errors.New("unknown controller operation")
	}
	o := r.Options
	if o.Message == "" || len(o.Message) > 4096 {
		return errors.New("controller operation requires authorizing message")
	}
	if o.Launcher != socket || !filepath.IsAbs(socket) {
		return errors.New("controller operation launcher mismatch")
	}
	if o.MaxWait < 0 || o.MaxWait > 24*time.Hour || o.OverlapWait < 0 || o.OverlapWait > 24*time.Hour {
		return errors.New("controller operation wait outside 0..24h")
	}
	if r.Action != "start" && o.Note == "" {
		return errors.New("controller operation requires note")
	}
	if r.Action == "roll" && (o.Binary == "") == (o.BuildRef == "") {
		return errors.New("roll requires exactly one of binary or build-ref")
	}
	return nil
}

func (s *operationService) request(r OperationRequest) (OperationResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Status {
		v, ok := s.results[r.ID]
		if !ok {
			var err error
			v, ok, err = s.readReceipt(r.ID)
			if err != nil {
				return v, err
			}
			if !ok {
				return v, errors.New("unknown controller operation receipt")
			}
		}
		return v, nil
	}
	if err := ValidateOperation(r, s.socket); err != nil {
		return OperationResult{}, err
	}
	// Action+options include the immutable authorization message. Identical
	// retries return the original receipt, including after completion.
	raw, _ := json.Marshal(struct {
		Action  string
		Options Options
	}{r.Action, r.Options})
	hash := sha256.Sum256(raw)
	id := hex.EncodeToString(hash[:])
	if v, ok := s.results[id]; ok {
		return v, nil
	}
	if v, ok, err := s.readReceipt(id); err != nil {
		return v, err
	} else if ok {
		return v, nil
	}
	if s.active {
		return OperationResult{}, errors.New("another controller operation is in progress")
	}
	v := OperationResult{ID: id}
	if err := s.writeReceipt(v, true); err != nil {
		return OperationResult{}, err
	}
	if len(s.results) >= 256 {
		if s.receiptDir == "" {
			return OperationResult{}, errors.New("controller receipt capacity reached")
		}
		for old, receipt := range s.results {
			if receipt.Done {
				delete(s.results, old)
				break
			}
		}
	}
	s.results[id] = v
	s.active = true
	go func() {
		h, m, err := s.run(s.ctx, r.Action, r.Options)
		v := OperationResult{ID: id, Done: true, Health: h, Marker: m}
		if err != nil {
			v.Error = err.Error()
		}
		if err := s.writeReceipt(v, false); err != nil {
			v.Error = errors.Join(errors.New(v.Error), fmt.Errorf("controller receipt persistence: %w", err)).Error()
		}
		s.mu.Lock()
		s.results[id] = v
		s.active = false
		s.mu.Unlock()
	}()
	return v, nil
}

func (s *operationService) execute(ctx context.Context, action string, o Options) (Health, *Marker, error) {
	root := filepath.Dir(s.markerPath)
	client := Client{MarkerPath: s.markerPath, WatchdogIdentity: WatchdogIdentityPath(filepath.Join(root, "run", "watchdog-state.json"))}
	client.RPC = func(ctx context.Context, p Params) (Health, error) {
		credential, err := api.RepositoryFreeCredential(filepath.Join(root, "run", "credential"))
		if err != nil {
			return Health{}, err
		}
		raw, err := json.Marshal(p)
		if err != nil {
			return Health{}, err
		}
		response, err := api.Call(ctx, filepath.Join(root, "run", "rpc.sock"), credential, api.Request{Version: api.Version, Method: "Controller", Params: raw})
		if err != nil {
			return Health{}, err
		}
		var h Health
		err = json.Unmarshal(response.Result, &h)
		return h, err
	}
	switch action {
	case "roll":
		h, err := client.Roll(ctx, o)
		return h, nil, err
	case "stop":
		m, err := client.Stop(ctx, o)
		return Health{}, m, err
	case "start":
		h, err := client.Start(ctx, o)
		return h, nil, err
	}
	return Health{}, nil, errors.New("unknown controller operation")
}

// OperationRPC authenticates both submission and receipt reads with the private
// launcher capability. Submission returns before any long-running phase ends.
func OperationRPC(ctx context.Context, socket string, r OperationRequest) (OperationResult, error) {
	info, err := os.Lstat(socket)
	if err != nil {
		return OperationResult{}, err
	}
	if info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0600 {
		return OperationResult{}, errors.New("invalid launcher capability socket")
	}
	key, err := readLauncherKey(LauncherKeyPath(socket))
	if err != nil {
		return OperationResult{}, err
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
	if err != nil {
		return OperationResult{}, err
	}
	defer conn.Close()
	deadline := time.Now().Add(30 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if err = json.NewEncoder(conn).Encode(launchRequest{Token: key, Operation: &r}); err != nil {
		return OperationResult{}, err
	}
	var response launchResponse
	if err = json.NewDecoder(conn).Decode(&response); err != nil {
		return OperationResult{}, err
	}
	if response.Error != "" {
		return OperationResult{}, errors.New(response.Error)
	}
	if response.Operation == nil {
		return OperationResult{}, errors.New("launcher does not support full controller operations; update it before retrying")
	}
	return *response.Operation, nil
}

func WaitOperation(ctx context.Context, socket string, result OperationResult) (OperationResult, error) {
	for !result.Done {
		select {
		case <-ctx.Done():
			return result, fmt.Errorf("controller operation %s continues on launcher: %w", result.ID, ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
		next, err := OperationRPC(ctx, socket, OperationRequest{Status: true, ID: result.ID})
		if err != nil {
			return result, err
		}
		result = next
	}
	if result.Error != "" {
		return result, errors.New(result.Error)
	}
	return result, nil
}
