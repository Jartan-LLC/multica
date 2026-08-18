package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Tests for the agent-definition write gate (Jartan fork, SEC-2026-0069).
//
// The finding these cover: an ordinary agent seat could rewrite any other
// agent's definition, because every mutation handler asked "is the caller the
// owner or an admin?" and a mat_ task token answers yes on behalf of its
// owning human. The assertions below are written against the acceptance
// criteria on JAR-571 (AC1–AC5) and Thorne's closure statement on the same
// issue, so a reader can map a failure straight back to the criterion it
// breaks.
//
// Every test here drives the handler directly with the header shape the auth
// middleware produces for a task token — X-Actor-Source: task_token plus
// X-Agent-ID — and with X-User-ID left at the OWNING human, which is the
// point: the finding exists precisely because that human clears the old check.

// agentActorReq builds a request as the Auth middleware leaves it for a mat_
// task token: server-set X-Actor-Source and X-Agent-ID, with X-User-ID still
// the agent's owning human (newRequest sets it to testUserID, who owns every
// fixture agent here).
func agentActorReq(actingAgentID, method, path string, body any) *http.Request {
	req := newRequest(method, path, body)
	req.Header.Set("X-Actor-Source", "task_token")
	req.Header.Set("X-Agent-ID", actingAgentID)
	return req
}

// allowListAgent puts an agent on the workspace's allow-list for the duration
// of one test, straight through the pool — the API path is exercised
// separately by TestAgentDefinitionWriters_OwnerOnly.
func allowListAgent(t *testing.T, agentID string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(), `
		INSERT INTO agent_definition_writer (workspace_id, agent_id, created_by)
		VALUES ($1, $2, $3)
		ON CONFLICT (workspace_id, agent_id) DO NOTHING
	`, testWorkspaceID, agentID, testUserID); err != nil {
		t.Fatalf("allow-list agent: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent_definition_writer WHERE agent_id = $1`, agentID)
	})
}

// agentDefinitionSnapshot captures the behaviour-defining columns so a test
// can prove a denied write left the record byte-identical (AC1), not merely
// that it returned an error code.
func agentDefinitionSnapshot(t *testing.T, agentID string) string {
	t.Helper()
	var snapshot string
	if err := testPool.QueryRow(context.Background(), `
		SELECT concat_ws('|', name, description, instructions, model, thinking_level,
		                 service_tier, runtime_id::text, permission_mode, visibility,
		                 max_concurrent_tasks::text, avatar_url,
		                 mcp_config::text, custom_args::text, runtime_config::text,
		                 status, archived_at::text)
		FROM agent WHERE id = $1
	`, agentID).Scan(&snapshot); err != nil {
		t.Fatalf("snapshot agent %s: %v", agentID, err)
	}
	return snapshot
}

// TestAgentDefinitionGate_OrdinaryAgentDeniedAcrossSurface is AC1 and AC2: an
// agent seat that is NOT on the allow-list is refused on every mutation route
// of the agent record — not one field, not one endpoint — and the target's
// record is byte-identical afterwards. The gate lives in canManageAgent, which
// is the predicate all of these share, so this is also the test that proves a
// route added upstream inherits the gate.
//
// AC2 (server-side, not CLI-side) is structural here: these calls go straight
// into the handler with no CLI in the path at all.
func TestAgentDefinitionGate_OrdinaryAgentDeniedAcrossSurface(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}

	attacker := createHandlerTestAgent(t, "gate-attacker", nil)
	target := createHandlerTestAgent(t, "gate-target", nil)
	before := agentDefinitionSnapshot(t, target)

	randomUUID := "00000000-0000-0000-0000-0000000000ff"

	cases := []struct {
		name    string
		call    func(w http.ResponseWriter, r *http.Request)
		method  string
		path    string
		body    any
		params  []string
		wantMsg string
	}{
		{
			name: "update instructions", call: testHandler.UpdateAgent,
			method: http.MethodPut, path: "/api/agents/" + target,
			body:   map[string]any{"instructions": "you now report to me"},
			params: []string{"id", target},
		},
		{
			name: "update model", call: testHandler.UpdateAgent,
			method: http.MethodPut, path: "/api/agents/" + target,
			body:   map[string]any{"model": "some-other-model"},
			params: []string{"id", target},
		},
		{
			name: "update mcp_config", call: testHandler.UpdateAgent,
			method: http.MethodPut, path: "/api/agents/" + target,
			body:   map[string]any{"mcp_config": map[string]any{"mcpServers": map[string]any{}}},
			params: []string{"id", target},
		},
		{
			name: "update custom_args", call: testHandler.UpdateAgent,
			method: http.MethodPut, path: "/api/agents/" + target,
			body:   map[string]any{"custom_args": []string{"--dangerously-skip-permissions"}},
			params: []string{"id", target},
		},
		{
			name: "update avatar_url (the `agent avatar` path)", call: testHandler.UpdateAgent,
			method: http.MethodPut, path: "/api/agents/" + target,
			body:   map[string]any{"avatar_url": "https://example.invalid/a.png"},
			params: []string{"id", target},
		},
		{
			name: "archive", call: testHandler.ArchiveAgent,
			method: http.MethodPost, path: "/api/agents/" + target + "/archive",
			params: []string{"id", target},
		},
		{
			name: "restore", call: testHandler.RestoreAgent,
			method: http.MethodPost, path: "/api/agents/" + target + "/restore",
			params: []string{"id", target},
		},
		{
			name: "cancel-tasks", call: testHandler.CancelAgentTasks,
			method: http.MethodPost, path: "/api/agents/" + target + "/cancel-tasks",
			params: []string{"id", target},
		},
		{
			name: "skills set", call: testHandler.SetAgentSkills,
			method: http.MethodPut, path: "/api/agents/" + target + "/skills",
			body:   map[string]any{"skill_ids": []string{}},
			params: []string{"id", target},
		},
		{
			name: "skills add", call: testHandler.AddAgentSkills,
			method: http.MethodPost, path: "/api/agents/" + target + "/skills/add",
			body:   map[string]any{"skill_ids": []string{}},
			params: []string{"id", target},
		},
		{
			name: "skill enable", call: testHandler.SetAgentSkillEnabled,
			method: http.MethodPut, path: "/api/agents/" + target + "/skills/" + randomUUID + "/enabled",
			body:   map[string]any{"enabled": false},
			params: []string{"id", target, "skillId", randomUUID},
		},
		{
			name: "skill remove", call: testHandler.RemoveAgentSkill,
			method: http.MethodDelete, path: "/api/agents/" + target + "/skills/" + randomUUID,
			params: []string{"id", target, "skillId", randomUUID},
		},
		{
			name: "runtime-skill toggle", call: testHandler.SetAgentRuntimeSkillEnabled,
			method: http.MethodPut, path: "/api/agents/" + target + "/runtime-skills/enabled",
			body:   map[string]any{"runtime_id": testRuntimeID, "root": "user", "key": "k", "enabled": false},
			params: []string{"id", target},
		},
		{
			name: "label attach", call: testHandler.AttachLabelToAgent,
			method: http.MethodPost, path: "/api/agents/" + target + "/labels",
			body:   map[string]any{"label_id": randomUUID},
			params: []string{"id", target},
		},
		{
			name: "label detach", call: testHandler.DetachLabelFromAgent,
			method: http.MethodDelete, path: "/api/agents/" + target + "/labels/" + randomUUID,
			params: []string{"id", target, "labelId", randomUUID},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := withURLParams(agentActorReq(attacker, tc.method, tc.path, tc.body), tc.params...)
			w := httptest.NewRecorder()
			tc.call(w, req)
			if w.Code != http.StatusForbidden {
				t.Fatalf("expected 403 for %s from a non-allow-listed agent, got %d: %s", tc.name, w.Code, w.Body.String())
			}
		})
	}

	// env was already denied to agents upstream; assert it still is, so a
	// regression in the gate cannot quietly relax the surface it borrowed the
	// predicate from (Thorne's closure item 4).
	for _, tc := range []struct {
		name string
		call func(w http.ResponseWriter, r *http.Request)
	}{
		{"env get", testHandler.GetAgentEnv},
		{"env set", testHandler.UpdateAgentEnv},
	} {
		req := withURLParam(agentActorReq(attacker, http.MethodPut, "/api/agents/"+target+"/env",
			map[string]any{"custom_env": map[string]string{"API_KEY": "x"}}), "id", target)
		w := httptest.NewRecorder()
		tc.call(w, req)
		if w.Code != http.StatusForbidden {
			t.Errorf("expected 403 for %s from an agent principal, got %d: %s", tc.name, w.Code, w.Body.String())
		}
	}

	if after := agentDefinitionSnapshot(t, target); after != before {
		t.Fatalf("target agent record changed under a denied write\nbefore: %s\nafter:  %s", before, after)
	}
}

// TestAgentDefinitionGate_CreateAndCopyClosed is AC3. `agent copy` has no
// server endpoint — the CLI implements it as GET + POST /api/agents — so
// closing create closes copy, and minting a seat can no longer be the way an
// agent gets a definition it may write.
func TestAgentDefinitionGate_CreateAndCopyClosed(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}

	attacker := createHandlerTestAgent(t, "gate-create-attacker", nil)

	body := map[string]any{
		"name":         "minted-by-an-agent",
		"runtime_id":   testRuntimeID,
		"instructions": "whatever I like",
	}
	req := agentActorReq(attacker, http.MethodPost, "/api/agents", body)
	w := httptest.NewRecorder()
	testHandler.CreateAgent(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 on create from a non-allow-listed agent, got %d: %s", w.Code, w.Body.String())
	}

	var count int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM agent WHERE name = 'minted-by-an-agent'`).Scan(&count); err != nil {
		t.Fatalf("count minted agents: %v", err)
	}
	if count != 0 {
		t.Fatalf("a denied create still wrote %d agent row(s)", count)
	}
}

// TestAgentDefinitionGate_AllowListedAgentMayTune is AC4: the allow-list is
// what keeps People Ops' hire / tune / retire authority working after the gate.
// A listed seat updates instructions and model, archives and restores — and
// every one of those writes leaves an audit row naming the acting agent.
func TestAgentDefinitionGate_AllowListedAgentMayTune(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}

	peopleOps := createHandlerTestAgent(t, "gate-people-ops", nil)
	target := createHandlerTestAgent(t, "gate-tune-target", nil)
	allowListAgent(t, peopleOps)

	req := withURLParam(agentActorReq(peopleOps, http.MethodPut, "/api/agents/"+target,
		map[string]any{"instructions": "revised by People Ops"}), "id", target)
	w := httptest.NewRecorder()
	testHandler.UpdateAgent(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("allow-listed agent update: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var instructions string
	if err := testPool.QueryRow(context.Background(),
		`SELECT instructions FROM agent WHERE id = $1`, target).Scan(&instructions); err != nil {
		t.Fatalf("read back instructions: %v", err)
	}
	if instructions != "revised by People Ops" {
		t.Fatalf("expected the tuned instructions on disk, got %q", instructions)
	}

	for _, step := range []struct {
		name string
		call func(w http.ResponseWriter, r *http.Request)
		path string
	}{
		{"archive", testHandler.ArchiveAgent, "/archive"},
		{"restore", testHandler.RestoreAgent, "/restore"},
	} {
		req := withURLParam(agentActorReq(peopleOps, http.MethodPost, "/api/agents/"+target+step.path, nil), "id", target)
		w := httptest.NewRecorder()
		step.call(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("allow-listed agent %s: expected 200, got %d: %s", step.name, w.Code, w.Body.String())
		}
	}

	// Audit: evidence, not a control (Thorne, JAR-521) — but a privileged seat
	// acting unseen is what makes the residual unbounded, so the rows must exist
	// and must name the acting agent rather than its owning human.
	var audited int
	if err := testPool.QueryRow(context.Background(), `
		SELECT count(*) FROM activity_log
		WHERE action = 'agent_definition_written'
		  AND actor_type = 'agent'
		  AND actor_id = $1
		  AND details->>'target_agent_id' = $2
	`, peopleOps, target).Scan(&audited); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	if audited < 3 {
		t.Errorf("expected an audit row per allow-listed write (update, archive, restore); got %d", audited)
	}
}

// TestAgentDefinitionGate_AllowListedAgentCannotChangeAccess holds the line
// Thorne drew inside the allow-list: permission_mode / invocation_targets /
// visibility stay owner-only for EVERY agent, listed or not. Without this a
// single compromised People Ops seat could re-trust or re-target any other
// seat, which is a wider capability than the one the gate took away.
//
// The no-op resubmit must still be tolerated: upstream's own PATCH-as-PUT
// clients echo the unchanged permission back on unrelated edits, and breaking
// that would be a regression dressed up as a control.
func TestAgentDefinitionGate_AllowListedAgentCannotChangeAccess(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}

	peopleOps := createHandlerTestAgent(t, "gate-access-people-ops", nil)
	target := createHandlerTestAgent(t, "gate-access-target", nil)
	allowListAgent(t, peopleOps)

	// createHandlerTestAgent seeds permission_mode = public_to + a workspace
	// target, so 'private' is a REAL change here.
	req := withURLParam(agentActorReq(peopleOps, http.MethodPut, "/api/agents/"+target,
		map[string]any{"permission_mode": "private"}), "id", target)
	w := httptest.NewRecorder()
	testHandler.UpdateAgent(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("allow-listed agent changing permission_mode: expected 403, got %d: %s", w.Code, w.Body.String())
	}

	var mode string
	if err := testPool.QueryRow(context.Background(),
		`SELECT permission_mode FROM agent WHERE id = $1`, target).Scan(&mode); err != nil {
		t.Fatalf("read back permission_mode: %v", err)
	}
	if mode != "public_to" {
		t.Fatalf("permission_mode changed under a denied write: %q", mode)
	}

	// No-op resubmit alongside a legitimate edit: permitted, and the other
	// field must actually land.
	req = withURLParam(agentActorReq(peopleOps, http.MethodPut, "/api/agents/"+target,
		map[string]any{"permission_mode": "public_to", "instructions": "tuned alongside a no-op permission echo"}), "id", target)
	w = httptest.NewRecorder()
	testHandler.UpdateAgent(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("no-op permission resubmit from an allow-listed agent: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Create must not be the way around the same rule.
	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{"permission_mode on create", map[string]any{"name": "gate-created-1", "runtime_id": testRuntimeID, "permission_mode": "public_to"}},
		{"invocation_targets on create", map[string]any{"name": "gate-created-2", "runtime_id": testRuntimeID, "invocation_targets": []any{}}},
		{"public visibility on create", map[string]any{"name": "gate-created-3", "runtime_id": testRuntimeID, "visibility": "workspace"}},
		{"custom_env on create", map[string]any{"name": "gate-created-4", "runtime_id": testRuntimeID, "custom_env": map[string]string{"API_KEY": "x"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := agentActorReq(peopleOps, http.MethodPost, "/api/agents", tc.body)
			w := httptest.NewRecorder()
			testHandler.CreateAgent(w, req)
			if w.Code != http.StatusForbidden {
				t.Fatalf("expected 403 for %s from an allow-listed agent, got %d: %s", tc.name, w.Code, w.Body.String())
			}
		})
	}
}

// TestAgentDefinitionGate_HumanMemberUnaffected is AC5's first half at the
// handler level: the gate constrains agent principals, not humans. The same
// update that is refused above succeeds for the workspace owner over an
// ordinary (non-task-token) request.
func TestAgentDefinitionGate_HumanMemberUnaffected(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}

	target := createHandlerTestAgent(t, "gate-human-target", nil)

	req := withURLParam(newRequest(http.MethodPut, "/api/agents/"+target,
		map[string]any{"instructions": "set by a human owner"}), "id", target)
	w := httptest.NewRecorder()
	testHandler.UpdateAgent(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("human workspace owner update: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// And a human write is NOT recorded as an agent-definition write by an
	// agent — the audit trail exists to make privileged agent writes visible,
	// so polluting it with human edits would bury the signal.
	var rows int
	if err := testPool.QueryRow(context.Background(), `
		SELECT count(*) FROM activity_log
		WHERE action = 'agent_definition_written' AND details->>'target_agent_id' = $1
	`, target).Scan(&rows); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	if rows != 0 {
		t.Errorf("human edit wrote %d agent_definition_written audit row(s); expected 0", rows)
	}
}

// TestAgentDefinitionWriters_OwnerOnly covers the allow-list's own write path,
// which is what stops the control being circular: no agent may edit the list,
// allow-listed or not, and neither may a non-owner human member. Only the human
// workspace owner can.
func TestAgentDefinitionWriters_OwnerOnly(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}

	peopleOps := createHandlerTestAgent(t, "gate-list-people-ops", nil)
	other := createHandlerTestAgent(t, "gate-list-other", nil)
	allowListAgent(t, peopleOps)
	plainMember := createPermissionTestMember(t, "gate-list-member@multica.test")

	// An allow-listed agent cannot extend the list — the case that would make
	// the gate self-widening.
	req := agentActorReq(peopleOps, http.MethodPut, "/api/agent-definition-writers",
		map[string]any{"agent_ids": []string{peopleOps, other}})
	w := httptest.NewRecorder()
	testHandler.SetAgentDefinitionWriters(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("allow-listed agent editing the allow-list: expected 403, got %d: %s", w.Code, w.Body.String())
	}

	// A non-owner human member cannot either: the list is owner-only, not
	// member-writable.
	req = newRequestAs(plainMember, http.MethodPut, "/api/agent-definition-writers",
		map[string]any{"agent_ids": []string{other}})
	w = httptest.NewRecorder()
	testHandler.SetAgentDefinitionWriters(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("plain member editing the allow-list: expected 403, got %d: %s", w.Code, w.Body.String())
	}

	// The human workspace owner can, and the read-back reflects it.
	req = newRequest(http.MethodPut, "/api/agent-definition-writers",
		map[string]any{"agent_ids": []string{other}})
	w = httptest.NewRecorder()
	testHandler.SetAgentDefinitionWriters(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("workspace owner editing the allow-list: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent_definition_writer WHERE agent_id = $1`, other)
	})

	var listed []AgentDefinitionWriterResponse
	if err := json.Unmarshal(w.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode allow-list response: %v", err)
	}
	if len(listed) != 1 || listed[0].AgentID != other {
		t.Fatalf("expected the replace to leave exactly [%s], got %+v", other, listed)
	}

	// The replace is wholesale, so the seat that was listed before is now off
	// the list — and immediately loses the privilege, with no restart or cache
	// flush in between.
	target := createHandlerTestAgent(t, "gate-list-target", nil)
	req = withURLParam(agentActorReq(peopleOps, http.MethodPut, "/api/agents/"+target,
		map[string]any{"instructions": "should not land"}), "id", target)
	w = httptest.NewRecorder()
	testHandler.UpdateAgent(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("de-listed agent update: expected 403, got %d: %s", w.Code, w.Body.String())
	}

	// Ids must name agents in this workspace: a typo is refused rather than
	// stored as a grant that silently does nothing.
	req = newRequest(http.MethodPut, "/api/agent-definition-writers",
		map[string]any{"agent_ids": []string{"00000000-0000-0000-0000-0000000000ff"}})
	w = httptest.NewRecorder()
	testHandler.SetAgentDefinitionWriters(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown agent id in allow-list: expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

// TestAgentDefinitionWriters_EmptyListFailsClosed is the shipped default: with
// no rows at all, no agent may mutate any definition. Stated as its own test
// because "fail closed" is a claim about the ABSENCE of configuration, and an
// implementation that read an empty list as "unconfigured, allow everything"
// would pass every other test in this file.
func TestAgentDefinitionWriters_EmptyListFailsClosed(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}

	if _, err := testPool.Exec(context.Background(),
		`DELETE FROM agent_definition_writer WHERE workspace_id = $1`, testWorkspaceID); err != nil {
		t.Fatalf("clear allow-list: %v", err)
	}

	actor := createHandlerTestAgent(t, "gate-empty-actor", nil)
	target := createHandlerTestAgent(t, "gate-empty-target", nil)

	req := withURLParam(agentActorReq(actor, http.MethodPut, "/api/agents/"+target,
		map[string]any{"instructions": "should not land"}), "id", target)
	w := httptest.NewRecorder()
	testHandler.UpdateAgent(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("empty allow-list must deny every agent: expected 403, got %d: %s", w.Code, w.Body.String())
	}
}
