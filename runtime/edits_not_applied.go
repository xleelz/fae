package runtime

// EditsNotApplied is the typed bridge refusal of a completed turn whose
// answer passed every check but whose edits the bridge could not write (a
// failed write, rename or handoff write). The bridge writes a turn's edits
// all or nothing: on such a failure it restores every file it had written,
// so the worktree is as the turn found it, and records this class. Nothing
// of the answer reached the task and the failure is the bridge's, not the
// provider's, so the executor types the turn a ValidationFailed, a bounded
// stage retry that charges no provider health (FAE-ROB-016, FAE-VCS-003).
// A bridge that could not restore the worktree records no class: that turn
// stays uncertain.
const EditsNotApplied = "edits-not-applied"
