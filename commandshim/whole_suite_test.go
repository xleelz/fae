package commandshim

import (
	"reflect"
	"testing"
)

// FAE-HOST-015: a -run value that selects every test (empty, ".", ".*",
// "^.*$", "Test", one alternative of them, or one before a subtest level)
// does not narrow a recursive go test, so it runs as a whole suite at one
// thread on the engine path (Prepare); a -run that narrows it, a later -run
// that narrows it, or a single package keeps the clamp of 2. Before, any
// -run counted as a filter.
func TestEnginePathRunThatSelectsEveryTestIsAWholeSuite(t *testing.T) {
	p := policy(t)
	for _, c := range []struct {
		flags   []string
		threads string
	}{
		{[]string{"-run="}, "1"}, {[]string{"-run=."}, "1"}, {[]string{"-run=.*"}, "1"}, {[]string{"-run=^.*$"}, "1"},
		{[]string{"-run=.+"}, "1"}, {[]string{"-run=^"}, "1"}, {[]string{"-run=Test"}, "1"}, {[]string{"-run=^Test"}, "1"},
		{[]string{"-run=TestX|.*"}, "1"}, {[]string{"-run=.*/Sub"}, "1"}, {[]string{"-run=/Sub"}, "1"}, {[]string{"-run=TestX", "-run=.*"}, "1"},
		{[]string{"-run=TestX"}, "2"}, {[]string{"-run=^$"}, "2"}, {[]string{"-run=TestX/.*"}, "2"}, {[]string{"-run=(TestX|.*)Y"}, "2"},
		{[]string{"-run=[|].*"}, "2"}, {[]string{"-run=X\\|.*"}, "2"}, {[]string{"-run=.*", "-run=TestX"}, "2"},
		// Total patterns off wave 11's fixed list (re-audit 10), and near misses.
		{[]string{"-run=Test.*"}, "1"}, {[]string{"-run=(.*)"}, "1"}, {[]string{"-run=x*"}, "1"}, {[]string{"-run=^T"}, "1"},
		{[]string{"-run=(TestX|.*)"}, "1"}, {[]string{"-run=^Test[A-Z]"}, "2"}, {[]string{"-run=^Test.+"}, "2"},
	} {
		argv := append(append([]string{"go", "test", "-json", "-count=1"}, c.flags...), "./...")
		got, err := p.Prepare(argv)
		want := append(append([]string{"go", "test", "-p=" + c.threads, "-parallel=" + c.threads, "-json", "-count=1"}, c.flags...), "./...")
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%q: got %q %v, want %q", argv, got, err, want)
		}
	}
	// One package is not a whole suite, whatever its -run.
	if got, err := p.Prepare([]string{"go", "test", "-run=.*", "."}); err != nil || got[2] != "-p=2" {
		t.Errorf("single package: %q %v", got, err)
	}
}

// FAE-HOST-015: the same for a nested go test (PrepareAccounted), with -run
// or -test.run given as a separate argument or attached, and after -args,
// where the test binary takes the last -test.run.
func TestNestedRunThatSelectsEveryTestIsAWholeSuite(t *testing.T) {
	p, _ := accountedGoRun(t)
	test := func(args ...string) []string { return append([]string{"go", "test"}, args...) }
	ran := func(threads string, args ...string) []string {
		return append([]string{"go", "test", "-p=" + threads, "-parallel=" + threads}, args...)
	}
	for _, c := range []struct{ argv, want []string }{
		{test("-run=", "./..."), ran("1", "-run=", "./...")},
		{test("-run", "", "./..."), ran("1", "-run", "", "./...")},
		{test("-run=.", "./..."), ran("1", "-run=.", "./...")},
		{test("-run", ".*", "all"), ran("1", "-run", ".*", "all")},
		{test("-test.run=^.*$", "./..."), ran("1", "-test.run=^.*$", "./...")},
		{test("-run=X|.*", "./..."), ran("1", "-run=X|.*", "./...")},
		{test("-run=X", "./...", "-args", "-test.run=.*"), ran("1", "-run=X", "./...", "-args", "-test.run=.*")},
		{test("-run=X", "./..."), ran("2", "-run=X", "./...")},
		{test("-run=.*", "./...", "-args", "-test.run", "X"), ran("2", "-run=.*", "./...", "-args", "-test.run", "X")},
		{test("-run=.*", "."), ran("2", "-run=.*", ".")},
		{test("-run=Test.*", "./..."), ran("1", "-run=Test.*", "./...")},
		{test("-test.run", "(.*)", "./..."), ran("1", "-test.run", "(.*)", "./...")},
		{test("-run=X", "./...", "-args", "-test.run=^T"), ran("1", "-run=X", "./...", "-args", "-test.run=^T")},
		{test("-run=^Test[A-Z]", "./..."), ran("2", "-run=^Test[A-Z]", "./...")},
	} {
		got, err := p.PrepareAccounted(c.argv, nil)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q: got %q %v, want %q", c.argv, got, err, c.want)
		}
	}
}
