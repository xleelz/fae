// Package proof interprets verifier reports. It does not execute a verifier,
// trust an exit code as proof, or establish the soundness of a verifier.
package proof

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

type Outcome string

const (
	Proved      Outcome = "proved"
	Disproved   Outcome = "disproved"
	Unknown     Outcome = "unknown"
	Error       Outcome = "error"
	Unsupported Outcome = "unsupported"
	Skipped     Outcome = "skipped"
)

type Kind string

const (
	Deductive            Kind = "deductive"
	BoundedModel         Kind = "bounded-model"
	CounterexampleSearch Kind = "counterexample-search"
	Certificate          Kind = "certificate"
)

type JSONFields struct {
	Outcome     string             `json:"outcome"`
	Obligations string             `json:"obligations"`
	Verified    string             `json:"verified"`
	Failed      string             `json:"failed"`
	Unknown     string             `json:"unknown"`
	Skipped     string             `json:"skipped"`
	ToolVersion string             `json:"tool_version,omitempty"`
	StatusMap   map[string]Outcome `json:"status_map"`
}

type Config struct {
	JSONFields     *JSONFields       `json:"json_fields,omitempty"`
	Tool           string            `json:"tool"`
	Parser         string            `json:"parser,omitempty"`
	Kind           Kind              `json:"kind,omitempty"`
	Sources        []string          `json:"sources,omitempty"`
	ToolVersion    string            `json:"tool_version,omitempty"`
	Bounds         map[string]string `json:"bounds,omitempty"`
	Assumptions    []string          `json:"assumptions,omitempty"`
	MinObligations int               `json:"min_obligations,omitempty"`
}
type Run struct {
	Stdout, Stderr            string
	ExitCode                  int
	TimedOut, Truncated       bool
	SourceDigest, ToolVersion string
}
type Result struct {
	Outcome      Outcome           `json:"outcome"`
	Kind         Kind              `json:"kind"`
	Tool         string            `json:"tool"`
	Parser       string            `json:"parser"`
	Obligations  int               `json:"obligations"`
	Verified     int               `json:"verified"`
	Failed       int               `json:"failed"`
	Unknown      int               `json:"unknown"`
	Skipped      int               `json:"skipped"`
	Passed       bool              `json:"passed"`
	SourceDigest string            `json:"source_digest,omitempty"`
	ToolVersion  string            `json:"tool_version,omitempty"`
	Bounds       map[string]string `json:"bounds,omitempty"`
	Assumptions  []string          `json:"assumptions,omitempty"`
	Detail       string            `json:"detail,omitempty"`
}

var toolName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]*$`)

func toolKind(tool string) Kind {
	switch tool {
	case "gobra", "verus", "nagini", "theorem":
		return Deductive
	case "gomela", "kani":
		return BoundedModel
	case "crosshair":
		return CounterexampleSearch
	case "tscert":
		return Certificate
	}
	return ""
}
func ValidKind(k Kind) bool {
	return k == Deductive || k == BoundedModel || k == CounterexampleSearch || k == Certificate
}
func Validate(c Config) error {
	if !toolName.MatchString(c.Tool) {
		return fmt.Errorf("formal tool must be a literal tool identifier")
	}
	if c.Parser != "" && c.Parser != "native" && c.Parser != "normalized-json" && c.Parser != "json" {
		return fmt.Errorf("formal parser must be native, normalized-json or json")
	}
	if toolKind(c.Tool) == "" && ((c.Parser != "normalized-json" && c.Parser != "json") || !ValidKind(c.Kind)) {
		return fmt.Errorf("custom formal tool requires a JSON report parser and explicit evidence kind")
	}
	if c.Parser == "json" {
		if e := validateJSONFields(c.JSONFields); e != nil {
			return e
		}
	} else if c.JSONFields != nil {
		return fmt.Errorf("formal json_fields requires parser json")
	}
	if c.Kind != "" && !ValidKind(c.Kind) {
		return fmt.Errorf("unsupported formal evidence kind")
	}
	if k := toolKind(c.Tool); k != "" && c.Kind != "" && c.Kind != k {
		return fmt.Errorf("formal tool %s requires evidence kind %s", c.Tool, k)
	}
	if c.MinObligations < 0 || c.MinObligations > 1000000000 {
		return fmt.Errorf("formal minimum obligations must be between 1 and 1000000000 when set")
	}
	for _, s := range c.Sources {
		if s == "" || strings.Contains(s, "\\") || !filepath.IsLocal(s) || filepath.Clean(s) != s {
			return fmt.Errorf("formal sources must be clean relative paths")
		}
	}
	return nil
}
