# Configured commands and formal evidence

The configured-command policy executes an operator-approved literal HostCommand
vector without imposing a Go, Cargo or other language-specific option grammar.
This enables commands such as `cargo kani`, `go run ./cmd/gomela`, a Java verifier
jar or a dedicated normalization wrapper. The broker still requires the exact
vector in its target allowlist, a physical working directory in the task's
worktree, a code-role dispatch and its heavy-operation lease. Existing process
ownership, cancellation, output bounds and declared write collectors remain in
force. A local wrapper executable must resolve inside that worktree. Direct VCS
commands retain their dedicated broker path.

Arguments are passed separately to the executable. They are not joined into a
shell command. The policy does not guess a new tool's flags or silently change its
worker count. Configure those bounds explicitly, for example
`java -Xmx768m -XX:ActiveProcessorCount=1 ...`, alongside the ordinary stage timeout
and host resource admission. Any shell/interpreter behavior explicitly present in
the approved vector belongs to that configured program.

## A command label is not proof

A formal HostCommand declares its tool, parser and literal source files, for
example:

```yaml
formal:
  tool: gobra
  parser: native
  sources: [proof.go]
```

Its command can be an exactly allowlisted vector such as:

```json
["java", "-Xmx768m", "-Xss32m", "-XX:ActiveProcessorCount=1",
 "-jar", "/installed/gobra.jar", "--logLevel", "DEBUG", "-i", "proof.go"]
```

Use installed tools. Namespace views also need the verifier's installation and
runtime dependencies declared as read-only runtime mounts. FAE does not install
verifiers or fetch a tool merely because a workflow names one.

Declared source files are hashed before and after the actual command. Omitting
`sources` binds the task's authoritative Git-tree identity; declare any generated
or scratch proof inputs explicitly. Sources must be
physical relative regular files, without globs, directories, duplicates or links;
the total is bounded to 10,000 files and 64 MiB. A source change invalidates an
otherwise positive report, even when that file was also declared scratch. The
receipt preserves source identity, process status and available executable/tool
identity. An expected configured tool version is never presented as an observed
version; a pin without matching observed evidence cannot pass.

Native parsers recognize tool-specific evidence. A custom verifier or trusted
normalization wrapper can use `parser: normalized-json`, an explicit evidence
kind, and one JSON object on stdout:

```json
{"outcome":"proved","kind":"deductive","obligations":2,
 "verified":2,"failed":0,"unknown":0,"skipped":0}
```

Every count is required and must agree. A positive result requires sufficient
positive obligations, all discharged, no failed/unknown/skipped obligations and
a successful completed process. Zero exit status, a `passed` label, zero checks,
truncated output, timeout, an unsupported report or a malformed report cannot
manufacture proof. Parser failures remain structured nonpassing receipts rather
than discarding the command's actual output or resource cost. Counterexample
search evidence is recorded as such and is not promoted to deductive proof.

For the installed Gobra version, `--logLevel DEBUG` reports explicitly specified
members and the subset that are trusted or abstract. Combined with a completed
zero-error verification and zero reported trusted/abstract members in that package, this supplies
positive checked-contract evidence. Those units are contract-bearing members,
not an invented count of individual solver VCs. Plain `Gobra found 0 errors`
without positive checked units stays nonpassing. Bounds and assumptions remain
part of the proof's stated scope; a parser does not establish verifier soundness.

## Executed checks

Tests run configured subprocess fixtures that emit positive, unknown and malformed
reports, and verify that exact allowlisting and source fingerprints remain
mandatory. These fixtures test transport/normalization, not a real verifier.
A separate test uses the existing Gobra jar and Z3 on a tiny annotated Go method:
the valid contract produces a positive receipt and changing its result violates
the contract. It checks real source and launcher identities. No verifier is
installed by the test; when Java, Z3 or the jar is unavailable, only that native
test is skipped. This is evidence about the tiny fixture, not a proof of FAE or
arbitrary application code.
