package domain

import (
	"bytes"
	"testing"
)

func TestCanonical(t *testing.T) {
	a := []byte(`{"b": 1.00, "a":[-0,1e2,9007199254740993]}`)
	b := []byte(`{"a":[0,100,9007199254740993],"b":1}`)
	ca, e := CanonicalJSON(a)
	if e != nil {
		t.Fatal(e)
	}
	cb, e := CanonicalJSON(b)
	if e != nil || !bytes.Equal(ca, cb) {
		t.Fatalf("%s != %s: %v", ca, cb, e)
	}
	ha, _ := ContentHash(a)
	hb, _ := ContentHash(b)
	if ha != hb || len(ha) != 64 {
		t.Fatal(ha, hb)
	}
	for _, s := range []string{`{"a":1,"a":2}`, `{"a":{"x":1,"x":2}}`, `[] {}`, `NaN`, `1e10001`, string([]byte{'"', 255, '"'})} {
		if _, e := CanonicalJSON([]byte(s)); e == nil {
			t.Errorf("accepted %q", s)
		}
	}
}
func TestPaths(t *testing.T) {
	for _, s := range []string{"/", "/a", "/a/b"} {
		if e := ValidateAbsolutePath(s); e != nil {
			t.Error(s, e)
		}
	}
	for _, s := range []string{"", "a", "/a/", "//a", "/a//b", "/a/../b", "/./a", "/a\x00", "/\xff"} {
		if ValidateAbsolutePath(s) == nil {
			t.Error("accepted", s)
		}
	}
}
func TestStatesAndMode(t *testing.T) {
	for _, s := range []State{Pending, Blocked, Parked, Escalated, Done, Retired, Cancelled} {
		if !s.Kernel() {
			t.Fatal(s)
		}
		want := s == Done || s == Retired || s == Cancelled
		if s.Terminal() != want {
			t.Fatal(s)
		}
	}
	if State("custom").Kernel() {
		t.Fatal("custom kernel")
	}
	for _, mode := range []Mode{Local, Normal, "unknown"} {
		for _, live := range []bool{false, true} {
			if (Disposition{Mode: mode, Live: live}).AllowsExternalEffects() != (mode == Normal && live) {
				t.Fatal(mode, live)
			}
		}
	}
}
func FuzzCanonical(f *testing.F) {
	f.Add([]byte(`{"a":1}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		c, e := CanonicalJSON(b)
		if e != nil {
			return
		}
		d, e := CanonicalJSON(c)
		if e != nil || !bytes.Equal(c, d) {
			t.Fatalf("not idempotent: %s %s %v", c, d, e)
		}
	})
}
