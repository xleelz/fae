package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/xleelz/fae/hostguard"
)

// REQUIREMENTS.md's conventions: a "Default N (configurable)" value falls back
// to its default when the configured value is empty, out of range or not
// finite. On the command line that covers the flags carrying a default the
// spec states: Appendix A's, the instance home (FAE-ARC-020), the instance
// name (FAE-CLI-001), serve's adapter, disposition and UI bind (FAE-CLI-010)
// and the ProviderHealth window (FAE-API-001). Two kinds keep a refusal
// instead: runtime-config's limits, which change a running instance, and a
// heavy-op weight, whose fallback to 1 would under-count the operation. A
// fallback is announced with a warning; a token the flag's type cannot parse
// (x for a number, soon for a duration) stays a usage error (2), like any
// malformed flag (FAE-CLI-001).

// configWarnings receives the warnings for configured values that fall back
// to their defaults; tests capture it.
var configWarnings io.Writer = os.Stderr

// warnLine writes one CLI warning in the daemon's log-line form
// (FAE-OBS-001): an RFC 3339 UTC time, the instance, the component and the
// text. Every verb's warnings take this form, and a detached serve's land in
// its rotated serve log.
func warnLine(w io.Writer, instance, text string) {
	fmt.Fprintf(w, "%s instance=%s component=cli warning: %s\n", time.Now().UTC().Format(time.RFC3339Nano), logField(instance), text)
}

var plainLogField = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// logField keeps a log field one token: anything but a plain name is quoted.
func logField(s string) string {
	if plainLogField.MatchString(s) {
		return s
	}
	return strconv.Quote(s)
}

// flagInstance is the instance a flag set resolved, for its warnings.
func flagInstance(f *flag.FlagSet) string {
	if fl := f.Lookup("instance"); fl != nil && fl.Value.String() != "" {
		return fl.Value.String()
	}
	return "default"
}

// warnFallback announces that a flag of command (a flag set's name) fell back
// to its default; accepts is empty for an empty value.
func warnFallback(instance, command, name, value, accepts, def string) {
	text := fmt.Sprintf("fae %s: --%s is empty; using the default %s", command, name, def)
	if accepts != "" {
		text = fmt.Sprintf("fae %s: --%s %s is out of range (%s); using the default %s", command, name, value, accepts, def)
	}
	warnLine(configWarnings, instance, text)
}

// valueRange is the range of values a flag carrying a configurable default
// accepts.
type valueRange struct {
	accepts string         // the range, as the warning states it
	valid   func(any) bool // whether the parsed value is in the range
}

const (
	// maxLandingFloorGiB bounds gate run's landing floor. No host has more
	// memory, and the bound keeps the conversion to bytes far inside int64.
	maxLandingFloorGiB = 1 << 20
	// maxLoopExpiry is the latest loop expiry loops.New accepts.
	maxLoopExpiry = 365 * 24 * time.Hour
)

var (
	positiveDuration = valueRange{"a positive duration", func(v any) bool { return v.(time.Duration) > 0 }}
	positiveCount    = valueRange{"a positive integer", func(v any) bool { return v.(int) > 0 }}
	nonEmpty         = valueRange{"a nonempty value", func(v any) bool { return v.(string) != "" }}
	unboundedLimit   = valueRange{"-1 (unbounded) or 1..1000000", func(v any) bool { n := v.(int); return n == -1 || n >= 1 && n <= 1000000 }}
	zeroKeepsLive    = func(what string) valueRange {
		return valueRange{"0 (the live configuration's " + what + ") or 1..1000000", func(v any) bool { n := v.(int); return n >= 0 && n <= 1000000 }}
	}
	// heavyCapacity is a heavy-op pool size, as the host ledger accepts it.
	heavyCapacity = valueRange{"1..1000000 weight units", func(v any) bool { n := v.(int); return n >= 1 && n <= 1000000 }}
	// landingFloor is gate run's --memory-floor-gib (FAE-HOST-007).
	landingFloor = valueRange{fmt.Sprintf("a finite number of GiB above 0 and at most %d", maxLandingFloorGiB), func(v any) bool { _, ok := landingFloorBytes(v.(float64)); return ok }}
)

// providerHealthDefaults: ProviderHealth's time-series window is a spec
// default (15 min, FAE-API-001); the daemon serves 1 s to 24 h.
var providerHealthDefaults = map[string]valueRange{
	"window": {"1s..24h0m0s", func(v any) bool { d := v.(time.Duration); return d >= time.Second && d <= 24*time.Hour }},
}

// landingFloorBytes converts a landing floor in GiB to bytes. ok is false for
// a floor that is not finite, not above zero, above maxLandingFloorGiB or too
// small to be a byte, so the conversion can never overflow or wrap to the
// negative floor that applies only the soft floor.
func landingFloorBytes(gib float64) (bytes int64, ok bool) {
	if !(gib > 0 && gib <= maxLandingFloorGiB) { // NaN fails every comparison
		return 0, false
	}
	bytes = int64(gib * float64(hostguard.GiB))
	return bytes, bytes > 0
}

// commonDefaults are the common flags that carry a default: the instance home
// (FAE-ARC-020) and the instance name (FAE-CLI-001). Only an empty value
// falls back. A relative home or an invalid name is refused where it is used
// (exit 1), never redirected to the default instance.
var commonDefaults = map[string]valueRange{"home": nonEmpty, "instance": nonEmpty}

// configurable wraps a flag that carries a configurable default, so that an
// empty value keeps the default instead of failing to parse.
type configurable struct {
	flag.Value
	empty bool
}

func (c *configurable) Set(s string) error {
	if c.empty = strings.TrimSpace(s) == ""; c.empty {
		return nil
	}
	return c.Value.Set(s)
}

// String is safe on the zero value, which the flag package builds when it
// prints defaults.
func (c *configurable) String() string {
	if c == nil || c.Value == nil {
		return ""
	}
	return c.Value.String()
}

// configurableFlags applies REQUIREMENTS.md's rule for a "Default N
// (configurable)" value to those of f's flags named in ranges. Call it before
// parsing and the function it returns after: a value that is empty, out of
// range or not finite is replaced by the flag's default, with a warning on
// configWarnings naming the command, the flag, the value and the default. A
// value the flag's type cannot parse stays a usage error (2).
func configurableFlags(f *flag.FlagSet, ranges ...map[string]valueRange) func() {
	lookup := func(name string) (valueRange, bool) {
		for _, m := range ranges {
			if r, ok := m[name]; ok {
				return r, true
			}
		}
		return valueRange{}, false
	}
	f.VisitAll(func(fl *flag.Flag) {
		if _, ok := lookup(fl.Name); !ok {
			return
		}
		if _, wrapped := fl.Value.(*configurable); !wrapped {
			fl.Value = &configurable{Value: fl.Value}
		}
	})
	return func() {
		var fell []*flag.Flag
		f.Visit(func(fl *flag.Flag) {
			r, ok := lookup(fl.Name)
			c, wrapped := fl.Value.(*configurable)
			if !ok || !wrapped {
				return
			}
			switch {
			case c.empty && fl.DefValue == "":
				return // an empty value is the default itself
			case c.empty:
				warnFallback(flagInstance(f), f.Name(), fl.Name, "", "", fl.DefValue)
			case !r.valid(c.Value.(flag.Getter).Get()):
				warnFallback(flagInstance(f), f.Name(), fl.Name, c.String(), r.accepts, fl.DefValue)
			default:
				return
			}
			fell = append(fell, fl)
		})
		for _, fl := range fell {
			_ = fl.Value.(*configurable).Value.Set(fl.DefValue)
		}
	}
}
