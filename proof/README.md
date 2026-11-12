# Formal report interpretation

This package parses reports; it neither runs tools nor proves the verifier or
its adapters sound. The executor must bind the report to the executed command,
source/tree digest, timeout and observed tool version. Configured assumptions
and bounds remain attached to the result. Bounded-model success is explicitly
`bounded-model`, and CrossHair remains `counterexample-search`: even a reported
confirmation never passes the proof gate.

`Parse(Config, Run)` returns `Result` and an error for malformed, inconsistent,
or contradictory reports. Outcomes are `proved`, `disproved`, `unknown`,
`error`, `unsupported`, and `skipped`. `Passed` requires exit zero, a completed
positive report, at least `min_obligations` (default one), and every obligation
verified, with no failed, unknown or skipped checks. Exit zero alone proves
nothing. Counts mean the units the tool actually reports: Verus verified
functions, Kani properties, or Gomela's six safety/model checks per model.
They are not interchangeable measures of proof strength.

Native parsers support Verus's counted summary, Kani's completed harness
summaries, and Gomela's complete per-model safety results. Unreachable Kani
checks count as skipped, so vacuous checks do not pass. Gobra/Nagini bare
success has no discharged-obligation count and is `unknown`. Gobra DEBUG
statistics provide native positive evidence when a completed zero-error report
counts positive explicitly specified members, reports the total methods and
functions, and reports zero trusted or abstract specified members. These
obligation units are contract-bearing members, not individual SMT VCs. Any
trusted/abstract members, missing statistics, timeout, or skipped verification
prevents passage. Positive and mutated-negative DEBUG reports captured from
the installed verifier are retained under `testdata/`. Gobra also supports
an additional explicit `N obligations verified` or `N assertions verified`
line if an adapter obtains that count from the verifier. It does not estimate
counts from source text or zero errors. Gobra errors become `disproved` only
when each has a recognized failed verification-condition diagnostic; type or
translation errors remain errors.

Theorem and TScert native text is deliberately `unsupported`: use an explicit
`normalized-json` adapter that extracts actual completed verification counts.
A certificate status label alone is insufficient. Normalized adapters are
trusted reports from configured commands, not independent rechecking of their
claims. The same adapter contract is available to arbitrary named tools when
an evidence `kind` is explicitly configured.

The normalized format is one JSON object, without unknown or duplicate fields:

```json
{"outcome":"proved","kind":"deductive","obligations":2,"verified":2,"failed":0,"unknown":0,"skipped":0,"tool_version":"1.2"}
```

All five counts are required, nonnegative and bounded; their sum must equal
`obligations`. `kind`, `tool_version` and `detail` are optional. A report cannot
change the kind fixed by a known tool. A pinned version must match an observed
run/report version; copying the expected version into the observed field is
not an observation. Timeouts, truncated reports and missing obligations never
pass. These checks do not establish reachability, adequacy of assumptions,
or correspondence between a concurrency model and production code.

Native formats were checked against the [Verus guide](https://verus-lang.github.io/verus/guide/getting_started_cmd_line.html),
[Kani report documentation](https://model-checking.github.io/kani/verification-results.html),
and repository-captured Gobra/Gomela logs. CrossHair's
[own discussion of confirmation](https://github.com/pschanely/CrossHair/discussions/156)
explains why counterexample search is classified separately. Theorem's
[CLI documentation](https://github.com/theoremts/theorem) and TScert's
[acceptance description](https://github.com/cristianoc/TScert) do not supply a
portable counted native summary accepted by this adapter.

## Existing and future JSON-reporting tools

An arbitrary installed verifier can use `parser: json` without a Go adapter.
Configure `json_fields` with dotted object paths for `outcome`, `obligations`,
`verified`, `failed`, `unknown`, and `skipped`, plus an explicit `status_map`.
For example, a report containing `report.status = "success"` and a nested
`report.counts` object can map those fields to the normalized concepts.
`tool_version` is an optional observed-version path. Arbitrary tools must
explicitly declare an evidence kind; known tools retain their fixed kind.

Only bounded dotted object paths are supported, without expressions, wildcards,
array indexing or scripts. All five counts must exist in the actual report as
integer JSON numbers; strings, fractions and exponent notation are refused.
There are no default counts and no conversion from exit status to proof. Unknown
status values, duplicate JSON keys, overdeep reports, missing fields, and
inconsistent counts fail closed. Other report fields may be ignored, but cannot
override the executor's source digest or the configured evidence kind. This
supports new report layouts without claiming that their tool or model has been
independently checked by FAE.
