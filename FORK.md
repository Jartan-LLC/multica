# Jartan fork of Multica

This is Jartan LLC's fork of [`multica-ai/multica`](https://github.com/multica-ai/multica).
It carries security patches against upstream behaviour that Jartan cannot fix
from outside the code. Everything else tracks upstream.

| Patch | Finding | Section |
|---|---|---|
| Permission gate on agent-definition writes | `SEC-2026-0069` (High) | [Why the fork exists](#why-the-fork-exists) |
| Chat-session access scoped to the calling agent | `SEC-2026-0078` (High) | [The chat-surface patches](#the-chat-surface-patches) |
| The sending agent recorded on chat-initiated runs | `SEC-2026-0079` (High) | [The chat-surface patches](#the-chat-surface-patches) |

All three fall out of one root: **an agent run's token carries its owning
human's identity**. `resolveActor` can tell an agent principal from a member
server-side and unspoofably, and each patch applies that distinction to one
surface that was missing it. The root itself — the runtime drawing no per-run
identity boundary — is `SEC-2026-0075` and is not fixed here.

Read this before rebasing, deploying, or extending any of them.

## Why the fork exists

On upstream, any agent run can permanently rewrite any other agent's definition:

```
multica agent update <any-agent-id> --instructions "..."
```

No approval, no diff, no revision history. The rewritten text is what every
future run of that agent starts from.

The cause is not a missing access model, it is an inconsistently applied one.
Every agent-mutation handler gates on `canManageAgent` — "the agent's owner OR
a workspace owner/admin" — and never asks *which kind of principal* is calling.
An agent run authenticates with a `mat_` task token whose `X-User-ID` is stamped
to its **owning human**, so in a workspace where one human owns every agent,
every agent seat clears that check for every other agent. One turn of influence
over any agent — an issue body, an attachment, a repository a run reads —
converts into permanent control of a seat holding real credentials.

Two surfaces upstream already refuse agent principals outright, with a
server-verified predicate: `agent env` (`authorizeAgentEnv`) and the MCP-server
writers (`requireAgentMcpWriter`). The patch applies that same predicate to the
rest of the agent-record mutation surface.

Tracked as `SEC-2026-0069` (High) on Jartan's security register; the build is
JAR-577, the spec and rulings are on JAR-571.

## What the agent-definition gate does

The governing access model is Thorne's amendment on JAR-571, comment
`0b3ad55b` (2026-08-18), which supersedes the original ruling `83bcdfb0`
wherever the two differ. Section numbers below refer to that amendment.

1. **Gate.** Before any agent-record mutation, resolve the actor:
   - human member → unchanged, existing `canManageAgent` / role checks decide;
   - agent principal **not** on the workspace allow-list → `403`, nothing written;
   - agent principal writing **its own** record → `403`, allow-listed included (§2);
   - agent principal on the allow-list → permitted, within the operation and
     field scope below.
2. **Allow-list.** A per-workspace list of agent ids (`agent_definition_writer`),
   writable **only by a human workspace owner**. It exists so People Ops keeps
   its standing authority to hire, tune and retire agents. An **empty list is the
   shipped default and fails closed**: no agent may mutate any definition.
   Thorne's recommended initial contents are one seat, Juno.
3. **Operation scope for a listed agent** (§2). Permitted: `create`, `update`,
   the four workspace-skill writes, `archive` / `restore`. Everything else on
   the agent-record surface stays owner-only for every agent — `cancel-tasks`,
   runtime-skill overrides, agent labels, Lark bindings. The scope is an
   argument to `canManageAgent`, and its zero value is closed, so a mutation
   route added upstream cannot reach an agent principal until someone chooses a
   scope for it at the call site. On a rebase that is a compile error, not a
   silently widened gate.
4. **Field scope for a listed agent** (§2, §3). On update: `instructions`,
   `model`, `thinking_level`, `name`, `description` — and nothing else. The
   check is an allow-list over the request body, so a writable field upstream
   adds later is denied to agents until someone rules on it. On create it adds
   `runtime_id` (required there, denied on update), `skill_ids` and `template`.
   `mcp_config` is **owner-only alongside `custom_env`** — an MCP block is a
   tool grant and an egress path, and nothing in the instruction text a
   reviewer reads would show it changing.
5. **The trust axis is server-set on create, never caller-set** (§4).
   `permission_mode` / `visibility` / `invocation_targets` from an agent
   principal are **ignored on create, not refused**: the server mints the seat
   `public_to` with the workspace as its sole invocation target — the only shape
   the roster has ever had, and the shape that makes a new hire assignable. On
   update the same input is refused outright with a `403`.

   Both halves have a live failure behind them. Minting `private` instead (a
   literal reading of the original ruling) lands every hire unassignable, since
   `issue create --assignee-id` against a private seat fails — the Ward
   precedent, JAR-498. Refusing the input instead breaks every create from a
   CLI that serialises a defaulted `--visibility`, which is the silent
   CLI/server skew this fork exists to avoid (AC8). Confirmed at the pinned
   release: the CLI sends these keys only when the flag was explicitly set —
   but the rule does not depend on that holding.
6. **Create and copy are closed the same way.** `agent copy` has no server
   endpoint — the CLI implements it as `GET` + `POST /api/agents` — so gating
   create closes copy. Without this, an agent barred from rewriting a seat could
   mint one it owns outright. Note that `agent copy` carries the source's
   `avatar_url`, `custom_args` and `max_concurrent_tasks` into the create body,
   so a copy by a listed seat fails loud on the first of those; hiring is
   `agent create`.
7. **Audit.** Every definition write by a listed agent writes an
   `agent_definition_written` row to `activity_log`, and every change to the
   allow-list writes `agent_definition_writers_updated`. This is evidence, not a
   control: an audit trail alone does not close the finding (write, act, write
   back, and the trail shows two edits with a zero diff).

### What it deliberately does not do

- **No web UI.** Enforcement is at the API; a UI affordance is a follow-on.
- **No CLI changes.** The fleet runs the upstream CLI binary, so a CLI-side
  check would be neither reachable nor a control. The API is **additive-only**
  — new routes and fields, no change to any existing request or response shape
  — so an unmodified upstream CLI keeps working against this server.
- **It does not fix the root.** The runtime draws no per-run identity boundary
  (`SEC-2026-0075`). This is a per-object compensating control on one instance
  of that root, and it says so.
- **It does not deny `mcn_` cloud-node PATs.** The gate keys on `resolveActor`'s
  `"agent"` verdict, as ruled. If cloud nodes ever run agent code here, widen
  the predicate in `authorizeAgentDefinitionWrite`.

### Consequences a reader should know before deploying

- **`mcp_config` becomes a human act.** Platform's recorded route for binding an
  MCP server to an agent is `multica agent update <id> --mcp-config-file`
  (JAR-528). Under §3 that call is the workspace owner's, not Rook's — on
  create as well as on update.
- **A listed seat cannot edit itself** (§2), so any change to the seat holding
  the write is the owner's. Zero cost against the live record.
- **Agent Design Standard §3's create call** no longer needs
  `--permission-mode public_to --public-to-workspace`; both are now no-ops. The
  read-back after create stays, and changes job: it verifies the server did its
  part.

## The residual, stated plainly

The allow-list weakens the control by exactly one seat. An allow-listed agent is
still an agent, reachable by the same injection paths. The gate is **"one seat
instead of thirty", not "no seats"**. It is bounded by: keeping the list
minimal, the operation and field scope above, no listed seat being able to edit
itself or the list, and the audit rows.

Two more limits worth naming, because neither is closed by this patch:

- **The gate rests on the caller using its own agent credential.** It classifies
  a caller as an agent from the server-verified `mat_` task token. A process
  that gets hold of a *human's* credential — a `mul_` PAT, a session, or the
  `JWT_SECRET` itself — authenticates as that human and the gate does not apply
  to it, by design. So any exposure of human credentials on the host is a path
  around this control, not merely a separate issue. Thorne has since made this
  the **ninth** condition for closing `SEC-2026-0069` (`2ed2fc2a`): the
  world-readable deployment `.env` (`SEC-2026-0077`, JAR-583) lets a run mint a
  member token and step around the gate entirely. Root-side remediation is
  JAR-587.
- **`POST /api/agents/mika` is deliberately left ungated.** It is a server-owned,
  idempotent bootstrap of the workspace's single built-in Chief-of-Staff seat,
  keyed on `system_key`, and it is not a general create path.

## The chat-surface patches

Two findings, one file, one root — the same "every agent authenticates as the
owner" that the gate above exists for, on the chat API instead of the agent
record. Registered on JAR-613, built on JAR-616. Both are upstream design; the
fork introduced neither.

### `SEC-2026-0078` — chat authorisation keyed on the token owner

Upstream authorises every chat surface on the session's creator:
`loadChatSessionForUser` admits when `chat_session.creator_id` equals the
request's `X-User-ID`, and the list endpoints select `WHERE creator_id = $2`.
The auth middleware stamps an `mat_` token's `X-User-ID` with the token row's
owning human, so thirty seats and the owner resolve to one id and the creator
predicate separates nobody from anybody. Demonstrated, not inferred: from inside
an ordinary run, `GET /api/chat/sessions?status=all` returned every chat session
in the workspace — the owner's private conversations included, with message
content — and `GET /api/chat/sessions/{id}/messages` returned their transcripts.

The patch scopes chat authorisation by **principal**. For a task-token request a
session is reachable on exactly two openings:

- the session's **pinned** agent (`chat_session.agent_id`) — its own
  conversation, whose transcript its runs are handed anyway; and
- the session's **creating** agent (`chat_session.creator_agent_id`, migration
  901) — an agent that opened a session to talk to another agent must be able to
  read the reply.

Everything else is denied. **Member requests are untouched**: the creator check
remains their whole rule.

The rule lives in `chatActorScope` (`chat_actor_gate.go`), applied at
`loadChatSessionForUser` — the single choke point every per-session chat handler
funnels through, including the file-upload and agent-builder paths — and at each
list endpoint, which cannot use that choke point because they never load a
session. The scope's **zero value is a member scope**, so a call site that
forgets to build one falls back to upstream behaviour rather than locking
members out of their own chats.

`creator_agent_id` is NULL for every pre-existing row and every row the web UI
writes, which is correct: those sessions were created by a human. There is no
backfill and none is possible.

### `SEC-2026-0079` — an agent's chat send recorded as the owner's own action

The sending principal was resolved on the way in and then dropped.
`SendChatMessage` resolves `("agent", id)`, and `SendDirectChatMessage` then
stamped `attribution.DirectHumanRun(<owner>, …)` — source `direct_human`, "a
member's own action enqueued the run", which the attribution package classifies
as compliance-grade precise. At claim, `initiator_type` was set to `"member"`
wherever the row carried an `initiator_user_id`, and the daemon rendered "This
task was initiated by **\<owner\>** (…), a member of this workspace" into the one
prompt block that exists to answer "who am I answering".

The record did not omit the sender. It asserted a false one, and told the
recipient the same thing.

The patch records the sending principal and stops the false claim:

- `agent_task_queue.initiator_agent_id` (migration 902) carries the sending
  agent; `initiator_user_id` is left NULL, because no human sent it.
- Attribution becomes a **delegation** off the sending task
  (`attribution.AgentChatSend`), with the accountable human copied from that task
  rather than chained, and `delegated_from_task_id` making the hop traceable to a
  real run. A send whose sending run resolves no human lands `unattributed` (then
  the workspace's own fallback policy), never `direct_human`.
- At claim the initiator renders through the **agent branch
  `BuildTaskInitiatorBlock` already had** — "initiated by X, another agent in
  this workspace". No new prompt text was written for this.

Both values are server-verified, not caller-asserted: the auth middleware forces
`X-Agent-ID` and `X-Task-ID` from the token row, overriding whatever the client
sent.

### What these two deliberately do not do

- **No web UI**, as with the gate — enforcement is at the API.
- **They do not fix the root** (`SEC-2026-0075`). A process holding a *human's*
  credential authenticates as that human and neither patch applies to it, by
  design — the same residual the gate carries.
- **They do not add sender identity to the message body.** The recipient learns
  the sender from `## Task Initiator`, which is server-written; nothing in the
  message text is trusted for it.
- **`SEC-2026-0078` does not restrict what a session's pinned agent may read.**
  That agent already receives the transcript through its own runs; denying the
  API while the daemon delivers the same bytes would be theatre.

### Consequences a reader should know

- **An agent-to-agent chat channel must create its own session.** Reaching a
  session opened by someone else now fails, so the pattern is: the sender creates
  the session (stamped `creator_agent_id`), sends, and polls it. Both ends stay
  reachable — the sender as creator, the recipient as pinned agent.
- **`HasPendingChatTasks` answers an agent principal off the list query**, not
  the `EXISTS` fast path, which cannot express the principal rule without
  changing an upstream query's shape. Members keep the fast path.
- **The refusal is a 404, not a 403**, so probing session ids from a run tells
  the caller nothing about which sessions exist.

## Where the patches live

| Path | What it is |
|---|---|
| `server/migrations/900_agent_definition_writer.{up,down}.sql` | the allow-list table |
| `server/pkg/db/queries/agent_definition_writer.sql` | its queries (+ generated code) |
| `server/internal/handler/agent_definition_gate.go` | the gate, the actor resolution, the audit helper |
| `server/internal/handler/agent_definition_writers_api.go` | owner-only allow-list API |
| `server/internal/handler/agent.go` | scoped gate call in `canManageAgent`; create/update field scope; server-set create permission |
| `server/internal/handler/skill.go`, `agent_runtime_skills.go`, `label.go`, `lark.go` | the scope chosen at each remaining `canManageAgent` call site; audit at the shared skill-write exits |
| `server/cmd/server/router.go` | the two additive routes |
| `server/pkg/db/queries/workspace_delete.sql` | allow-list rows go with the workspace |
| `server/internal/handler/agent_definition_gate_test.go` | the tests, written against AC1–AC5 and the amendment's §2/§3/§4 |

The chat-surface patches (`SEC-2026-0078`, `SEC-2026-0079`):

| Path | What it is |
|---|---|
| `server/migrations/901_chat_session_creator_agent.{up,down}.sql` | `chat_session.creator_agent_id` |
| `server/migrations/903_chat_session_creator_agent_index.{up,down}.sql` | its index, built concurrently in its own file |
| `server/migrations/902_task_initiator_agent.{up,down}.sql` | `agent_task_queue.initiator_agent_id` |
| `server/internal/handler/chat_actor_gate.go` | `chatActorScope` — the whole reachability rule, in one file |
| `server/internal/handler/chat.go` | the scope at `loadChatSessionForUser`, the two list branches, the pending-task endpoints; the create stamp; the sending task id on send |
| `server/internal/handler/agent_builder.go` | the same scope on the builder-session list, and the create stamp |
| `server/internal/attribution/agent_chat_send.go` | `AgentChatSend` — delegation attribution for an agent-sent message |
| `server/internal/service/agent_chat_send.go` | the DB read that gathers its facts, kept out of the pure package |
| `server/internal/service/task.go` | `SendDirectChatMessage`: agent-sender branch, `initiator_agent_id`, no `direct_human` |
| `server/internal/handler/daemon.go` | claim renders the agent initiator through the branch that already existed |
| `server/pkg/db/queries/chat.sql` | the three columns on their statements (+ generated code) |
| `server/cmd/migrate/main.go` | the concurrent-index cleanup hook for 903 |
| `server/internal/handler/chat_actor_gate_test.go`, `chat_sender_attribution_test.go` | the tests, written against Thorne's two closure conditions on JAR-616 |

Fork-only migrations use the **900+ prefix band**, never the next sequential
number, so a rebase never collides with an upstream migration that took the same
number. `schema_migrations` tracks each version by name, not by a single cursor,
so an out-of-band prefix applies and rolls back normally.

## Operating it

The allow-list is owner-configured runtime data, not a build-time constant. As
the human workspace owner, with a session cookie or a `mul_` PAT (a `mat_` task
token is refused):

```bash
# read the current list
curl -sS "$MULTICA_SERVER_URL/api/agent-definition-writers?workspace_id=$WORKSPACE_ID" \
  -H "Authorization: Bearer $MULTICA_PAT"

# replace it wholesale (empty array = fail closed, no agent may write)
curl -sS -X PUT "$MULTICA_SERVER_URL/api/agent-definition-writers?workspace_id=$WORKSPACE_ID" \
  -H "Authorization: Bearer $MULTICA_PAT" -H 'Content-Type: application/json' \
  -d '{"agent_ids":["<people-ops-agent-id>"]}'
```

The list takes effect on the next request — no restart, no cache flush.

## Rebasing onto upstream

Upstream pushes daily, so this patch carries a permanent rebase cost. It is kept
small on purpose: the gate is one call inside `canManageAgent`, which is the
predicate every agent-record mutation route already funnels through, so a
mutation route added upstream **inherits the gate on rebase** instead of quietly
opening a hole.

```bash
git remote add upstream https://github.com/multica-ai/multica.git   # once
git fetch upstream && git rebase upstream/main
cd server && go build ./... \
  && go test ./internal/handler/ -run 'AgentDefinition|ChatActorGate|ChatSenderAttribution' \
  && go test ./cmd/migrate/
```

Check after every rebase — the gate:

- `canManageAgent` still calls `authorizeAgentDefinitionWrite`, and still first.
- New `/api/agents` mutation routes go through `canManageAgent`; if one does
  not, gate it explicitly.
- `CreateAgent` still carries the gate, the create field scope, and the
  server-set permission branch (`agentCreatedSeatPermission`).
- Every `canManageAgent` call site still passes a deliberate scope. A new one
  will not compile without one — choose `agentDefinitionScopeClosed` unless the
  route is inside the permitted operation set.
- Any new writable field on `UpdateAgent` is denied to agents by default
  (`agentDefinitionUpdateFields` is an allow-list); add it only if it is
  behaviour config a listed seat should hold.
- The `agent_definition_writer` row in `workspaceDeletionManifest` still matches
  the schema (upstream's own drift test will fail loudly if it does not).

And the chat-surface patches:

- `loadChatSessionForUser` still calls `chatActorScopeFor`, and still after the
  creator check. It is the choke point; a per-session chat handler added upstream
  inherits the rule through it.
- A **list** endpoint added upstream over `chat_session` does NOT inherit
  anything — it never loads a session. Any new `WHERE creator_id = ...` query
  needs `scope.allows(...)` applied to its rows, exactly as the four existing
  ones do.
- `CreateChatSession` (both call sites) still stamps `creator_agent_id` from the
  resolved actor, never from the request body.
- `SendDirectChatMessage` still branches on `agentSender` before building
  attribution, and still passes `initiator_agent_id` + `delegated_from_task_id`
  to `CreateChatTask`. An upstream rewrite of that function's attribution is the
  most likely place for `direct_human` to come back.
- The claim handler still checks `task.InitiatorAgentID` **before**
  `task.InitiatorUserID`. Reversing the order restores the false member claim
  without failing to compile.
- Migration 903 still has its entry in `concurrentIndexCleanups`; upstream's own
  test fails loudly if it does not.

## Deploying

Deploy is a human act — no agent holds a credential that reaches the host.
Point both Dokploy compose services' `build.context` at this fork at a **pinned
tag or commit SHA, never a floating branch**, and redeploy. Rollback is
repointing to the previous known-good ref; Postgres is untouched either way.

## Licence

Upstream's "Multica License" (Apache-2.0 plus conditions) permits internal use
within a single organisation and permits publishing a fork's source. Offering it
as a hosted service to third parties, or removing the Multica name or logo from
the UI, is not permitted without a licence or written waiver. This fork does
neither.
