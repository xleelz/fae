# Installed adapter schema contract

Every configured `external_tools` alias is an explicit installed capability.
Before live setup enables any executor, Fae performs a bounded read-only
`GET <endpoint>/schemas`. This applies even when a later task elects dry-run:
dry-run performs no effect query or mutation, but startup still validates the
installed capability. There is no configuration-only schema bypass.

The response is `{ "server": "exact-server", "tools": [ { "name": "tool",
"inputSchema": { ... }, "outputSchema": { ... } } ] }`. Tool descriptors use
MCP `tools/list` schema fields; the installed adapter must obtain these from its
actual vendor/installed tool implementation. Fae implements this explicit HTTP
adapter contract, not a native MCP transport or arbitrary vendor authentication.
The server must match exactly; bare and `mcp__server__tool` names resolve only to
the pinned server and tool. Duplicate or ambiguous descriptors are refused.
Responses are limited to 1 MiB and 10 seconds. Redirects, remote plain HTTP,
external schema references, duplicate JSON keys and trailing documents are
refused. Schema documents must declare object properties. Every pin must name
an explicit property even if additional properties are otherwise permitted.
Required fields, types, enums and other JSON Schema constraints are validated.

Configured pins remain strings for compatibility with workflow declarations.
String-typed arguments retain those strings; other types require JSON-encoded
values such as `"true"` for a boolean pin. The mutation intent transmits both
original `arguments` and canonical `typed_arguments`; adapters must use the
latter as their vendor invocation arguments and reject disagreement. The digest
covers typed arguments. `schema_digest` pins the discovered input schema so
recovery refuses a changed schema instead of assuming the old operation remains
valid. Integer conversion preserves JSON number precision.

Ticket room resources come from `GET /rooms/<escaped-room>/resources`:

```json
{
  "server": "installed-server",
  "room": "configured-room",
  "actions": [
    {
      "name": "room-defined-action",
      "tool": "installed-tool",
      "arguments": {"vendor-defined-key": "vendor-defined-value"}
    }
  ]
}
```

Logical lifecycle configuration selects an exact returned action name. Fae does
not invent vendor status values or vendor argument keys. Task-specific bindings
can overlay the returned arguments before validation against the exact tool's
freshly discovered schema. Missing actions and invalid arguments fail closed,
with the room named in the error. Resources cache independently by endpoint,
server and room for one minute by default, with a hard maximum of five minutes
and a 128-entry bound. Expired entries never supply stale actions after a failed
refresh. Cached values are copied before returning them to callers.

At startup, only explicitly configured daemon bindings may defer value checks.
Review policies allow their documented identity tokens and integer `$revision`;
ticket actions allow their configured task/disposition/ticket source keys.
Discovery still compiles the complete schema, requires every dynamic key to be
explicitly declared with its source's type, checks every static pin, and rejects
unknown tokens. Missing required dynamic fields are allowed at installation;
complete required/value/cross-field validation runs after daemon substitution,
before the effect request. Generic aliases receive no placeholder exemption.
