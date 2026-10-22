package runtime

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestAsynchronousAndMalformed(t *testing.T) {
	s := &Scripted{Scripts: map[string][]Script{"slow": {{Output: json.RawMessage(`{}`), Delay: time.Hour}}, "fast": {{Output: json.RawMessage(`{}`)}}, "bad": {{Output: json.RawMessage(`no`)}}}}
	ctx, cancel := context.WithCancel(context.Background())
	slow, e := s.Dispatch(ctx, Turn{ID: "s", Task: "a", Role: "slow"})
	if e != nil {
		t.Fatal(e)
	}
	fast, e := s.Dispatch(ctx, Turn{ID: "f", Task: "b", Role: "fast"})
	if e != nil {
		t.Fatal(e)
	}
	select {
	case o := <-fast:
		if o.Err != nil {
			t.Fatal(o.Err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked sibling")
	}
	bad, _ := s.Dispatch(ctx, Turn{ID: "b", Task: "c", Role: "bad"})
	if o := <-bad; o.Class != "malformed-output" {
		t.Fatal(o)
	}
	cancel()
	if o := <-slow; o.Class != "interrupted" {
		t.Fatal(o)
	}
	if _, e = s.Dispatch(ctx, Turn{ID: "s", Task: "a", Role: "slow"}); e == nil {
		t.Fatal("duplicate dispatched")
	}
}
func TestSanitize(t *testing.T) {
	env, d, e := Sanitize([]string{"PATH=/bin", "FAE_HOME=hostile", "CUSTOM_TOKEN=x"}, map[string]string{"FAE_HOME": "isolated"}, []string{"CUSTOM_"})
	if e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(env, []string{"FAE_HOME=isolated", "PATH=/bin"}) {
		t.Fatal(env)
	}
	if len(d.Removed) != 2 || len(d.Set) != 1 {
		t.Fatal(d)
	}
	if _, _, e = Sanitize([]string{"INVALID"}, nil, nil); e == nil {
		t.Fatal("bad entry accepted")
	}
	if _, _, e = Sanitize(nil, map[string]string{"A=B": "x"}, nil); e == nil {
		t.Fatal("bad key accepted")
	}
}
func TestCommandShape(t *testing.T) {
	allowed := [][]string{{"echo", "$(touch bad); hi"}}
	if e := CheckCommand(allowed[0], allowed); e != nil {
		t.Fatal(e)
	}
	for _, args := range [][]string{nil, {"sh", "-c", "anything"}, {"echo", "different"}, {"echo", "nul\x00"}} {
		if CheckCommand(args, allowed) == nil {
			t.Fatal("allowed", args)
		}
	}
}

func TestEmptyPrefixWithoutEnvironment(t *testing.T) {
	if _, _, err := Sanitize(nil, nil, []string{""}); err == nil {
		t.Fatal("empty prefix accepted")
	}
}
