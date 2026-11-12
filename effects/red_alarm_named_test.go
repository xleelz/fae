package effects

import (
	"slices"
	"testing"
)

// The tests go test's own -timeout alarm names are those listed under its
// panic's "running tests:", in Go 1.27's indented form and the unindented
// form of Go 1.23 and earlier; text that only resembles it names none.
func TestGoTestAlarmNamed(t *testing.T) {
	for output, want := range map[string][]string{
		"=== RUN   TestA\npanic: test timed out after 2s\n\trunning tests:\n\t\tTestA (2s)\n\t\tTestB/sub (1s)\n\ngoroutine 7 [running]:\n": {"TestA", "TestB/sub"},
		"panic: test timed out after 2s\nrunning tests:\n\tTestA (2s)\n\ngoroutine 7 [running]:\n":                                          {"TestA"},
		"panic: test timed out after 2s\n\ngoroutine 7 [running]:\n":                                                                        nil,
		"    x_test.go:9: test timed out after 2s\n\trunning tests:\n\t\tTestA (2s)\n":                                                      nil,
		"panic: test timed out after 2s\nrunning tests:\nTestA (2s)\n":                                                                      nil,
		"fixture server crashed: bind: address already in use\n":                                                                            nil,
	} {
		var got []string
		for name := range goTestAlarmNamed(output) {
			got = append(got, name)
		}
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("%q: named %v, want %v", output, got, want)
		}
	}
}
