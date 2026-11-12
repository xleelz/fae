package workflow

import (
	"bytes"
	"encoding/json"
)

// Presence distinguishes omitted defaults from an explicit invalid zero. The
// flags do not participate in content identity except by preserving that zero.
func (d *Document) UnmarshalJSON(raw []byte) error {
	type plain Document
	var v plain
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	decodeErr := decoder.Decode(&v)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	*d = Document(v)
	_, d.maxSessionsSet = fields["max_sessions"]
	_, d.sessionCountSet = fields["session_count"]
	return decodeErr
}
func (d Document) MarshalJSON() ([]byte, error) {
	type plain Document
	raw, err := json.Marshal(plain(d))
	if err != nil {
		return nil, err
	}
	if !d.maxSessionsSet && !d.sessionCountSet {
		return raw, nil
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	if d.maxSessionsSet && d.MaxSessions == 0 {
		fields["max_sessions"] = json.RawMessage("0")
	}
	if d.sessionCountSet && d.SessionCount == 0 {
		fields["session_count"] = json.RawMessage("0")
	}
	return json.Marshal(fields)
}
