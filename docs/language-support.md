# Programming language support

Fae has scoped command adapters for Go, Rust, Python, and TypeScript. Select the
commands and test shapes in the workflow and pin their original command vectors
in the target allowlist. The built-in workflow's commands still default to Go;
language support does not automatically rewrite an existing pinned workflow.

| Language | Build/check | Selected test shape | Formatter |
| --- | --- | --- | --- |
| Go | Existing `go build`, `go vet` | `go-unit`, `go-integration` | Existing `gofmt` |
| Rust | `cargo build`, `cargo check` | `rust-unit` with `cargo test` | Scoped leaf-file `rustfmt` |
| Python | `python3 -m compileall`, `python3 -m py_compile`, `ruff check` | `python-pytest` | `ruff format` |
| TypeScript | `tsc --project tsconfig.json`, optionally `--noEmit` | `node-test` | No built-in formatter adapter |

For example, a test-run HostCommand can declare one of these pairs:

```json
{"command":["cargo","test","--test","answer"],"test_run":true,"test_shape":"rust-unit"}
```

```json
{"command":["python3","-m","pytest","tests"],"test_run":true,"test_shape":"python-pytest"}
```

```json
{"command":["node","--test","tests/answer.test.ts"],"test_run":true,"test_shape":"node-test"}
```

These are stage fragments: supply the normal role, output artifact, timeout,
weight and routing fields. The task's selected test IDs would respectively be
`answer`, `tests/test_answer.py::test_answer`, and `answer`. Fae adds exact
selection, serial worker settings and its evidence options after matching the
operator's original allowlisted vector. A whole-suite stage omits `test_shape`
and records actual suite execution counts; a selected RED uses its declared shape.

Build/typecheck/format commands can use an ordinary HostCommand check with an
exit-code parser. Declare any emitted files in its `writes`; typecheck-only
commands need no generated outputs. Node can execute erasable TypeScript on a
capable Node release (native tests use Node 24); this does not typecheck or
apply tsconfig transformations. Run `tsc --noEmit` as a separate check. Compiled
JavaScript workflows must compile before testing so they do not test stale
generated files.

Automatically derived RED scopes recognize conventional Python test files,
Rust integration-test files under `tests/` or ending `_test.rs`, and Node
`.test`/`.spec` TypeScript/JavaScript files, alongside Go `_test.go` files.
Other layouts, including Rust inline unit tests, require an operator-pinned
test file. Frozen identities use the source declaration name while runner
evidence verifies the complete qualified test ID.

Role shell tools support the dedicated Cargo, rustfmt, pytest, Ruff and tsc
wrappers. Generic Python and Node interpreters are not globally shadowed,
because provider CLIs can use them to start. Interpreter-based builds/tests
use accounted HostCommand stages. Explicitly allowlisted generator/check
scripts retain their existing behavior and do not gain test-evidence semantics.

Install toolchains and provision dependencies before executing a workflow.
Fae does not install packages, execute arbitrary package-manager scripts, or
claim every ecosystem command is supported. Rust currently uses a private,
offline Cargo home and excludes workspaces, build scripts, custom harnesses,
procedural macros and doctests. Python plugin autoload and caller-controlled
pytest options are constrained. TypeScript config references and output paths
must stay within the declared scope.

Namespace-restricted commands need the relevant toolchain installation in the
target's `runtime_mounts`. In particular, a tsc script needs its installation's
sibling library files, not just the `bin/tsc` entrypoint.

See [Rust](rust-support.md), [Python](python-support.md), and
[TypeScript](typescript-support.md) for exact supported grammars and limitations.
