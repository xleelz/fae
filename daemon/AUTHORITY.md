# RPC authority

The owner credential authenticates the operator. Request fields never select the actor.
Start the daemon with `serve --principal-config /absolute/path/principals.json`
to configure delegated credentials. The configuration is an owner-only regular
JSON file (mode 0600), containing for example:

```json
[{"credential_file":"/absolute/path/controller.credential","actor":"controller"},
 {"credential_file":"/absolute/path/task-prophet.credential","actor":"prophet","task":"task-id"}]
```

Use a private directory outside the repository. Missing credential files are
created with 32 random bytes and mode 0600; existing files are reused across
restarts. Supported actors are controller, prophet, workflow-role, and qa.
Delegates send their credential through the existing authenticated Unix RPC API.
The operator CLI continues to use the owner credential. Removing a binding and
restarting revokes that delegated credential. Credentials never belong in messages.

A free-text operator message authorizes an operator action and is consumed for its
first successful action class. Further actions in that class may reuse it until
its age limit expires. An incompatible class requires another message, or explicit
multi-class directions. To direct a controller, record message text such as:

```json
{"text":"Install this workflow and admit tasks","authorize":{"verbs":["WorkflowInstall","Admit"]}}
```

Alternatively explicitly grant classes (`task`, `governance`, `runtime`,
`controller`, `schedule`) using `authorize.classes`. Controller subactions use
exact names such as `Controller.rearm` and `Schedule.create`. A Prophet requires
an exact verb direction; only `RestartWithInformation` and `ResumeWithContext`
are enacted. It cannot change governance or quality dispositions. Per-task
credentials are confined to their task history and those two controls; they
cannot open the instance event stream. Workflow roles and QA cannot mutate RPC
control state. Artifact production and quality artifacts retain their separate
stage capabilities; this table does not create a new artifact-write endpoint.

Consumption and the mutation's audit row commit atomically. Internal writes
reached from that RPC inherit the citation. Rollback preserves the message's
previous consumption. Explicit lifecycle/watchdog continuations retain their
existing durable-grant age exemption; new actions still require a fresh message.
