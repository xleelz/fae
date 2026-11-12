package provider

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FAE-SEC-003: a provider's raw logs keep no credential. The (fake) provider
// prints, on its event stream and its stderr, an Anthropic-shaped key, an
// OAuth refresh token field and the value of a credential-named variable of
// its environment, and ends stderr without a line break. events.jsonl and
// stderr.log hold "[redacted]" in their place, every event line is still
// JSON, the unfinished last line is kept, and the turn completes. On the
// base, both logs kept all three.
func TestProviderRawLogsKeepNoCredential(t *testing.T) {
	envValue := "planted-provider-env-value-0123456789"
	t.Setenv("FAE_PLANTED_PROVIDER_TOKEN", envValue)
	key := "sk-ant-api03-" + strings.Repeat("Zq7_x-W2", 9)
	refresh := "rt1.Zq8vLm2Np4Rs6Tu8Vw0Xy1Za3Bc5De7Fg9Hi1Jk3"
	c, path, l := providerFixture(t, `printf '%s\n' '{"type":"thread.started","thread_id":"planted-thread"}'
printf '%s\n' '{"type":"error","message":"invalid key `+key+`"}'
printf '{"type":"debug","env":"%s","auth":{"refresh_token":"%s"}}\n' "$FAE_PLANTED_PROVIDER_TOKEN" '`+refresh+`'
printf 'warning: key %s token %s refresh_token=%s' '`+key+`' "$FAE_PLANTED_PROVIDER_TOKEN" '`+refresh+`' >&2`)
	if e := Run(context.Background(), c, path); e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"events.jsonl", "stderr.log"} {
		paths, _ := filepath.Glob(filepath.Join(l.LogDirectory, "provider-*", name))
		if len(paths) != 1 {
			t.Fatal(name, paths)
		}
		raw, e := os.ReadFile(paths[0])
		if e != nil {
			t.Fatal(e)
		}
		text := string(raw)
		for _, secret := range []string{key[:20], key[len(key)-12:], envValue, refresh[:16], refresh[len(refresh)-12:]} {
			if strings.Contains(text, secret) {
				t.Errorf("%s keeps %q:\n%s", name, secret, text)
			}
		}
		if strings.Count(text, "[redacted]") != 3 {
			t.Errorf("%s:\n%s", name, text)
		}
		if name == "stderr.log" && !strings.HasPrefix(text, "warning: key [redacted] token [redacted] refresh_token=[redacted]") {
			t.Errorf("the unfinished last line was lost or changed:\n%s", text)
		}
		if name == "events.jsonl" {
			for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
				var event map[string]any
				if e := json.Unmarshal([]byte(line), &event); e != nil {
					t.Errorf("an event is no longer JSON: %v\n%s", e, line)
				}
			}
			if !strings.Contains(text, `"thread_id":"planted-thread"`) {
				t.Errorf("an ordinary event changed:\n%s", text)
			}
		}
	}
}
