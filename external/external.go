// Package external implements an explicitly installed, daemon-only effect
// adapter. Query is read-only; ambiguous replies never trigger mutation retries.
package external

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xleelz/fae/domain"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Tool struct {
	DiscoveredSchema *ToolSchema       `json:"-"`
	Endpoint         string            `json:"endpoint"`
	Server           string            `json:"server"`
	Name             string            `json:"name"`
	Role             string            `json:"role"`
	Arguments        map[string]string `json:"arguments"`
}
type Intent struct {
	SchemaDigest   string            `json:"schema_digest,omitempty"`
	TypedArguments map[string]any    `json:"typed_arguments,omitempty"`
	Endpoint       string            `json:"endpoint"`
	Server         string            `json:"server"`
	Key            string            `json:"key"`
	Task           string            `json:"task"`
	Stage          string            `json:"stage"`
	Tool           string            `json:"tool"`
	Arguments      map[string]string `json:"arguments"`
	Digest         string            `json:"digest"`
	Tree           string            `json:"tree"`
	Session        string            `json:"session"`
}
type Receipt struct {
	Data       json.RawMessage `json:"data,omitempty"`
	Key        string          `json:"key"`
	Task       string          `json:"task"`
	Tool       string          `json:"tool"`
	Digest     string          `json:"digest"`
	ExternalID string          `json:"external_id"`
	State      string          `json:"state"`
	Tree       string          `json:"tree"`
	CommitSHA  string          `json:"commit_sha,omitempty"`
}

func CanonicalTool(server, tool, reported string) (string, error) {
	if server == "" || tool == "" || strings.ContainsAny(server+tool, "*/ \t\n") {
		return "", errors.New("invalid exact tool pin")
	}
	if reported != tool && reported != "mcp__"+server+"__"+tool {
		return "", errors.New("tool differs from pinned server/tool")
	}
	return tool, nil
}
func (t Tool) Validate() error {
	if _, e := CanonicalTool(t.Server, t.Name, t.Name); e != nil {
		return e
	}
	u, e := url.Parse(t.Endpoint)
	if e != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
		return errors.New("invalid external endpoint")
	}
	if u.Scheme == "http" && u.Hostname() != "127.0.0.1" && u.Hostname() != "::1" {
		return errors.New("external HTTP requires literal loopback; remote endpoints require TLS")
	}
	if t.Role == "" || t.Role == "*" || len(t.Arguments) == 0 {
		return errors.New("exact role and argument pins required")
	}
	// Review creation cannot implicitly publish or merge.
	if t.Name == "review_create" {
		if t.Arguments["branch"] == "" {
			return errors.New("review_create requires recorded branch pin")
		}
		for _, k := range []string{"auto_publish", "auto_merge", "auto_merge_with_rerun"} {
			if v := t.Arguments[k]; v != "" && v != "false" {
				return fmt.Errorf("%s must be omitted or false", k)
			}
		}
		if t.Arguments["draft"] != "true" {
			return errors.New("review_create requires draft=true")
		}
	}
	return nil
}
func NewIntent(t Tool, task, stage, key, role, tree, session string, pins map[string]string) (Intent, error) {
	if e := t.Validate(); e != nil {
		return Intent{}, e
	}
	if task == "" || stage == "" || key == "" || tree == "" || session == "" || role != t.Role {
		return Intent{}, errors.New("missing identity or role grant")
	}
	a, _ := json.Marshal(t.Arguments)
	b, _ := json.Marshal(pins)
	if !bytes.Equal(a, b) {
		return Intent{}, errors.New("arguments differ from installed pins")
	}
	var typed map[string]any
	var schemaDigest string
	if t.DiscoveredSchema != nil {
		var err error
		schemaDigest, err = domain.ContentHash(t.DiscoveredSchema.InputSchema)
		if err != nil {
			return Intent{}, err
		}
		typed, err = ValidatePins(*t.DiscoveredSchema, pins)
		if err != nil {
			return Intent{}, err
		}
		a, err = json.Marshal(typed)
		if err != nil {
			return Intent{}, err
		}
	}
	digest, _ := domain.ContentHash(a)
	identityParts := []string{task, t.Server, t.Name, stage, key, digest}
	if t.Name == "review_create" {
		identityParts = []string{task, t.Server, t.Name, t.Arguments["branch"]}
	}
	if t.Name == "ticket_create" {
		identityParts = []string{task, t.Server, t.Name, key}
	}
	identity, _ := json.Marshal(identityParts)
	stable, _ := domain.ContentHash(identity)
	clone := map[string]string{}
	for k, v := range pins {
		clone[k] = v
	}
	return Intent{SchemaDigest: schemaDigest, TypedArguments: typed, Endpoint: t.Endpoint, Server: t.Server, Key: stable, Task: task, Stage: stage, Tool: t.Name, Arguments: clone, Digest: digest, Tree: tree, Session: session}, nil
}
func Match(t Tool, i Intent, r Receipt) error {
	tool, e := CanonicalTool(t.Server, t.Name, r.Tool)
	if e != nil {
		return e
	}
	if !CanAdopt(r.Key == i.Key && r.Task == i.Task && tool == i.Tool, r.Digest == i.Digest, r.ExternalID != "", r.State == "performed" && r.Tree == i.Tree) {
		return errors.New("external receipt does not prove pinned effect")
	}
	return nil
}
func requestPath(ctx context.Context, t Tool, method, path string, i Intent) (Receipt, bool, error) {
	var r Receipt

	if err := ValidateIntentSchema(t, i); err != nil {
		return r, false, err
	}
	if t.DiscoveredSchema != nil {
		current, err := new(Discovery).Discover(ctx, t)
		if err != nil {
			return r, false, err
		}
		expected, _ := json.Marshal(t.DiscoveredSchema)
		actual, _ := json.Marshal(current)
		expected, _ = domain.CanonicalJSON(expected)
		actual, _ = domain.CanonicalJSON(actual)
		if !bytes.Equal(expected, actual) {
			return r, false, errors.New("effect schema changed from installed descriptor")
		}
	}

	if t.Endpoint != i.Endpoint || t.Server != i.Server || t.Name != i.Tool {
		return r, false, errors.New("external adapter changed from durable intent")
	}
	raw, e := json.Marshal(i)
	if e != nil {
		return r, false, e
	}
	target := strings.TrimRight(t.Endpoint, "/") + path + url.PathEscape(i.Key)
	var body io.Reader
	if method == http.MethodPost {
		body = bytes.NewReader(raw)
	} else {
		target += "?task=" + url.QueryEscape(i.Task) + "&digest=" + url.QueryEscape(i.Digest)
		if path == "/reads/" {
			target += "&intent=" + url.QueryEscape(string(raw))
		}
	}
	req, e := http.NewRequestWithContext(ctx, method, target, body)
	if e != nil {
		return r, false, e
	}
	if method == http.MethodPost {
		req.GetBody = nil
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", i.Key)
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true, TLSHandshakeTimeout: 10 * time.Second}, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("external redirects refused") }}
	resp, e := client.Do(req)
	if e != nil {
		return r, false, e
	}
	defer resp.Body.Close()
	if method == http.MethodGet && resp.StatusCode == http.StatusNotFound {
		return r, false, nil
	}
	statusError := resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated
	dec := json.NewDecoder(io.LimitReader(resp.Body, 1<<20))
	dec.DisallowUnknownFields()
	if e = dec.Decode(&r); e != nil {
		return r, false, e
	}
	if dec.Decode(new(any)) != io.EOF {
		return r, false, errors.New("trailing external response")
	}
	if statusError {
		return r, false, fmt.Errorf("external status %d", resp.StatusCode)
	}
	return r, true, nil
}

// Query never performs an effect. A not-found observation is not proof that an
// earlier request cannot complete later and must not authorize blind replay.
func Query(ctx context.Context, t Tool, i Intent) (Receipt, bool, error) {
	return requestPath(ctx, t, http.MethodGet, "/effects/", i)
}
func Perform(ctx context.Context, t Tool, i Intent) (Receipt, error) {
	r, _, e := requestPath(ctx, t, http.MethodPost, "/effects/", i)
	return r, e
}

// Read invokes a read-only adapter operation. Reads never use the effects POST
// endpoint, including baseline verification and status polling.
func Read(ctx context.Context, t Tool, i Intent) (Receipt, error) {
	schema, e := new(Discovery).Discover(ctx, t)
	if e != nil {
		return Receipt{}, e
	}
	if t.DiscoveredSchema != nil {
		a, _ := json.Marshal(t.DiscoveredSchema)
		b, _ := json.Marshal(schema)
		a, _ = domain.CanonicalJSON(a)
		b, _ = domain.CanonicalJSON(b)
		if !bytes.Equal(a, b) {
			return Receipt{}, errors.New("read schema changed from installed descriptor")
		}
	}
	if len(schema.OutputSchema) == 0 {
		return Receipt{}, errors.New("read tool requires discovered output schema")
	}
	r, found, e := requestPath(ctx, t, http.MethodGet, "/reads/", i)
	if e != nil {
		return r, e
	}
	if !found {
		return r, errors.New("external read absent")
	}
	if e = Match(t, i, r); e != nil {
		return r, e
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(r.Data))
	decoder.UseNumber()
	if e = decoder.Decode(&value); e != nil {
		return r, e
	}
	if e = ValidateSchema(schema.OutputSchema, value); e != nil {
		return r, e
	}
	return r, nil
}
