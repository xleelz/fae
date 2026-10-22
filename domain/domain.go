// Package domain contains immutable value types and pure validation helpers.
package domain

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

type State string

const (
	Pending   State = "Pending"
	Blocked   State = "Blocked"
	Parked    State = "Parked"
	Escalated State = "Escalated"
	Done      State = "Done"
	Retired   State = "Retired"
	Cancelled State = "Cancelled"
)

func (s State) Terminal() bool { return s == Done || s == Retired || s == Cancelled }
func (s State) Kernel() bool {
	switch s {
	case Pending, Blocked, Parked, Escalated, Done, Retired, Cancelled:
		return true
	}
	return false
}

type Mode string

const (
	Local  Mode = "Local"
	Normal Mode = "Normal"
)

type Disposition struct {
	Mode          Mode   `json:"mode"`
	Live          bool   `json:"live"`
	TicketRoom    string `json:"ticket_room,omitempty"`
	Destination   string `json:"destination,omitempty"`
	PushNamespace string `json:"push_namespace,omitempty"`
}

func (d Disposition) AllowsExternalEffects() bool { return d.Mode == Normal && d.Live }

// CanonicalJSON sorts object keys and preserves JSON number precision. Duplicate
// object keys are refused so distinct source meanings cannot share a digest.
func CanonicalJSON(raw []byte) ([]byte, error) {
	if !utf8.Valid(raw) {
		return nil, fmt.Errorf("invalid UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	v, err := readValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err = dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("trailing JSON content")
	}
	var b bytes.Buffer
	if err = writeCanonical(&b, v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
func readValue(d *json.Decoder) (any, error) {
	t, e := d.Token()
	if e != nil {
		return nil, e
	}
	switch x := t.(type) {
	case json.Delim:
		switch x {
		case '{':
			m := map[string]any{}
			for d.More() {
				k, e := d.Token()
				if e != nil {
					return nil, e
				}
				s, ok := k.(string)
				if !ok {
					return nil, fmt.Errorf("invalid key")
				}
				if _, ok = m[s]; ok {
					return nil, fmt.Errorf("duplicate key %q", s)
				}
				v, e := readValue(d)
				if e != nil {
					return nil, e
				}
				m[s] = v
			}
			_, e = d.Token()
			return m, e
		case '[':
			a := []any{}
			for d.More() {
				v, e := readValue(d)
				if e != nil {
					return nil, e
				}
				a = append(a, v)
			}
			_, e = d.Token()
			return a, e
		default:
			return nil, fmt.Errorf("unexpected delimiter")
		}
	default:
		return t, nil
	}
}
func ContentHash(raw []byte) (string, error) {
	b, e := CanonicalJSON(raw)
	if e != nil {
		return "", e
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:]), nil
}

// ValidateAbsolutePath checks the raw representation; callers must separately
// enforce symlink containment at the time of filesystem use.
func ValidateAbsolutePath(path string) error {
	if !utf8.ValidString(path) || strings.IndexByte(path, 0) >= 0 || !strings.HasPrefix(path, "/") {
		return fmt.Errorf("invalid absolute path")
	}
	if path == "/" {
		return nil
	}
	for _, c := range strings.Split(path[1:], "/") {
		if c == "" || c == "." || c == ".." {
			return fmt.Errorf("noncanonical path component")
		}
	}
	return nil
}

func writeCanonical(b *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			kb, _ := json.Marshal(k)
			b.Write(kb)
			b.WriteByte(':')
			if e := writeCanonical(b, x[k]); e != nil {
				return e
			}
		}
		b.WriteByte('}')
	case []any:
		b.WriteByte('[')
		for i, v := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			if e := writeCanonical(b, v); e != nil {
				return e
			}
		}
		b.WriteByte(']')
	case json.Number:
		s, e := canonicalNumber(string(x))
		if e != nil {
			return e
		}
		b.WriteString(s)
	default:
		raw, e := json.Marshal(v)
		if e != nil {
			return e
		}
		b.Write(raw)
	}
	return nil
}
func canonicalNumber(s string) (string, error) {
	negative := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	exponent := 0
	if p := strings.IndexAny(s, "eE"); p >= 0 {
		var e error
		exponent, e = strconv.Atoi(s[p+1:])
		if e != nil || exponent > 10000 || exponent < -10000 {
			return "", fmt.Errorf("number exponent exceeds canonical bound")
		}
		s = s[:p]
	}
	if p := strings.IndexByte(s, '.'); p >= 0 {
		exponent -= len(s) - p - 1
		s = s[:p] + s[p+1:]
	}
	s = strings.TrimLeft(s, "0")
	if s == "" {
		return "0", nil
	}
	for strings.HasSuffix(s, "0") {
		s = strings.TrimSuffix(s, "0")
		exponent++
	}
	if exponent >= 0 {
		s += strings.Repeat("0", exponent)
	} else if p := len(s) + exponent; p > 0 {
		s = s[:p] + "." + s[p:]
	} else {
		s = "0." + strings.Repeat("0", -p) + s
	}
	if negative {
		s = "-" + s
	}
	return s, nil
}
