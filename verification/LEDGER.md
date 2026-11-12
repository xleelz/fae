# Acceptance ledger for shipped formal checks

`verification/ledger/*.json` maps every formal check that `scripts/verify.sh` runs to
the evidence behind it. It covers each Gobra production guard and each Gomela/SPIN
positive model together with its negative model. For each check it records:

- the requirements it supports;
- the production predicate or code path it targets;
- the Go tests that exercise that code;
- its firing control;
- whether a native daemon-path test reaches it.

It also covers the non-formal checks Fae ships (FAE-VER-001): every validator rule,
shim refusal, role-shim refusal, pre-tool hook denial, landing gate, host admission
or hold reason, bridge refusal class, refusal of account mode's nested git policy
and refusal of the derived task scope, enumerated from its source; the generated
corpora; and refusal and redaction families mapped by their tests (`native-check`
entries in `verification/ledger/checks.json`, `census.json` and `refusals.json`).

`scripts/check-ledger.py` enforces the census. `scripts/test.sh` runs it, so an
unmapped or stale check fails CI. This is bookkeeping toward FAE-VER-001/-002. It is
**not** a certification of any requirement.

## Files

Every `*.json` file in `verification/ledger/` is loaded. Each file is a JSON array of
entries. The split into `core`, `runtime`, `operator` and `lifecycle` is only for
readability, as is the split of the `native-check` entries between `checks.json`,
`census.json` and `refusals.json`. Entry ids must be unique across all files.

## Entry schema

Fields shared by both kinds:

| Field | Meaning |
|---|---|
| `id` | `gobra:<verify_guard name or gobra-NAME.log name>` or `gomela:<model file stem>`. |
| `kind` | `gobra-guard` or `gomela-model`. |
| `requirements` | IDs defined as `**FAE-...**` in REQUIREMENTS.md that the check supports, meaning it enforces part of that requirement's text. Use `[]` when no requirement text covers the check. |
| `unmapped_reason` | Required when `requirements` is empty, and forbidden otherwise. It says why no requirement is claimed, for example an operator-requested backup extension. |
| `production` | `{path, function}`: the production code the check targets. Gobra entries also list `call_sites`, the top-level functions whose bodies call the predicate. |
| `positive.tests` | A non-empty list of Go tests that call the predicate, or its production caller, on the accept path. `note` is a terse description. |
| `native` | Either `"unknown"` or `{tests, path}`. The tests are daemon tests that start a real daemon (`Start(ctx, cfg)`) and reach the production call site through daemon code. `path` is the static chain followed, including any place where a test calls a daemon method directly instead of going through RPC or the drive. |
| `status` | `native-evidenced` when `native` is an object, `native-unknown` when it is `"unknown"`. |
| `limits` | Optional terse caveats, such as accept-path only, a Herdr-dependent skip, or an always-true input. |

Fields for `gobra-guard` entries:

| Field | Meaning |
|---|---|
| `check.source` | The file Gobra verifies. It must equal `production.path`. |
| `firing.mutation` | `{original, weakened}`, exactly as `verify.sh` applies it. |
| `firing.refusal_tests` | Optional list of tests that feed refusing inputs to the predicate or its caller. |

Fields for `gomela-model` entries:

| Field | Meaning |
|---|---|
| `check.model` | `verification/gomela/<stem>.go`. |
| `check.functions` | The model functions declared in that file. |
| `firing.negative_model` | The `verification/negative/*.go.txt` file that verify.sh copies as this model's negative control. |
| `firing.functions` | The functions declared in that negative file. |

Unknown stays unknown. Do not infer native reachability from coverage, test totals or
a related test. Tests under `verification/` only run models as Go code, so they are
rejected as production evidence.

Fields for `native-check` entries (`id` is `check:<name>`):

| Field | Meaning |
|---|---|
| `category` | `validator-rule`, `shim-refusal`, `role-shim-refusal`, `hook-denial`, `landing-gate`, `host-guard-gate`, `bridge-refusal-class`, `nested-git-refusal`, `derived-scope-refusal`, `generated-corpus`, `refusal-family` or `redaction-family`. |
| `check.source`, `check.markers` | For every category but `generated-corpus`, `refusal-family` and `redaction-family`: a census source file and the rules this entry maps, each written as the exact source text the checker enumerates (the arguments of a `bad(...)` or `Issue{...}` in the validator, `workflow/requests.go`'s `validateRequests` included, of a `refuse(...)` in `commandshim/shim.go`, of a `deny(refuse(...))` or `deny(fmt.Errorf(...))` or the reason of a `&LocalRefusal{...}` in `RunShim` (`commandshim/role.go`), the denial text `InstallDenials` writes and the Local-mode text `InstallLocalDenials` adds, the reason expression of a `return ..., true` in `BypassesShim` (`commandshim/hook.go`), a gate name literal (`Gate: "x"` or `verdict("x", ...)`) in `controller/gates.go`, a reason literal in `hostguard` `admit`/`Record`, a refusal class constant in `bridgeRefusalClassKnown` (`runtime/direct.go`), the argument of a `refuse(...)` or the reason of a `&LocalRefusal{...}` in `commandshim/accounted_git.go`, the argument of a `liveScopeRefusal(...)` in `daemon/live_scope.go` or `daemon/live_red_subtests.go`, or of a `return fmt.Errorf(...)` or `return errors.New(...)` in `liveRedTurnChange` (`daemon/live_scope.go`)). |
| `check.corpus` | For `generated-corpus`: the corpus tests. For `refusal-family` and `redaction-family`: the tests that fire the family's refusals, or redact its secret forms. A family's rules live in a source the census does not enumerate, so a rule added there is not noticed; it is mapped so that its tests and requirements are on record. |
| `production` | `{path, function}` of the checking code. |
| `firing.refusal_tests` | Required and non-empty: tests that feed refusing inputs. Every marker must occur verbatim in a file defining one of them, so each rule has a firing case (the Go corpora `TestValidatorRuleCorpus`, `TestRequestPolicyRuleCorpus`, `TestShimRefusalCorpus`, `TestRoleShimRefusalCorpus`, `TestPreToolHookDenialCorpus` and `TestHostGateReasonCorpus` are keyed by these markers and carry their own source-census tests; `TestGateSelfTestProvesEveryGateCanFail` names every landing gate, and the runtime and execution bridge-refusal tests name every refusal class). For `nested-git-refusal` and `derived-scope-refusal` (`IN_TEST_CENSUS`) a marker need not be quoted: a cited test's body must read the source file by its base name and enumerate it with each of the category's patterns for that file, and that test fails unless every rule it finds fires on some input (`TestAccountedGitProtectsTheRunsRepository` for both of `accounted_git.go`'s patterns; `TestLiveScopeRefusalCensus` for `live_scope.go`'s two and `live_red_subtests.go`'s one). |

The census sources are listed in `CENSUS` in `scripts/check-ledger.py`. A rule added to a
source without a ledger entry, a marker that no longer exists, a marker claimed twice and a
marker with no firing control all fail the checker. A pattern that enumerates one kind of
call (`CALL_TOKENS`: `refuse(`, `&LocalRefusal{`, `liveScopeRefusal(`, and
`fmt.Errorf(` or `errors.New(` in `liveRedTurnChange`) holds its source to that one shape:
every such call outside a comment line and a `func` declaration must be the only one on its
line and end it. A refusal split over lines, followed by a comment or sharing a line with
another, which the pattern would not enumerate, fails the checker instead.

## Adding a check

When you add a `verify_guard` line, a hard-coded Gobra run with its `sed` mutation, or
a Gomela model, the census fails until you add a matching entry:

1. Add the entry to the most relevant `verification/ledger/*.json` file. Copy the
   mutation strings verbatim from `verify.sh`.
2. Find the production call sites, for example with
   `grep -rn 'PredicateName(' --include=*.go`. Record the enclosing top-level function
   names.
3. Cite tests that call the predicate or its caller. Also cite a daemon test that
   starts a daemon and reaches the call site, after reading the chain. If you cannot
   confirm one, write `"native": "unknown"` and `"status": "native-unknown"`.
4. Read the requirement text before mapping an ID. If no text covers the check, give
   an `unmapped_reason` instead.
5. Run `python3 -I scripts/check-ledger.py`.

If you remove or rename a check, update or delete its entry. A stale entry is an error.

## What the checker enforces

`python3 -I scripts/check-ledger.py` fails, with one line per problem, when any of the
following holds:

- `verify.sh` contains a check-like line it cannot parse: a Gobra run, a `sed`
  mutation, a `verify_guard` call, or a copy into the Gomela run directory.
- A Gobra check has no firing mutation, or a mutation has no verified check.
- A shipped Gobra check or Gomela model has no ledger entry.
- A ledger entry matches no shipped check.
- A negative model is not the firing control of exactly one entry.
- Any of these does not exist: a referenced file, a production or call-site function,
  or a model or negative-model function.
- A call-site body does not call the predicate.
- A recorded mutation differs from `verify.sh`, or its original text does not occur
  exactly once in the source file.
- A cited test is not defined as `func TestX(t *testing.T)` in a `*_test.go` outside
  `verification/`.
- A requirement ID is not defined in REQUIREMENTS.md.
- `positive.tests` is empty, or a firing control is missing.
- `native` or `status` is malformed or inconsistent, a field is unknown, or
  `unmapped_reason` is missing or misused.
- A census rule is claimed by no entry or by more than one, a claimed marker no longer
  occurs in its source, or a claimed rule has no firing control: no cited refusal test
  quotes it or, for a source with an in-test census, no cited refusal test's body
  reads that source and applies each of its patterns.
- A census source holds a call of the kind a pattern enumerates in another shape
  (`CALL_TOKENS`).

`--report` adds one line for every requirement ID in REQUIREMENTS.md. A requirement
with no ledger entry is listed as `no mapped check`; this is information, not a
failure. The report also lists the unmapped checks with their reasons.

`--self-test` drives the failure modes listed above against synthetic in-memory
inputs.

## Limits

- **Census scope.** The ledger covers the formal checks shipped in `verify.sh`, every
  validator rule (the request policy's included), shim and role-shim refusal,
  pre-tool hook denial, landing gate, host-guard admission or hold reason, bridge
  refusal class, refusal of account mode's nested git policy (each `refuse(...)`,
  exit 77, and `&LocalRefusal{...}`, exit 78, in `commandshim/accounted_git.go`:
  `check:nested-git-refusals`, `check:nested-git-local-refusals`) and refusal of the
  derived task scope (each `liveScopeRefusal(...)` in `daemon/live_scope.go` and
  `daemon/live_red_subtests.go`, and each error `liveRedTurnChange` returns:
  `check:derived-scope-refusals`, `check:red-subtest-refusal`,
  `check:red-turn-change-refusals`), and the generated corpora. Only those call shapes
  in those files are enumerated: a refusal made another way there or in another
  file is outside the census. The shapes it cannot see, in a censused file too
  (re-audit 11's probe): a `&Refusal{...}` literal; a new helper that builds one
  (`refusef(...)`), a function alias or a one-line `func` around `refuse`; a plain
  error (`errors.New`, `fmt.Errorf`) a helper returns, which `RunShim` still turns
  into exit 77; and a refusal in a function the pattern is scoped to (`RunShim`)
  moved out of it. `CALL_TOKENS` refuses only a censused call written in another
  shape (split over lines, followed by a comment, two on a line, in an if
  initializer); it does not know these. Wave 13 brought `role.go`'s
  `&LocalRefusal{...}` in and turned `accounted_git.go`'s two `errors.New`
  refusals into `refuse(...)`, so no uncensused refusal in a censused file is known
  today. It does not enumerate
  `count-tests.py`, this checker itself, runtime refusals outside these sources, or
  daemon RPC refusals. Among the checks outside it:
  - the engine's route refusals (`engine/route_refusal.go`, `check:route-refusals`),
    the RED reason rule's tests (`check:red-reason`), the scope widen
    (`check:scope-widen`) and the protected-path rule `ProtectedPath`
    (`workflow/protected_path.go`, `check:protected-paths`) are mapped as refusal
    families, and secret redaction
    (`daemon/secret_redaction.go`, `secretshape/`, `check:secret-redaction`) as a redaction family,
    by their tests: a rule or secret form added there goes unnoticed;
  - the one path rule `DeclaredPathSafe` (`workflow/command.go`), whose refusals in
    derived scope are enumerated with it;
  - the reasons each provider bridge refuses a turn for (`provider/*.go`); the
    classes they record are enumerated (`check:bridge-refusal-classes`,
    `check:bridge-edits-not-applied`);
  - the landing's comparison of raw and redacted gate output (`controller/land.go`).
  Each has unit tests. An in-test census counts a refusal as fired when one refusal's
  text holds every literal fragment of its message, so a refusal whose literal text
  another one also holds is not told apart from it (re-audit 10 found four such sites:
  three in `accounted_git.go` and one in `live_scope.go`; the per-case expected texts
  fire each today). The census enumerates the
  refusals a source makes, not the inputs it fails to refuse. The ledger does not certify the 284 requirements. Some
  requirements have no mapped check, and a mapped requirement means only that some
  check supports part of its text.
- **Requirements follow the production path.** An entry maps a requirement only
  where production code reaches the checked condition. `gobra:shim` proves
  `CanExecute`'s `!nested` conjunct, but no production caller sets `Nested`: a
  nested call meets the `InstallDenials` stubs (refuse mode; `InstallLocalDenials` for a Local task) or `PrepareAccounted`
  (account mode), neither of which calls `CanExecute`. So `gobra:shim` maps no part
  of FAE-HOST-014; the entries that check those nested paths carry it.
- **Static evidence.** Test names and call-chain descriptions were established by
  reading code. The checker confirms that names, files and call sites exist, but not
  that a test executes the call site or asserts the guarded outcome.
- **Accept-path coverage.** Many native tests exercise only the accept path. Refusal
  is then shown only by unit tests and the formal firing control; `limits` says so
  where known.
- **Formal claims.** Formal checks prove a Boolean predicate or a bounded channel
  protocol. They do not prove input derivation, persistence, Git, Dolt, HTTP,
  provider or OS behavior.
- **Snapshot.** Native tests that need Herdr (`herdr-core`) skip when it is absent. Run the checker
  with `--report` for the current mapped-requirement and native-unknown counts rather
  than relying on any figure written here.

The report separates accepted requirements from blockquoted proposals. Proposal
mappings do not count toward accepted requirement support. A requirement without
a mapped guard remains unassessed by this census; it is not automatically missing
from the implementation. Mappings for orphan termination, actor permissions,
maintenance admission, cron admission and listener binding are limited to the
properties those predicates and their cited tests actually exercise. They do not
claim unrelated history, metrics, CLI or external-effect behavior.
