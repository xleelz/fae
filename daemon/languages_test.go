package daemon

import "testing"

func TestDerivedRedIdentityAcrossLanguages(t *testing.T) {
	for _, tc := range []struct{ shape, file, id, source, changed string }{
		{"rust-unit", "tests/answer.rs", "checks::answer", "#[test]\nfn answer() { assert_eq!(answer(),42); }", "#[test]\nfn answer() { assert_eq!(answer(),43); }"},
		{"python-pytest", "tests/test_answer.py", "tests/test_answer.py::test_answer[one]", "def test_answer():\n    assert answer() == 42\n", "def test_answer():\n    assert answer() == 43\n"},
		{"node-test", "tests/answer.test.ts", "answer", "test('answer', () => { assert.equal(answer(),42); });", "test('answer', () => { assert.equal(answer(),43); });"},
	} {
		fields := map[string]any{"test_shape": tc.shape, "test_id": tc.id, "declared_files": []string{tc.file}, "expected_failure": "answer differs from expected value"}
		file, id, _, err := liveRedIdentity(fields, []string{tc.file}, true)
		if err != nil || file != tc.file || id != tc.id {
			t.Fatal(tc.shape, file, id, err)
		}
		identity, err := redIdentity(file, []byte(tc.source), id)
		if err != nil || identity == "" {
			t.Fatal(tc.shape, err)
		}
		if err = checkFrozenRed(file, []byte(tc.source), []byte(tc.changed), id); err == nil {
			t.Fatal("changed frozen test accepted", tc.shape)
		}
		if err = liveRedTurnChange(liveTaskScope{Derived: true, Files: []string{file}}, []string{file}); err != nil {
			t.Fatal(tc.shape, err)
		}
	}
}
