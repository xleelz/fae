package execution

import (
	"sort"

	hosteffects "github.com/xleelz/fae/effects"
)

// FeedbackRun is one failed host-command run among a turn's input artifacts,
// as the engine_feedback brief block carries it. Excerpt is the plain-text
// failure excerpt stored when the run failed (effects.FailureExcerpt); it is
// empty for a run recorded without one.
type FeedbackRun struct {
	Stage            string   `json:"stage"`
	DispatchID       string   `json:"dispatch_id"`
	Command          []string `json:"command"`
	ExitCode         int      `json:"exit_code"`
	ExecutedCount    int      `json:"executed_count"`
	MatchedCount     int      `json:"matched_count"`
	Excerpt          string   `json:"excerpt"`
	ExcerptTruncated bool     `json:"excerpt_truncated"`
}

// FeedbackTotalLimit bounds the excerpts of one brief together; each is also
// bounded by effects.ExcerptLimit.
const FeedbackTotalLimit = 48 << 10

// engineFeedbackBlock is the engine_feedback brief block, or nil (no block)
// when the turn has no failed run to show.
func engineFeedbackBlock(runs []FeedbackRun) any {
	if len(runs) == 0 {
		return nil
	}
	return map[string]any{"kind": "engine_feedback", "runs": boundFeedback(runs)}
}

// boundFeedback holds every excerpt to effects.ExcerptLimit and all of them
// to FeedbackTotalLimit. Over the total, the shortest keep their text and the
// rest share what remains equally, each cut around its middle and flagged.
func boundFeedback(runs []FeedbackRun) []FeedbackRun {
	out := append([]FeedbackRun(nil), runs...)
	total := 0
	for i := range out {
		text, cut := hosteffects.BoundExcerpt(out[i].Excerpt, hosteffects.ExcerptLimit)
		out[i].Excerpt, out[i].ExcerptTruncated = text, out[i].ExcerptTruncated || cut
		total += len(text)
	}
	if total <= FeedbackTotalLimit {
		return out
	}
	order := make([]int, len(out))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return len(out[order[a]].Excerpt) < len(out[order[b]].Excerpt) })
	budget := FeedbackTotalLimit
	for k, i := range order {
		share := budget / (len(order) - k)
		if len(out[i].Excerpt) > share {
			out[i].Excerpt, _ = hosteffects.BoundExcerpt(out[i].Excerpt, share)
			out[i].ExcerptTruncated = true
		}
		budget -= len(out[i].Excerpt)
	}
	return out
}

// briefBlocks drops absent (nil) blocks from a brief's ordered block list.
func briefBlocks(blocks ...any) []any {
	out := make([]any, 0, len(blocks))
	for _, b := range blocks {
		if b != nil {
			out = append(out, b)
		}
	}
	return out
}
