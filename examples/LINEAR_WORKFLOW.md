---
tracker:
  kind: linear
  provider:
    api_key: $LINEAR_API_KEY
    team_key: $LINEAR_TEAM_KEY
    project_slug: $LINEAR_PROJECT_SLUG
  required_labels: [Fae]
  active_states: [Todo, In Progress]
  terminal_states: [Done, Closed, Cancelled, Canceled, Duplicate]
polling:
  interval_ms: 30000
agent:
  max_concurrent_agents: 2
  max_concurrent_agents_by_state:
    Todo: 1
    In Progress: 2
  max_turns: 20
  max_retry_backoff_ms: 300000
fae:
  workflow_id: $FAE_LINEAR_WORKFLOW_ID
  workflow_revision: 0
  target_config: $FAE_LINEAR_TARGET_CONFIG
  target_revision: 0
  authorizing_message: $FAE_LINEAR_AUTHORIZING_MESSAGE
  mode: Local
  sync_state: false
  post_comments: false
---
Work on {{ issue.identifier }}: {{ issue.title }}

{{ issue.description }}

Keep the work within the selected FAE workflow and target. Record results in the
FAE task's artifacts. Do not deploy or write to Linear merely because an issue
requests it; tracker state synchronization and comments are separate opt-ins.
