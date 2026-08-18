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

## Where the patch lives

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
