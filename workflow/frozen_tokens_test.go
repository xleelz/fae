package workflow

import "testing"

// FAE-WF-076 for the non-Go shapes: reformatting the RED file (indentation,
// whitespace, comments) or adding a test leaves the identity unchanged;
// changing the declared test's assertion changes it.
func TestTokenTestIdentity(t *testing.T) {
	for name, c := range map[string]struct {
		id, frozen, reformatted, changed string
	}{
		"python": {"test_answer",
			"import app\n\ndef test_answer():\n    assert app.answer() == 42  # the RED\n",
			"import app\n\n\ndef test_answer():\n  # the RED\n  assert   app.answer() == 42\n\ndef test_added():\n    assert True\n",
			"import app\n\ndef test_answer():\n    assert app.answer() == 43\n"},
		"shell braces": {"test_answer",
			"test_answer() {\n  [ \"$(answer)\" = 42 ] || fail \"got $(answer)\"\n}\n",
			"test_other() { true; }\ntest_answer()\n{\n\t[ \"$(answer)\" = 42 ] ||\n\t\tfail \"got $(answer)\"\n}\n",
			"test_answer() {\n  [ \"$(answer)\" = 41 ] || fail \"got $(answer)\"\n}\n"},
		"js": {"answers",
			"it('answers', () => {\n  expect(answer()).toBe(42); // RED\n});\n",
			"it('answers', () => { expect(answer()).toBe(42) ; });\nit('added', () => {});\n",
			"it('answers', () => {\n  expect(answer()).toBe(7);\n});\n"},
	} {
		a, err := TokenTestIdentity([]byte(c.frozen), c.id)
		if err != nil {
			t.Fatal(name, err)
		}
		if err = CheckFrozenTokens([]byte(c.frozen), []byte(c.reformatted), c.id); err != nil {
			t.Error(name, "formatter pass or added test tripped the identity:", err)
		}
		if CheckFrozenTokens([]byte(c.frozen), []byte(c.changed), c.id) == nil {
			t.Error(name, "changed assertion kept the identity")
		}
		if b, _ := TokenTestIdentity([]byte(c.frozen), c.id); a != b || a[:7] != "tokens:" {
			t.Error(name, "identity unstable", a, b)
		}
	}
	if _, err := TokenTestIdentity([]byte("def test_other():\n    pass\n"), "test_answer"); err == nil {
		t.Fatal("absent test accepted")
	}
	if _, err := TokenTestIdentity([]byte("def test_answer_two():\n    pass\n"), "test_answer"); err == nil {
		t.Fatal("prefix of another test accepted as the declared test")
	}
}
