# Gate AST, version 1

Gates are JSON syntax trees, not executable strings. `workflow.Parse` rejects unknown object keys. `ValidateExpr` checks operation names, arity and selector requirements. Gate results are three-valued: only `true` passes. `false` and `unknown` fail.

```text
expression := literal | lookup | unary | binary
literal    := {"op":"literal", "value": JSON-value}
lookup     := {"op":"field", selector, "path": [string, ...], ["all":boolean], ["enum":[string,...]]}
            | {"op":"exists", selector}
            | {"op":"count", selector, ["args":[expression]]}
selector   := "type":string | "stage":string | "type":string,"stage":string
unary      := {"op":"not", "args":[expression]}
binary     := {"op":binary-op, "args":[expression,expression]}
binary-op  := "and" | "or" | "eq" | "ne" | "lt" | "le" | "gt" | "ge" | "prefix" | "in"
```

`field` selects the most recently recorded matching evidence by slice order, or all matches when `all` is true. Field paths traverse object keys. `count` returns the number of matches; its optional filter is evaluated with one matching record in scope. Unknown filters make the whole count unknown. `exists` returns false for an empty selection. Missing fields, missing field-selected artifacts, unrecognized declared enum values, invalid operations and type mismatches return unknown with a reason. Comparisons support exact decimal numbers; strings and booleans support equality, strings support prefix, and `in` accepts an array of same-typed literals.

Boolean operations use strong Kleene logic: false AND unknown is false; true OR unknown is true; otherwise an unknown operand propagates. Both operands are evaluated for an audit trace. Results record every leaf and the sorted distinct evidence IDs read.

Expression nesting is limited to 64 and evaluation to 10,000 node visits. Number representations/exponents have a 10,000-character/magnitude bound. The language has no recursion, user functions, I/O, clock, random values or executable loops.

The validator conservatively requires a positive-evidence witness for each referenced collection on every passing disjunct. Tests must additionally assert `exit_code == 0` and `executed_count >= 1` on their named host-command stage. This restricted proof rule can reject otherwise safe complex expressions; it never assumes an arbitrary predicate implies non-emptiness. An emptiness waiver needs both `empty_allowed: true` and a nonempty `empty_reason`, and cannot waive test-run counts.

The current AST is a foundational subset of the full requirements: it does not implement schema-derived field typing, array indexing, all primitive configurations, or the default workflow content.

## Durable interpreter extensions

Documents may declare `code_bearing: true`. Such graphs must traverse test-first
phase, a positive RED/right-failure gate, freezing, implementation, comparison,
and a gate asserting that comparison is unchanged before committing/integrating.
`phase` is closed to `test-first` and `implement`. Review stages support `reviewers`
(role IDs) and `count` (1–16, omitted means 1); each gets an isolated session.
Branch stages support ordered `cases` containing `when` expressions and `next`
edges, plus the required default edge. Freeze/Compare use `subject` to identify
the source artifact producer. These fields are parsed strictly like existing
fields. Stage outputs must be permitted by their declared role(s).

Artifact schemas are compiled with JSON Schema (2020-12 by default), supporting
local references and declared older drafts. Invalid schemas are rejected during
workflow validation. Output JSON is canonicalized (including duplicate-key
refusal) then schema-validated. Schema compilation cannot fetch network or file
resources: its loader always refuses external references. The compiled-schema
cache is bounded and performs no I/O.
