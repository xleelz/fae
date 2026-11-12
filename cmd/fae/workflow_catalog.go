package main

import (
	"encoding/json"
	"fmt"
	"github.com/xleelz/fae/workflow"
)

func loadWorkflowCatalog(path string) (map[string]workflow.Document, error) {
	out := map[string]workflow.Document{}
	if path == "" {
		return out, nil
	}
	raw, err := readBounded(path)
	if err != nil {
		return nil, err
	}
	var entries map[string]json.RawMessage
	if err = json.Unmarshal(raw, &entries); err != nil {
		return nil, err
	}
	if wrapped, ok := entries["workflows"]; ok {
		if err = json.Unmarshal(wrapped, &entries); err != nil {
			return nil, err
		}
	}
	for key, body := range entries {
		d, err := workflow.Parse(body)
		if err != nil {
			return nil, fmt.Errorf("catalog %s: %w", key, err)
		}
		if _, _, err = workflow.ScenarioCatalogRevision(key, d); err != nil {
			return nil, err
		}
		out[key] = d
	}
	return out, nil
}

type catalogFiles []string

func (c *catalogFiles) String() string        { return "" }
func (c *catalogFiles) Set(path string) error { *c = append(*c, path); return nil }
func loadWorkflowCatalogs(paths []string) (map[string]workflow.Document, error) {
	out := map[string]workflow.Document{}
	for _, path := range paths {
		docs, err := loadWorkflowCatalog(path)
		if err != nil {
			return nil, err
		}
		for key, doc := range docs {
			if old, ok := out[key]; ok {
				a, _ := json.Marshal(old)
				b, _ := json.Marshal(doc)
				if string(a) != string(b) {
					return nil, fmt.Errorf("conflicting catalog workflow %s", key)
				}
			}
			out[key] = doc
		}
	}
	return out, nil
}
