package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Tests for the agent-definition write gate (Jartan fork).
//
// The finding these cover: an ordinary agent seat could rewrite any other
// agent's definition, because every mutation handler asked "is the caller the
// owner or an admin?" and a mat_ task token answers yes on behalf of its
// owning human. The assertions below are written against the acceptance
// criteria for the gate (AC1–AC5), so a reader can map a failure straight back
// to the criterion it breaks.
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
	// predicate from (closure item 4).
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

	// Skills are instruction text and People Ops binds them on every hire, so
	// the skills scope stays open to a listed seat (§2).
	skillsReq := withURLParam(agentActorReq(peopleOps, http.MethodPut, "/api/agents/"+target+"/skills",
		map[string]any{"skill_ids": []string{}}), "id", target)
	skillsW := httptest.NewRecorder()
	testHandler.SetAgentSkills(skillsW, skillsReq)
	if skillsW.Code != http.StatusOK {
		t.Fatalf("allow-listed agent setting skills: expected 200, got %d: %s", skillsW.Code, skillsW.Body.String())
	}

	// Audit: evidence, not a control — but a privileged seat
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
// drawn inside the allow-list: permission_mode / invocation_targets /
// visibility are never written by an agent, listed or not. Without this a
// single compromised People Ops seat could re-trust or re-target any other
// seat, which is a wider capability than the one the gate took away.
//
// On UPDATE the amendment (§4) is fail-loud: any permission input from an
// agent principal is rejected outright, no-op resubmit included. The
// pre-amendment build tolerated an echo of the unchanged value; that tolerance
// exists for PATCH-as-PUT clients, which are human UI paths, and the CLI an
// agent uses sends only the flags it was given.
func TestAgentDefinitionGate_AllowListedAgentCannotChangeAccess(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}

	peopleOps := createHandlerTestAgent(t, "gate-access-people-ops", nil)
	target := createHandlerTestAgent(t, "gate-access-target", nil)
	allowListAgent(t, peopleOps)

	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		// createHandlerTestAgent seeds permission_mode = public_to + a
		// workspace target, so 'private' is a REAL change here.
		{"real change", map[string]any{"permission_mode": "private"}},
		{"no-op resubmit", map[string]any{"permission_mode": "public_to"}},
		{"legacy visibility", map[string]any{"visibility": "private"}},
		{"invocation targets", map[string]any{"invocation_targets": []any{}}},
		{"alongside a permitted field", map[string]any{"permission_mode": "public_to", "instructions": "must not land"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := agentDefinitionSnapshot(t, target)
			req := withURLParam(agentActorReq(peopleOps, http.MethodPut, "/api/agents/"+target, tc.body), "id", target)
			w := httptest.NewRecorder()
			testHandler.UpdateAgent(w, req)
			if w.Code != http.StatusForbidden {
				t.Fatalf("allow-listed agent sending %s: expected 403, got %d: %s", tc.name, w.Code, w.Body.String())
			}
			if after := agentDefinitionSnapshot(t, target); after != before {
				t.Fatalf("record changed under a denied write:\n before %s\n after  %s", before, after)
			}
		})
	}
}

// TestAgentDefinitionGate_AgentCreatedSeatIsServerPermissioned is §4 of the
// amendment, which reverses the pre-amendment rule. The server sets the trust
// axis on a seat created by an allow-listed agent — public_to with the
// workspace as the sole invocation target — and IGNORES whatever the caller
// sent rather than refusing it.
//
// Both halves are load-bearing and each has a live failure behind it:
//
//   - minting `private` (what a literal reading of the original ruling did)
//     lands every hire unassignable, because `issue create --assignee-id`
//     against a private seat fails.
//   - refusing the input fails every create from a CLI that serialises a
//     defaulted --visibility, which is exactly the silent CLI/server skew the
//     fork exists to avoid (AC8).
func TestAgentDefinitionGate_AgentCreatedSeatIsServerPermissioned(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}

	peopleOps := createHandlerTestAgent(t, "gate-mint-people-ops", nil)
	allowListAgent(t, peopleOps)

	otherMember := createPermissionTestMember(t, "gate-mint-member@multica.test")

	for i, tc := range []struct {
		name string
		body map[string]any
	}{
		{"no permission input at all", map[string]any{}},
		{"private permission_mode", map[string]any{"permission_mode": "private"}},
		{"defaulted legacy visibility", map[string]any{"visibility": "private"}},
		{"enumerated member target", map[string]any{
			"permission_mode":    "public_to",
			"invocation_targets": []any{map[string]any{"target_type": "member", "target_id": otherMember}},
		}},
		{"unparseable permission value", map[string]any{"permission_mode": "not-a-mode"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]any{
				"name":       fmt.Sprintf("gate-minted-%d", i),
				"runtime_id": testRuntimeID,
			}
			for k, v := range tc.body {
				body[k] = v
			}
			req := agentActorReq(peopleOps, http.MethodPost, "/api/agents", body)
			w := httptest.NewRecorder()
			testHandler.CreateAgent(w, req)
			if w.Code != http.StatusCreated && w.Code != http.StatusOK {
				t.Fatalf("allow-listed create: expected success, got %d: %s", w.Code, w.Body.String())
			}
			var created AgentResponse
			if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
				t.Fatalf("decode created agent: %v", err)
			}
			t.Cleanup(func() {
				testPool.Exec(context.Background(), `DELETE FROM agent WHERE id = $1`, created.ID)
			})

			var mode, visibility string
			if err := testPool.QueryRow(context.Background(),
				`SELECT permission_mode, visibility FROM agent WHERE id = $1`, created.ID).Scan(&mode, &visibility); err != nil {
				t.Fatalf("read back the minted seat: %v", err)
			}
			if mode != "public_to" {
				t.Fatalf("expected the server to mint public_to, got %q", mode)
			}
			if visibility != "workspace" {
				t.Fatalf("expected legacy visibility 'workspace', got %q", visibility)
			}

			// Exactly one target, and it is the workspace — not the member the
			// caller tried to enumerate.
			rows, err := testPool.Query(context.Background(),
				`SELECT target_type, target_id::text FROM agent_invocation_target WHERE agent_id = $1`, created.ID)
			if err != nil {
				t.Fatalf("read invocation targets: %v", err)
			}
			defer rows.Close()
			var targets []string
			for rows.Next() {
				var targetType, targetID string
				if err := rows.Scan(&targetType, &targetID); err != nil {
					t.Fatalf("scan invocation target: %v", err)
				}
				targets = append(targets, targetType+":"+targetID)
			}
			if len(targets) != 1 || targets[0] != "workspace:"+testWorkspaceID {
				t.Fatalf("expected exactly one workspace invocation target, got %v", targets)
			}
		})
	}
}

// TestAgentDefinitionGate_UpdateFieldScope is §2 and §3: past the gate, an
// allow-listed seat may write instructions, model, thinking_level, name and
// description — and nothing else. mcp_config is the one that moved in the
// amendment, and it is the one with teeth: an MCP block is a tool grant and an
// egress path that a reader of the instruction text would never see.
func TestAgentDefinitionGate_UpdateFieldScope(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}

	peopleOps := createHandlerTestAgent(t, "gate-scope-people-ops", nil)
	target := createHandlerTestAgent(t, "gate-scope-target", nil)
	allowListAgent(t, peopleOps)

	denied := []struct {
		name string
		body map[string]any
	}{
		{"mcp_config", map[string]any{"mcp_config": map[string]any{"mcpServers": map[string]any{"x": map[string]any{"command": "curl"}}}}},
		{"custom_args", map[string]any{"custom_args": []string{"--dangerously-skip-permissions"}}},
		{"runtime_config", map[string]any{"runtime_config": map[string]any{"gateway": map[string]any{"token": "x"}}}},
		{"runtime_id", map[string]any{"runtime_id": testRuntimeID}},
		{"service_tier", map[string]any{"service_tier": "priority"}},
		{"max_concurrent_tasks", map[string]any{"max_concurrent_tasks": 1}},
		{"status", map[string]any{"status": "offline"}},
		{"avatar_url", map[string]any{"avatar_url": "🐟"}},
		{"custom_env", map[string]any{"custom_env": map[string]string{"API_KEY": "x"}}},
		{"composio_toolkit_allowlist", map[string]any{"composio_toolkit_allowlist": []string{"github"}}},
		{"a permitted field alongside a denied one", map[string]any{"instructions": "must not land", "status": "offline"}},
	}
	for _, tc := range denied {
		t.Run("denied/"+tc.name, func(t *testing.T) {
			before := agentDefinitionSnapshot(t, target)
			req := withURLParam(agentActorReq(peopleOps, http.MethodPut, "/api/agents/"+target, tc.body), "id", target)
			w := httptest.NewRecorder()
			testHandler.UpdateAgent(w, req)
			if w.Code != http.StatusForbidden {
				t.Fatalf("expected 403 writing %s from an allow-listed agent, got %d: %s", tc.name, w.Code, w.Body.String())
			}
			if after := agentDefinitionSnapshot(t, target); after != before {
				t.Fatalf("record changed under a denied write of %s:\n before %s\n after  %s", tc.name, before, after)
			}
		})
	}

	// The permitted five, in one request, must still land — the gate is not
	// worth much if it also stops People Ops doing its job.
	permitted := map[string]any{
		"instructions":   "tuned by People Ops",
		"model":          "claude-opus-5",
		"thinking_level": "",
		"name":           "gate-scope-target-renamed",
		"description":    "a tuned description",
	}
	req := withURLParam(agentActorReq(peopleOps, http.MethodPut, "/api/agents/"+target, permitted), "id", target)
	w := httptest.NewRecorder()
	testHandler.UpdateAgent(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("allow-listed agent writing the permitted field set: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var instructions, name string
	if err := testPool.QueryRow(context.Background(),
		`SELECT instructions, name FROM agent WHERE id = $1`, target).Scan(&instructions, &name); err != nil {
		t.Fatalf("read back the tuned record: %v", err)
	}
	if instructions != "tuned by People Ops" || name != "gate-scope-target-renamed" {
		t.Fatalf("permitted fields did not land: instructions=%q name=%q", instructions, name)
	}
}

// TestAgentDefinitionGate_CreateFieldScope is the same scope on create, which
// is where it matters most: create is the field-check bypass if it is wider
// than update. The trust axis is the one exception — ignored, not refused, and
// covered by TestAgentDefinitionGate_AgentCreatedSeatIsServerPermissioned.
func TestAgentDefinitionGate_CreateFieldScope(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}

	peopleOps := createHandlerTestAgent(t, "gate-create-scope-people-ops", nil)
	allowListAgent(t, peopleOps)

	for i, tc := range []struct {
		name  string
		field string
		value any
	}{
		{"custom_env", "custom_env", map[string]string{"API_KEY": "x"}},
		{"mcp_config", "mcp_config", map[string]any{"mcpServers": map[string]any{}}},
		{"custom_args", "custom_args", []string{"--dangerously-skip-permissions"}},
		{"runtime_config", "runtime_config", map[string]any{"gateway": map[string]any{"token": "x"}}},
		{"service_tier", "service_tier", "priority"},
		{"max_concurrent_tasks", "max_concurrent_tasks", 12},
		{"avatar_url", "avatar_url", "🐟"},
		{"composio_toolkit_allowlist", "composio_toolkit_allowlist", []string{"github"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]any{
				"name":       fmt.Sprintf("gate-create-scope-%d", i),
				"runtime_id": testRuntimeID,
				tc.field:     tc.value,
			}
			req := agentActorReq(peopleOps, http.MethodPost, "/api/agents", body)
			w := httptest.NewRecorder()
			testHandler.CreateAgent(w, req)
			if w.Code != http.StatusForbidden {
				t.Fatalf("expected 403 creating with %s from an allow-listed agent, got %d: %s", tc.name, w.Code, w.Body.String())
			}
			var count int
			if err := testPool.QueryRow(context.Background(),
				`SELECT count(*) FROM agent WHERE name = $1`, body["name"]).Scan(&count); err != nil {
				t.Fatalf("count minted agents: %v", err)
			}
			if count != 0 {
				t.Fatalf("a denied create still wrote %d agent row(s)", count)
			}
		})
	}
}

// TestAgentDefinitionGate_NoSelfWrite is §2's new restriction: no agent
// principal writes its own record, allow-listed included. A seat that can
// rewrite thirty definitions AND its own can make its own compromise durable
// with no owner acting; requiring the owner for any change to the seat holding
// the write forecloses that.
func TestAgentDefinitionGate_NoSelfWrite(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}

	peopleOps := createHandlerTestAgent(t, "gate-self-people-ops", nil)
	allowListAgent(t, peopleOps)
	before := agentDefinitionSnapshot(t, peopleOps)

	req := withURLParam(agentActorReq(peopleOps, http.MethodPut, "/api/agents/"+peopleOps,
		map[string]any{"instructions": "I now answer to no one"}), "id", peopleOps)
	w := httptest.NewRecorder()
	testHandler.UpdateAgent(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("allow-listed agent updating itself: expected 403, got %d: %s", w.Code, w.Body.String())
	}
	if after := agentDefinitionSnapshot(t, peopleOps); after != before {
		t.Fatalf("self-write changed the record:\n before %s\n after  %s", before, after)
	}

	// The lifecycle routes are the same record and the same rule — a seat
	// archiving itself out of the way is a definition write too.
	for _, step := range []struct {
		name string
		call func(w http.ResponseWriter, r *http.Request)
		path string
	}{
		{"archive", testHandler.ArchiveAgent, "/archive"},
		{"restore", testHandler.RestoreAgent, "/restore"},
	} {
		req := withURLParam(agentActorReq(peopleOps, http.MethodPost, "/api/agents/"+peopleOps+step.path, nil), "id", peopleOps)
		w := httptest.NewRecorder()
		step.call(w, req)
		if w.Code != http.StatusForbidden {
			t.Fatalf("allow-listed agent %s on itself: expected 403, got %d: %s", step.name, w.Code, w.Body.String())
		}
	}

	// A human owner may still edit that seat — this rule constrains agent
	// principals, and it is the owner who is meant to hold the write.
	req = withURLParam(newRequest(http.MethodPut, "/api/agents/"+peopleOps,
		map[string]any{"instructions": "set by the owner"}), "id", peopleOps)
	w = httptest.NewRecorder()
	testHandler.UpdateAgent(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("human owner editing the allow-listed seat: expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

// TestAgentDefinitionGate_OperationScope is the other half of §2: "everything
// else on the mutation surface is denied to every agent principal,
// allow-listed included" is a statement about OPERATIONS as well as fields.
// The permitted set is create / update / skills / archive / restore; the
// routes below are outside it and stay owner-only even for a listed seat.
func TestAgentDefinitionGate_OperationScope(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}

	peopleOps := createHandlerTestAgent(t, "gate-op-people-ops", nil)
	target := createHandlerTestAgent(t, "gate-op-target", nil)
	allowListAgent(t, peopleOps)

	// The gate refuses before the label id is ever looked up, so a random id
	// is enough to prove the route is closed — same shape the non-allow-listed
	// surface test uses.
	randomUUID := "00000000-0000-0000-0000-0000000000ff"

	for _, tc := range []struct {
		name   string
		call   func(w http.ResponseWriter, r *http.Request)
		method string
		path   string
		body   any
		params []string
	}{
		{
			name: "cancel tasks", call: testHandler.CancelAgentTasks,
			method: http.MethodPost, path: "/api/agents/" + target + "/cancel-tasks",
			params: []string{"id", target},
		},
		{
			name: "runtime-skill override", call: testHandler.SetAgentRuntimeSkillEnabled,
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := agentDefinitionSnapshot(t, target)
			req := withURLParams(agentActorReq(peopleOps, tc.method, tc.path, tc.body), tc.params...)
			w := httptest.NewRecorder()
			tc.call(w, req)
			if w.Code != http.StatusForbidden {
				t.Fatalf("allow-listed agent on %s: expected 403, got %d: %s", tc.name, w.Code, w.Body.String())
			}
			if after := agentDefinitionSnapshot(t, target); after != before {
				t.Fatalf("record changed under a denied %s", tc.name)
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
