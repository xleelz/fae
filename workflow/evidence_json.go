package workflow

import (
	"bytes"
	"encoding/json"
	"errors"
)

// EvidenceJSON returns the schema-validated artifact value. List evidence also
// carries an items projection; requiring it to agree prevents split readings.
func EvidenceJSON(a Evidence) ([]byte, error) {
	if len(a.Value) == 0 {
		return json.Marshal(a.Fields)
	}
	if len(a.Fields) != 1 {
		return nil, errors.New("list artifact exposes unhashed metadata")
	}
	if _, exists := a.Fields["items"]; !exists {
		return nil, errors.New("list artifact items projection is missing")
	}
	var items []any
	dec := json.NewDecoder(bytes.NewReader(a.Value))
	dec.UseNumber()
	if err := dec.Decode(&items); err != nil {
		return nil, err
	}
	want, err := json.Marshal(items)
	if err != nil {
		return nil, err
	}
	got, err := json.Marshal(a.Fields["items"])
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(want, got) {
		return nil, errors.New("list artifact items projection differs from value")
	}
	return want, nil
}
