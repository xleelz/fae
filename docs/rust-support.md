# Native Rust commands

The native command policy supports `cargo build`, `cargo check`, `cargo test`,
and explicit, bounded leaf-file `rustfmt` invocations. These commands use the
existing host-heavy lease and nested-command accounting. Compilation is clamped
to one job; libtest execution is clamped to one thread. The `rust-unit` test
shape selects one literal test with libtest `--exact`, defaulting to `--lib`;
`--test NAME` selects one integration-test binary instead.

Unshaped `cargo test` runs native test targets with `--tests`; documentation
tests require a separate adapter and are excluded.

Cargo uses a private Cargo home and target directory, an absolute trusted rustc,
and `--offline --locked`. Supply a checked-in lockfile. This baseline supports
standard-library-only crates without downloads. It does not provision dependency
caches or install toolchains. It refuses workspaces, build scripts, procedural
macro declarations, custom harnesses, Cargo configuration files in source
ancestors, arbitrary Cargo options, escaped manifest strings, nonliteral path declarations,
and manifest paths outside the worktree.
Dependency builds needing unavailable offline packages fail normally; they do
not silently use a developer's Cargo home. Compiler and Cargo environment
configuration is removed before launch.

Rustfmt accepts explicit `.rs` files, each at most 1 MiB and 30,000 lines, with
optional `--check`, `--files-with-diff`, and `--edition`. It ignores project
configuration using an explicit empty configuration. Files declaring external
modules or a `#[path]` attribute are refused because rustfmt can follow those
modules; format their leaf files individually. The conservative lexical check
can also refuse such declarations inside comments or strings.

Selected-test evidence requires a complete libtest result and exactly one
executed selected test. Compilation errors, ignored tests, zero matches,
truncated output and timeouts cannot establish selected-test evidence. Expected
failure text must occur in the selected test's captured failure section.

Cargo's documented jobs limit controls compilation, while libtest has a separate
thread limit. See the [Cargo test reference](https://doc.rust-lang.org/cargo/commands/cargo-test.html)
and [rustfmt usage](https://github.com/rust-lang/rustfmt#running).
