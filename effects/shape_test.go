package effects

import (
	"context"
	"slices"
	"testing"
	"time"
)

func TestGoUnitShapeNarrowsToPinnedTest(t *testing.T) {
	b, w, _ := setup(t)
	write(t, w.Path, "y_test.go", "package test\nimport \"testing\"\nfunc TestXY(t *testing.T) {}\nfunc TestY(t *testing.T) {}\n")
	b.Allowed = [][]string{{"go", "test", "-json", "-count=1", "-a", "-x", "./..."}}
	ctx := context.Background()
	whole, err := b.Run(ctx, w, Command{Argv: b.Allowed[0], Timeout: time.Minute, TestID: "TestX"})
	if err != nil || whole.ExecutedCount != 3 || whole.MatchedCount != 1 {
		t.Fatalf("whole suite %+v %v", whole, err)
	}
	narrow, err := b.Run(ctx, w, Command{Argv: b.Allowed[0], Timeout: time.Minute, TestID: "TestX", Shape: "go-unit"})
	if err != nil || !narrow.FreshBuild || narrow.ExecutedCount != 1 || narrow.MatchedCount != 1 || !narrow.RedPassed {
		t.Fatalf("declared shape %+v %v", narrow, err)
	}
	for name, c := range map[string]Command{
		"unknown shape":   {Argv: b.Allowed[0], TestID: "TestX", Shape: "rust-integration"},
		"missing test id": {Argv: b.Allowed[0], Shape: "go-unit"},
		"regexp test id":  {Argv: b.Allowed[0], TestID: "TestX|TestY", Shape: "go-unit"},
	} {
		c.Timeout = time.Minute
		if _, err = b.Run(ctx, w, c); err == nil {
			t.Fatal("refused shape accepted:", name)
		}
	}
}

func TestGoUnitShapeVector(t *testing.T) {
	got, err := shapeArgv([]string{"go", "test", "-json", "-count=1", "-a", "-x", "./..."}, "go-unit", "TestBehavior")
	if err != nil || !slices.Equal(got, []string{"go", "test", "-json", "-count=1", "-a", "-x", "-run=^TestBehavior$", "./..."}) {
		t.Fatal(got, err)
	}
	got, err = shapeArgv([]string{"go", "test", "-json", "-p", "1", "./a", "./b"}, "go-unit", "TestBehavior")
	if err != nil || !slices.Equal(got, []string{"go", "test", "-json", "-p", "1", "-run=^TestBehavior$", "./a", "./b"}) {
		t.Fatal(got, err)
	}
	for _, argv := range [][]string{{"gofmt", "-l", "."}, {"go", "vet", "./..."}, {"go", "test", "-run=^TestOther$", "./..."}, {"go", "test", "-json", "-run", "TestOther", "./..."}} {
		if _, err = shapeArgv(argv, "go-unit", "TestBehavior"); err == nil {
			t.Fatal("unsupported vector narrowed", argv)
		}
	}
}
