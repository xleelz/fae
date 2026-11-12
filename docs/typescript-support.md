# TypeScript support

FAE supports scoped TypeScript typechecking/emission and the built-in Node test
runner. Tools must already be installed and on the target's pinned PATH; FAE does
not download packages or run installation hooks.

## Recommended source-test workflow

With Node 24.12 or newer, use native erasable TypeScript tests and a separate
compiler check:

```json
["tsc", "--project", "tsconfig.json", "--noEmit"]
["node", "--test", "test/arithmetic.test.ts"]
```

Declare `test_shape: node-test` and a literal `test_id`, for example `adds_numbers`,
for a named RED test. The broker constructs an anchored, escaped name selector
and requires exactly that one test to execute. Use identifier-like names accepted
by the workflow's test identifier grammar. An unshaped, explicitly allowlisted
`node --test <file>` may run all tests in that one file; its evidence counts
executed leaves, including nested suites, without inventing a RED match.

A minimal test needs no third-party runtime:

```ts
import { test } from 'node:test';
import assert from 'node:assert/strict';

const result: number = 3;
test('adds_numbers', () => assert.equal(result, 3));
```

Node's type stripping does **not** typecheck or use `tsconfig.json` transforms.
It supports erasable `.ts`, `.mts` and `.cts`, not `.tsx`, enums, parameter
properties, runtime namespaces, custom loaders or path-alias rewriting. Install
Node type declarations in the project when its compiler check needs them.
See the [official Node TypeScript documentation](https://nodejs.org/api/typescript.html).

## Compiled JavaScript workflow

Projects needing compiler transforms can compile to an explicit in-worktree
output directory and test the generated file:

```json
["tsc", "--project", "tsconfig.json"]
["node", "--test", "dist/arithmetic.test.js"]
```

For example, `compilerOptions` can declare `rootDir: "src"`, `outDir: "dist"`,
`target: "ES2022"` and `module: "commonjs"`. Declare generated output globs as
workflow scratch/check writes so they are retained for the test and excluded from
commits. Compile before each RED/GREEN/QA run; Node runner freshness proves actual
uncached execution, not that generated JavaScript was rebuilt. Native source tests
avoid this stale-output dependency when their syntax is supported.

## Bounds and evidence

Compiler commands require one explicit `--project`/`-p`. JSONC comments and trailing
commas are supported. Config inheritance, references, inputs, aliases and output
paths must remain inside the worktree; escaping symlinks, executable compiler
plugins, watch/build mode and diagnostic file-output flags are refused. Emission
requires an explicit `outDir`; output directories must satisfy the ordinary
on-disk/scoped-write policy. Compiler options may narrow output paths, select
`--noEmit`, or request declaration/incremental output inside that scope.

Node commands require one literal JavaScript or supported TypeScript file, without
file globs. The broker fixes `--test-reporter=tap`, `--test-concurrency=1`,
`--v8-pool-size=1` and `UV_THREADPOOL_SIZE=1`, and disables Node's environment-driven
compile cache. Runtime injection via `NODE_OPTIONS`, `NODE_PATH`, loaders,
external coverage/cache/warning destinations and TS runtime wrappers is refused.
These clamps complement the existing host lease and process-group accounting;
they do not claim arbitrary test code cannot create its own work.

The parser checks complete TAP plans, ordered outcomes, suite/leaf counts, summary
counts and process status. Skipped/TODO tests do not count as executed. No matching
test, an empty module's file-wrapper result, module-load errors, cancelled tests,
truncation and contradictory evidence fail rather than creating a green result.
Expected RED failure also requires the selected test to fail and the configured
failure text to appear in its actual run output.

`node --test` runs through a HostCommand. Node is not globally PATH-shimmed, because
provider CLIs and compiler scripts may use its interpreter shebang. Interactive
role-shell interpreter test invocations direct operators to the HostCommand path.
The scoped `tsc` adapter remains available to granted build roles. Arbitrary npm,
pnpm, yarn, ts-node, tsx, Jest/Vitest and Prettier scripts are not supported by these
adapters; a separate scoped adapter is required before claiming those integrations.

Local acceptance covers real TypeScript 5.9.3 typecheck/scoped emission/type-error
rejection, and real Node 24.22.0 pass/fail/skip/no-tests/nested-suite evidence. A
native TypeScript test also runs through the actual HostCommand broker, then its
source changes to prove the subsequent RED run observes the new source. Tests use
installed tools or an offline provisioned compiler; they never install dependencies.
