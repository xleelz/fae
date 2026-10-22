package external

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/xleelz/fae/domain"
)

// ToolSchema is an adapter's installed MCP tools/list descriptor.
type ToolSchema struct {
	Name         string          `json:"name"`
	InputSchema  json.RawMessage `json:"inputSchema"`
	OutputSchema json.RawMessage `json:"outputSchema,omitempty"`
}
type SchemaCatalog struct {
	Server string       `json:"server"`
	Tools  []ToolSchema `json:"tools"`
}
type RoomAction struct {
	Name      string         `json:"name"`
	Tool      string         `json:"tool"`
	Arguments map[string]any `json:"arguments"`
}
type RoomResources struct {
	Server  string       `json:"server"`
	Room    string       `json:"room"`
	Actions []RoomAction `json:"actions"`
}
type roomEntry struct {
	At    time.Time
	Value RoomResources
}

// Discovery has a bounded, per-server/endpoint/room cache. Expiration never
// serves stale workflow actions after a failed refresh. Zero TTL means 1 minute.
type Discovery struct {
	TTL   time.Duration
	Now   func() time.Time
	mu    sync.Mutex
	rooms map[string]roomEntry
}

type schemaDenyLoader struct{}

func (schemaDenyLoader) Load(u string) (any, error) {
	return nil, fmt.Errorf("external schema reference forbidden: %s", u)
}
func compileExternalSchema(raw json.RawMessage) (*jsonschema.Schema, error) {
	var schema any
	canonical, err := domain.CanonicalJSON(raw)
	if err != nil {
		return nil, err
	}
	raw = canonical
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&schema); err != nil {
		return nil, err
	}
	if dec.Decode(new(any)) != io.EOF {
		return nil, errors.New("trailing schema")
	}
	c := jsonschema.NewCompiler()
	c.UseLoader(schemaDenyLoader{})
	c.AssertFormat()
	c.AssertVocabs()
	const id = "https://fae.invalid/external-schema"
	if err := c.AddResource(id, schema); err != nil {
		return nil, err
	}
	compiled, err := c.Compile(id)
	if err != nil {
		return nil, err
	}
	return compiled, nil
}

func ValidateSchema(raw json.RawMessage, value any) error {
	compiled, err := compileExternalSchema(raw)
	if err != nil {
		return err
	}
	// Normalize json.Number/int inputs consistently with the library.
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err = dec.Decode(&value); err != nil {
		return err
	}
	return compiled.Validate(value)
}
func readDiscovery(ctx context.Context, t Tool, path string, value any) error {
	if err := t.Validate(); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(t.Endpoint, "/")+path, nil)
	if err != nil {
		return err
	}
	client := http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DisableKeepAlives: true, TLSHandshakeTimeout: 5 * time.Second}, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("schema redirects refused") }}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("discovery status %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil {
		return err
	}
	if len(raw) > 1<<20 {
		return errors.New("discovery response too large")
	}
	canonical, err := domain.CanonicalJSON(raw)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(canonical))
	dec.UseNumber()
	if err = dec.Decode(value); err != nil {
		return err
	}
	if dec.Decode(new(any)) != io.EOF {
		return errors.New("trailing discovery response")
	}
	return nil
}
func (d *Discovery) Discover(ctx context.Context, t Tool) (ToolSchema, error) {
	return d.discover(ctx, t, nil)
}

// DiscoverTemplate is only for daemon-configured source bindings at startup.
// Concrete calls must still use ValidatePins after substitution.
func (d *Discovery) DiscoverTemplate(ctx context.Context, t Tool, dynamic map[string]string) (ToolSchema, error) {
	return d.discover(ctx, t, dynamic)
}
func (d *Discovery) discover(ctx context.Context, t Tool, dynamic map[string]string) (ToolSchema, error) {
	if err := t.Validate(); err != nil {
		return ToolSchema{}, err
	}
	var catalog SchemaCatalog
	if err := readDiscovery(ctx, t, "/schemas", &catalog); err != nil {
		return ToolSchema{}, err
	}
	if catalog.Server != t.Server {
		return ToolSchema{}, errors.New("schema server differs from exact pin")
	}
	seen := map[string]bool{}
	var found *ToolSchema
	for _, s := range catalog.Tools {
		name, err := CanonicalTool(t.Server, t.Name, s.Name)
		if seen[s.Name] {
			return ToolSchema{}, errors.New("duplicate discovered tool")
		}
		seen[s.Name] = true
		if err == nil && name == t.Name {
			if found != nil {
				return ToolSchema{}, errors.New("ambiguous discovered tool")
			}
			copy := s
			found = &copy
		}
	}
	if found == nil {
		return ToolSchema{}, fmt.Errorf("installed tool %s/%s missing schema", t.Server, t.Name)
	}
	if len(dynamic) > 0 {
		if err := ValidateTemplatePins(*found, t.Arguments, dynamic); err != nil {
			return ToolSchema{}, fmt.Errorf("tool %s: %w", t.Name, err)
		}
	} else if _, err := ValidatePins(*found, t.Arguments); err != nil {
		return ToolSchema{}, fmt.Errorf("tool %s: %w", t.Name, err)
	}
	return *found, nil
}

// ValidatePins returns typed arguments while refusing undeclared keys even when
// a vendor schema permits additional properties. String values remain strings;
// other schema types require a JSON-encoded pin (including booleans/numbers).
func ValidatePins(s ToolSchema, pins map[string]string) (map[string]any, error) {
	var shape struct {
		Type       string                     `json:"type"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(s.InputSchema, &shape); err != nil {
		return nil, err
	}
	if shape.Type != "object" || shape.Properties == nil {
		return nil, errors.New("tool schema requires explicit object properties")
	}
	args := map[string]any{}
	for k, v := range pins {
		property, exists := shape.Properties[k]
		if !CanUseSchemaPin(exists) {
			return nil, fmt.Errorf("pin key %q absent from installed schema", k)
		}
		var p struct {
			Type json.RawMessage `json:"type"`
		}
		if err := json.Unmarshal(property, &p); err != nil {
			return nil, err
		}
		if string(p.Type) == `"string"` {
			args[k] = v
			continue
		}
		var value any
		dec := json.NewDecoder(strings.NewReader(v))
		dec.UseNumber()
		if err := dec.Decode(&value); err != nil {
			return nil, fmt.Errorf("pin %s must encode its schema type: %w", k, err)
		}
		if dec.Decode(new(any)) != io.EOF {
			return nil, errors.New("trailing pin JSON")
		}
		args[k] = value
	}
	if err := ValidateSchema(s.InputSchema, args); err != nil {
		return nil, err
	}
	return args, nil
}
func (d *Discovery) Room(ctx context.Context, t Tool, room string) (RoomResources, error) {
	if room == "" {
		return RoomResources{}, errors.New("room required")
	}
	ttl := d.TTL
	if ttl == 0 {
		ttl = time.Minute
	}
	if ttl < 0 || ttl > 5*time.Minute {
		return RoomResources{}, errors.New("room refresh bound must be at most five minutes")
	}
	now := time.Now()
	if d.Now != nil {
		now = d.Now()
	}
	key := t.Endpoint + "\x00" + t.Server + "\x00" + room
	d.mu.Lock()
	defer d.mu.Unlock()
	if e, ok := d.rooms[key]; ok && now.Sub(e.At) >= 0 && now.Sub(e.At) < ttl {
		return cloneRoom(e.Value), nil
	}
	var r RoomResources
	if err := readDiscovery(ctx, t, "/rooms/"+url.PathEscape(room)+"/resources", &r); err != nil {
		return r, fmt.Errorf("room %s: %w", room, err)
	}
	if r.Room != room || r.Server != t.Server {
		return RoomResources{}, fmt.Errorf("room %s resource identity mismatch", room)
	}
	seen := map[string]bool{}
	for _, a := range r.Actions {
		if a.Name == "" || seen[a.Name] || a.Tool == "" || a.Arguments == nil {
			return RoomResources{}, fmt.Errorf("room %s malformed or duplicate action", room)
		}
		seen[a.Name] = true
	}
	if d.rooms == nil || len(d.rooms) >= 128 {
		d.rooms = map[string]roomEntry{}
	}
	d.rooms[key] = roomEntry{now, cloneRoom(r)}
	return r, nil
}
func cloneRoom(r RoomResources) RoomResources {
	raw, _ := json.Marshal(r)
	var copy RoomResources
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	_ = dec.Decode(&copy)
	return copy
}

// ResolveRoomAction uses only names and arguments returned by the room, then
// verifies the exact installed tool schema. No vendor status vocabulary lives here.
func (d *Discovery) ResolveRoomAction(ctx context.Context, t Tool, room, action string) (Tool, error) {
	return d.ResolveRoomActionWithBindings(ctx, t, room, action, nil)
}

func (d *Discovery) ResolveRoomActionWithBindings(ctx context.Context, t Tool, room, action string, bindings map[string]string) (Tool, error) {
	r, err := d.Room(ctx, t, room)
	if err != nil {
		return Tool{}, err
	}
	for _, a := range r.Actions {
		if a.Name == action {
			result := t
			result.Name = a.Tool
			result.Arguments = map[string]string{}
			// Qualified names are accepted only for this exact server.
			prefix := "mcp__" + t.Server + "__"
			if strings.HasPrefix(result.Name, "mcp__") {
				if !strings.HasPrefix(result.Name, prefix) {
					return Tool{}, errors.New("room action server mismatch")
				}
				result.Name = strings.TrimPrefix(result.Name, prefix)
			}
			for k, v := range a.Arguments {
				if text, ok := v.(string); ok {
					result.Arguments[k] = text
				} else {
					raw, e := json.Marshal(v)
					if e != nil {
						return Tool{}, e
					}
					result.Arguments[k] = string(raw)
				}
			}

			for k, v := range t.Arguments {
				if _, dynamic := bindings[k]; dynamic {
					continue
				}
				if existing, ok := result.Arguments[k]; ok && existing != v {
					return Tool{}, fmt.Errorf("room %s action %s conflicts with installed pin %s", room, action, k)
				}
				result.Arguments[k] = v
			}
			for k, v := range bindings {
				result.Arguments[k] = v
			}
			schema, err := d.Discover(ctx, result)
			if err != nil {
				return Tool{}, fmt.Errorf("room %s action %s: %w", room, action, err)
			}
			result.DiscoveredSchema = &schema
			return result, nil
		}
	}
	return Tool{}, fmt.Errorf("room %s missing configured action %s", room, action)
}

// ValidateIntentSchema refuses replay/adoption under changed argument semantics.
func ValidateIntentSchema(t Tool, i Intent) error {
	if i.SchemaDigest == "" {
		if t.DiscoveredSchema != nil {
			return errors.New("durable intent lacks installed schema pin")
		}
		return nil
	}
	if t.DiscoveredSchema == nil {
		return errors.New("discovered schema required for durable intent")
	}
	digest, err := domain.ContentHash(t.DiscoveredSchema.InputSchema)
	if err != nil || digest != i.SchemaDigest {
		return errors.New("installed schema changed from durable intent")
	}
	args, err := ValidatePins(*t.DiscoveredSchema, i.Arguments)
	if err != nil {
		return err
	}
	a, _ := json.Marshal(args)
	b, _ := json.Marshal(i.TypedArguments)
	if !bytes.Equal(a, b) {
		return errors.New("typed arguments differ from durable pins")
	}
	return nil
}

// ValidateTemplatePins compiles the original complete schema, checks declared
// dynamic keys against their daemon source type, and validates every static
// property. Value and cross-property constraints are rechecked after binding.
func ValidateTemplatePins(s ToolSchema, pins map[string]string, dynamic map[string]string) error {
	if _, err := compileExternalSchema(s.InputSchema); err != nil {
		return err
	}
	var shape struct {
		Type       string                     `json:"type"`
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(s.InputSchema, &shape); err != nil {
		return err
	}
	if shape.Type != "object" || shape.Properties == nil {
		return errors.New("tool schema requires explicit object properties")
	}
	for k, typ := range dynamic {
		property, exists := shape.Properties[k]
		if !CanUseSchemaPin(exists) {
			return fmt.Errorf("dynamic pin key %q absent from installed schema", k)
		}
		var p struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(property, &p); err != nil {
			return err
		}
		if (typ != "string" && typ != "integer") || p.Type != typ {
			return fmt.Errorf("dynamic pin %s source type %s differs from schema %s", k, typ, p.Type)
		}
	}
	partial := map[string]any{"type": "object", "properties": map[string]json.RawMessage{}, "additionalProperties": false}
	props := partial["properties"].(map[string]json.RawMessage)
	statics := map[string]string{}
	for k, v := range pins {
		if _, ok := dynamic[k]; ok {
			continue
		}
		property, exists := shape.Properties[k]
		if !CanUseSchemaPin(exists) {
			return fmt.Errorf("pin key %q absent from installed schema", k)
		}
		props[k] = property
		statics[k] = v
	}
	for _, k := range shape.Required {
		if _, ok := dynamic[k]; !ok {
			if _, exists := pins[k]; !exists {
				return fmt.Errorf("required static pin %s missing", k)
			}
		}
	}
	raw, _ := json.Marshal(partial)
	_, err := ValidatePins(ToolSchema{InputSchema: raw}, statics)
	return err
}
