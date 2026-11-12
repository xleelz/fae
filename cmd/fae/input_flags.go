package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/xleelz/fae/daemon"
	"github.com/xleelz/fae/symphony"
)

type inputValues map[string]any

func (v *inputValues) String() string {
	if v == nil {
		return ""
	}
	raw, _ := json.Marshal(v)
	return string(raw)
}
func (v *inputValues) Set(text string) error {
	name, value, ok := strings.Cut(text, "=")
	if !ok || strings.TrimSpace(name) == "" {
		return errors.New("input requires name=value or name=@file")
	}
	if *v == nil {
		*v = inputValues{}
	}
	if _, dup := (*v)[name]; dup {
		return fmt.Errorf("input %s supplied twice", name)
	}
	raw := []byte(value)
	file := strings.HasPrefix(value, "@")
	if file {
		var err error
		raw, err = readBounded(strings.TrimPrefix(value, "@"))
		if err != nil {
			return err
		}
	}
	var decoded any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	err := dec.Decode(&decoded)
	if err == nil {
		var extra any
		if dec.Decode(&extra) != io.EOF {
			err = errors.New("trailing JSON input")
		}
	}
	if err != nil {
		if file {
			decoded, err = symphony.ParseYAMLValue(string(raw))
			if err != nil {
				return fmt.Errorf("input %s: %w", name, err)
			}
		} else {
			decoded = value
		}
	}
	(*v)[name] = decoded
	return nil
}

type parentBindings map[string]string

func (v *parentBindings) String() string { return "" }
func (v *parentBindings) Set(text string) error {
	name, source, ok := strings.Cut(text, "=")
	if !ok || name == "" || source == "" {
		return errors.New("from-parent requires input=artifact")
	}
	if *v == nil {
		*v = parentBindings{}
	}
	if _, dup := (*v)[name]; dup {
		return fmt.Errorf("from-parent input %s supplied twice", name)
	}
	(*v)[name] = source
	return nil
}

type taskInputFlags struct {
	Inputs      *inputValues
	FromParent  *parentBindings
	Parent      *string
	MaxSessions *int
	Flags       *flag.FlagSet
}

func addTaskInputFlags(f *flag.FlagSet) taskInputFlags {
	inputs := inputValues{}
	parents := parentBindings{}
	f.Var(&inputs, "input", "typed input name=value or name=@JSON-or-YAML-file; repeat for each")
	f.Var(&parents, "from-parent", "input=parent-artifact-or-input; repeat for each")
	return taskInputFlags{Flags: f, Inputs: &inputs, FromParent: &parents, Parent: f.String("parent", "", "existing parent task, including a Done task"), MaxSessions: f.Int("max-sessions", -1, "tree session cap; -1 is unbounded")}
}
func (f taskInputFlags) apply(p *daemon.AdmitParams) error {
	if *f.MaxSessions == 0 || *f.MaxSessions < -1 {
		return usage("--max-sessions must be -1 or positive")
	}
	p.Inputs = map[string]any(*f.Inputs)
	p.ParentTask = *f.Parent
	p.FromParent = map[string]string(*f.FromParent)
	f.Flags.Visit(func(v *flag.Flag) {
		if v.Name == "max-sessions" {
			p.MaxSessions = *f.MaxSessions
		}
	})
	return nil
}
