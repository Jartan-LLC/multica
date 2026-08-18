# Jartan fork of Multica

This is Jartan LLC's fork of [`multica-ai/multica`](https://github.com/multica-ai/multica).
It exists to carry one patch: a **permission gate on agent-definition writes**.
Everything else tracks upstream.

Read this before rebasing, deploying, or extending the patch.

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

## What the patch does

1. **Gate.** Before any agent-record mutation, resolve the actor:
   - human member → unchanged, existing `canManageAgent` / role checks decide;
   - agent principal **not** on the workspace allow-list → `403`, nothing written;
   - agent principal **on** the allow-list → permitted, within the field scope below.
2. **Allow-list.** A per-workspace list of agent ids (`agent_definition_writer`),
   writable **only by a human workspace owner**. It exists so People Ops keeps
   its standing authority to hire, tune and retire agents. An **empty list is the
   shipped default and fails closed**: no agent may mutate any definition.
3. **Field scope for a listed agent.** `permission_mode`, `invocation_targets`
   and `visibility` stay owner-only for *every* agent — a listed seat may tune
   behaviour, never re-trust or re-target a seat. `custom_env` stays denied to
   every agent, on create as well as on its own endpoint.
4. **Create and copy are closed the same way.** `agent copy` has no server
   endpoint — the CLI implements it as `GET` + `POST /api/agents` — so gating
   create closes copy. Without this, an agent barred from rewriting a seat could
   mint one it owns outright.
5. **Audit.** Every definition write by a listed agent writes an
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

## The residual, stated plainly

The allow-list weakens the control by exactly one seat. An allow-listed agent is
still an agent, reachable by the same injection paths. The gate is **"one seat
instead of thirty", not "no seats"**. It is bounded by: keeping the list
minimal, the field scope above, the list being owner-only so no listed seat can
widen it, and the audit rows.

Two more limits worth naming, because neither is closed by this patch:

- **The gate rests on the caller using its own agent credential.** It classifies
  a caller as an agent from the server-verified `mat_` task token. A process
  that gets hold of a *human's* credential — a `mul_` PAT, a session, or the
  `JWT_SECRET` itself — authenticates as that human and the gate does not apply
  to it, by design. So any exposure of human credentials on the host is a path
  around this control, not merely a separate issue.
- **`POST /api/agents/mika` is deliberately left ungated.** It is a server-owned,
  idempotent bootstrap of the workspace's single built-in Chief-of-Staff seat,
  keyed on `system_key`, and it is not a general create path.

## Where the patch lives

| Path | What it is |
|---|---|
| `server/migrations/900_agent_definition_writer.{up,down}.sql` | the allow-list table |
| `server/pkg/db/queries/agent_definition_writer.sql` | its queries (+ generated code) |
| `server/internal/handler/agent_definition_gate.go` | the gate, the actor resolution, the audit helper |
| `server/internal/handler/agent_definition_writers_api.go` | owner-only allow-list API |
| `server/internal/handler/agent.go` | gate call in `canManageAgent`; create + permission-field rules |
| `server/internal/handler/skill.go`, `agent_runtime_skills.go` | audit at the shared skill-write exits |
| `server/cmd/server/router.go` | the two additive routes |
| `server/pkg/db/queries/workspace_delete.sql` | allow-list rows go with the workspace |
| `server/internal/handler/agent_definition_gate_test.go` | the tests, written against AC1–AC5 |

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
cd server && go build ./... && go test ./internal/handler/ -run AgentDefinition
```

Check after every rebase:

- `canManageAgent` still calls `authorizeAgentDefinitionWrite`, and still first.
- New `/api/agents` mutation routes go through `canManageAgent`; if one does
  not, gate it explicitly.
- `CreateAgent` still carries the gate and the permission/`custom_env` rules.
- The `agent_definition_writer` row in `workspaceDeletionManifest` still matches
  the schema (upstream's own drift test will fail loudly if it does not).
- Any new writable field on `UpdateAgent` is behaviour config, not a trust axis;
  a trust axis belongs with `permission_mode` on the owner-only side.

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
