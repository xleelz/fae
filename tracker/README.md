# Linear Tracker Adapter Profile (`tracker.kind: "linear"`)

This package implements the portable Symphony Tracker Adapter contract (§11) and host-executed provider-native coding-agent tools (§10.5) for Linear's GraphQL API.

## 1. Adapter Kind & Configuration (`tracker.provider`)

- **`tracker.kind`**: `"linear"` (case-insensitive)
- **`tracker.active_states` default**: `["Todo", "In Progress"]`
- **`tracker.terminal_states` default**: `["Done", "Canceled", "Cancelled", "Duplicate"]`

| `tracker.provider` Key | Type | Default | Description |
|---|---|---|---|
| `endpoint` | `string` | `"https://api.linear.app/graphql"` | Linear GraphQL endpoint URL (`https://` required; loopback `http://127.0.0.1` / `http://localhost` allowed for local tests). |
| `api_key` | `string` | `"$LINEAR_API_KEY"` | Linear personal API key or `$VAR_NAME` environment variable reference. Missing/empty secret returns `missing_tracker_secret`. |
| `team_id` | `string` | `""` | Optional Linear team UUID scope filter. |
| `team_key` | `string` | `""` | Optional Linear team key scope filter (e.g. `"ENG"`). |
| `project_id` | `string` | `""` | Optional Linear project UUID scope filter. |
| `project_slug` | `string` | `""` | Optional Linear project `slugId` scope filter. |
| `assignee_id` | `string` | `""` | Optional assignee UUID filter applied to `issue.dispatchable`. |
| `state_map` | `map[string]string` | `in_progress -> "In Progress"`, `review`/`human_review -> "In Review"`, `done -> "Done"`, `cancelled -> "Canceled"` | Maps Fae workflow stages/outcomes to Linear workflow state names. |

## 2. Scope Selection & Cursor Pagination

- `FetchIssuesByStates(ctx, stateNames)` and `FetchIssuesByIDs(ctx, issueIDs)` immediately return `[]symphony.Issue{}, nil` without issuing any HTTP request when the input slice is empty (§11.1).
- Queries use GraphQL cursor pagination (`first: 50, after: $after`) and inspect `pageInfo { hasNextPage endCursor }`.
- If `hasNextPage` is `true` while `endCursor` is empty or repeats a previously seen cursor in the same batch, the adapter returns `tracker_pagination`.

## 3. Identity, `native_ref`, and Normalization Rules

- **`id`**: Linear issue UUID (`issue.id`).
- **`identifier`**: Linear human key (`issue.identifier`, e.g. `ENG-123`).
- **`native_ref`**: Non-secret provider metadata map containing `team_id`, `team_key`, `state_id`, `state_type`, `project_id`, and `number` when present (`nil` when empty).
- **`state`**: `issue.state.name` preserved in original casing; state comparisons use `symphony.NormalizeState` (trimmed lowercase).
- **`labels`**: Trimmed, lowercased, blank entries removed, deduplicated (`[]string{}` when empty, never `nil`).
- **`priority`**: Linear integers `1..4` map to `*int` (`1..4`); Linear `0` ("No priority") maps to `nil`.
- **`blocked_by`**: Extracted from `inverseRelations` (`blocks` / `blocked_by`) and `relations` (`blocked_by`), normalized to `[]symphony.BlockerRef` (`[]symphony.BlockerRef{}` when empty, never `nil`).
- **`dispatchable`**: `true` iff every entry in `blocked_by` is in a configured `terminal_states` state AND (when `assignee_id` is configured) `issue.assignee_id` matches `assignee_id`.
- **Malformed record handling (§11.1)**:
  - `FetchIssuesByStates`: omits any node missing required `id`, `identifier`, `title`, or `state.name` and increments `OmittedMalformedCount()` without failing the batch.
  - `FetchIssuesByIDs`: fails the batch with `tracker_response` if any returned node is missing required fields; issues deleted or moved outside the configured scope are omitted.

## 4. Host-Executed Provider-Native Agent Tools (§10.5 & §11.5)

- **Secret Environment Scrubbing**: `SecretEnvironmentNames()` returns `["LINEAR_API_KEY", "LINEAR_TOKEN", "LINEAR_API_TOKEN"]` plus any custom `$VAR_NAME` referenced by `tracker.provider.api_key`.
- **Advertised Tools**:
  1. `linear_get_issue` (`mutates: false`): Fetches normalized issue details, blockers, and recent comments (`issue_id` defaults to `toolCtx.Issue.ID`).
  2. `linear_update_issue_state` (`mutates: true`): Resolves the team's workflow state ID for `state` and transitions the issue (`issue_id` defaults to `toolCtx.Issue.ID`).
  3. `linear_create_comment` (`mutates: true`): Posts a Markdown comment `body` on the issue (`issue_id` defaults to `toolCtx.Issue.ID`).
  4. `linear_add_attachment_link` (`mutates: true`): Attaches a PR/branch/commit `url` and `title` to the issue (`issue_id` defaults to `toolCtx.Issue.ID`).
- **Scope Enforcement**: If `arguments["issue_id"]` targets an issue other than `toolCtx.Issue`, the adapter verifies the target issue belongs to the configured Linear team/project scope before executing; out-of-scope calls fail with `ToolResult{Success: false, Error: "issue outside configured tracker scope"}`.

## 5. Error Category Mapping (§11.4)

| Condition | Portable Category |
|---|---|
| `tracker.kind != "linear"` | `unsupported_tracker_kind` |
| Invalid `endpoint` URL or non-string provider field | `invalid_tracker_config` |
| Missing `LINEAR_API_KEY` or empty `$VAR_NAME` resolution | `missing_tracker_secret` |
| Network/DNS/timeout failure or missing required call parameter | `tracker_request` |
| HTTP 429 or GraphQL `RATELIMITED` error extension | `tracker_rate_limited` |
| Non-2xx HTTP status (other than 429) | `tracker_status` |
| Invalid JSON, GraphQL `errors`, or malformed `FetchIssuesByIDs` node | `tracker_response` |
| `hasNextPage == true` with empty or repeated `endCursor` | `tracker_pagination` |
