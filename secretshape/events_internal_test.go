package secretshape

import (
	"strings"
	"testing"
)

// joinedEvents leaves out the text between joined events, and unjoin maps a
// span back across it: one ending at a cut ends before the gap, one starting
// at a cut starts after it, and one across a cut takes the gap.
func TestJoinedEventsMapSpansBack(t *testing.T) {
	stream := shapeEvent("p", "T", "abc") + shapeEvent("p", "T", "def\n") + shapeEvent("p", "U", "ghi\n")
	v := joinedEvents([]byte(stream))
	if len(v.gaps) != 1 || !strings.Contains(string(v.b), `"Output":"abcdef\n"`) {
		t.Fatalf("%d gaps: %s", len(v.gaps), v.b)
	}
	at := strings.Index(string(v.b), "abcdef")
	for _, c := range []struct {
		span [2]int
		want string
	}{
		{[2]int{at, at + 3}, "abc"},
		{[2]int{at + 3, at + 6}, "def"},
		{[2]int{at + 1, at + 5}, `bc"}` + "\n" + stream[strings.Index(stream, "\n")+1:strings.Index(stream, "def")] + "de"},
	} {
		got := v.unjoin([][2]int{c.span})[0]
		if text := stream[got[0]:got[1]]; text != c.want {
			t.Errorf("%v -> %v: %q, want %q", c.span, got, text, c.want)
		}
	}
	if v := joinedEvents([]byte("plain text\nwith lines\n")); v.gaps != nil || string(v.b) != "plain text\nwith lines\n" {
		t.Fatal("text without events was changed")
	}
}

// A credential found across joined events spans at most Max bytes of the
// text, the JSON between the events included, so a stream that holds back
// Max bytes holds it whole: limit stops a span where the next byte would
// pass Max, however many events it crosses.
func TestEventViewLimitBoundsASpanInTheText(t *testing.T) {
	var stream strings.Builder
	for i := 0; i < 400; i++ {
		stream.WriteString(shapeEvent("p", "T", strings.Repeat("A", 20)))
	}
	b := []byte(stream.String())
	v := joinedEvents(b)
	if len(v.gaps) != 399 {
		t.Fatalf("%d gaps", len(v.gaps))
	}
	start := strings.Index(string(v.b), "AAAA")
	limit := v.limit(start)
	if got := v.offset(limit, true) - v.offset(start, false); got > Max || limit >= len(v.b) || v.offset(limit+1, true)-v.offset(start, false) <= Max {
		t.Fatalf("limit %d spans %d bytes of the text", limit, got)
	}
	if v := joinedEvents([]byte("plain")); v.limit(1) != 5 {
		t.Fatal("a text without events is limited by its length")
	}
}
