package workflow

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/xleelz/fae/domain"
)

type denyLoader struct{}

var compiledSchemas = struct {
	sync.Mutex
	values map[string]*jsonschema.Schema
}{values: map[string]*jsonschema.Schema{}}

func (denyLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("external schema resource forbidden: %s", url)
}
func compileArtifact(schema json.RawMessage) (*jsonschema.Schema, error) {
	raw, e := domain.CanonicalJSON(schema)
	if e != nil {
		return nil, e
	}
	key := string(raw)
	compiledSchemas.Lock()
	cached := compiledSchemas.values[key]
	compiledSchemas.Unlock()
	if cached != nil {
		return cached, nil
	}
	var doc any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if e = dec.Decode(&doc); e != nil {
		return nil, e
	}
	c := jsonschema.NewCompiler()
	c.UseLoader(denyLoader{})
	c.AssertFormat()
	c.AssertVocabs()
	const location = "https://fae.invalid/artifact-schema"
	if e = c.AddResource(location, doc); e != nil {
		return nil, e
	}
	compiled, e := c.Compile(location)
	if e != nil {
		return nil, e
	}
	compiledSchemas.Lock()
	// Keep author-supplied schemas from creating an unbounded process cache.
	if len(compiledSchemas.values) >= 128 {
		compiledSchemas.values = map[string]*jsonschema.Schema{}
	}
	compiledSchemas.values[key] = compiled
	compiledSchemas.Unlock()
	return compiled, nil
}

// ValidateArtifact implements JSON Schema 2020-12 by default. Explicit older
// drafts and local references are supported; network and filesystem loads are not.
// Duplicate object keys are rejected before schema validation.
func ValidateArtifact(schema, raw json.RawMessage) error {
	s, e := compileArtifact(schema)
	if e != nil {
		return fmt.Errorf("schema: %w", e)
	}
	canonical, e := domain.CanonicalJSON(raw)
	if e != nil {
		return e
	}
	var doc any
	dec := json.NewDecoder(bytes.NewReader(canonical))
	dec.UseNumber()
	if e = dec.Decode(&doc); e != nil {
		return e
	}
	return s.Validate(doc)
}
