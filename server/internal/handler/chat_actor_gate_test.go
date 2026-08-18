package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

// Tests for chat-session access by principal (Jartan fork, SEC-2026-0078).
//
// The finding these cover: from inside an ordinary agent run, using that run's
// own `mat_` token, `GET /api/chat/sessions?status=all` returned EVERY chat
// session in the workspace — the owner's private conversations with other
// agents included — and `GET /api/chat/sessions/{id}/messages` returned their
// full transcripts. Chat authorisation keyed on `chat_session.creator_id`, and
// the auth middleware stamps a task token's `X-User-ID` with the agent's owning
// human, so sixteen seats and the owner all satisfy the creator predicate.
//
// The assertions below are written against Thorne's closure condition on
// JAR-616: an ordinary agent seat's session list contains only sessions
// belonging to that seat, and a request against an owner-created session
// returns 403 or 404.
//
// Every test drives the handlers with the header shape the auth middleware
// produces for a task token — X-Actor-Source: task_token plus X-Agent-ID — and
// leaves X-User-ID at the OWNING human, which is the whole point: that human
// clears the creator check the fix replaces.

// chatAgentReq builds a chat request as the auth middleware leaves it for a
// mat_ task token, with the workspace + member context the handlers read.
func chatAgentReq(t *testing.T, actingAgentID, method, path string, body any) *http.Request {
	t.Helper()
	req := withChatTestWorkspaceCtx(t, newRequest(method, path, body))
	req.Header.Set("X-Actor-Source", "task_token")
	req.Header.Set("X-Agent-ID", actingAgentID)
	return req
}

// insertAgentCreatedChatSession seeds a session created BY an agent run, the
// shape an agent-to-agent conversation has: pinned to `agentID`, created by
// `creatorAgentID`, with the owning human still on creator_id because that is
// what a task token carries.
func insertAgentCreatedChatSession(t *testing.T, agentID, creatorAgentID string) string {
	t.Helper()
	var sessionID string
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO chat_session (workspace_id, agent_id, creator_id, creator_agent_id, title, status)
		VALUES ($1, $2, $3, $4, 'actor-gate-test', 'active')
		RETURNING id
	`, testWorkspaceID, agentID, testUserID, creatorAgentID).Scan(&sessionID); err != nil {
		t.Fatalf("insert agent-created chat session: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM chat_session WHERE id = $1`, sessionID)
	})
	return sessionID
}

// listedSessionIDs drives ListChatSessions and returns the ids it handed back.
func listedSessionIDs(t *testing.T, req *http.Request) map[string]bool {
	t.Helper()
	w := httptest.NewRecorder()
	testHandler.ListChatSessions(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("ListChatSessions: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var sessions []ChatSessionResponse
	if err := json.NewDecoder(w.Body).Decode(&sessions); err != nil {
		t.Fatalf("decode session list: %v", err)
	}
	ids := make(map[string]bool, len(sessions))
	for _, s := range sessions {
		ids[s.ID] = true
	}
	return ids
}

// TestChatActorGate_AgentSeatListsOnlyItsOwnSessions is the first half of
// Thorne's closure condition: `GET /api/chat/sessions?status=all` from an
// ordinary agent seat returns only sessions belonging to that seat.
//
// Three sessions exist, all with the owner on creator_id — which is what every
// row in this workspace has, agent-created or not:
//
//	owner  -> Noema   : the owner's private conversation with another agent
//	Ferro  -> Anvil   : opened by Ferro's run, pinned to Anvil
//	owner  -> Ferro   : the owner's conversation with Ferro itself
//
// Ferro's run may see the last two and must not see the first.
func TestChatActorGate_AgentSeatListsOnlyItsOwnSessions(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	ferro := createHandlerTestAgent(t, "ActorGateFerro", []byte("[]"))
	anvil := createHandlerTestAgent(t, "ActorGateAnvil", []byte("[]"))
	noema := createHandlerTestAgent(t, "ActorGateNoema", []byte("[]"))

	ownerWithNoema := insertChatSessionAs(t, noema, testUserID)
	ferroToAnvil := insertAgentCreatedChatSession(t, anvil, ferro)
	ownerWithFerro := insertChatSessionAs(t, ferro, testUserID)

	t.Run("the owner still sees all three", func(t *testing.T) {
		ids := listedSessionIDs(t, withChatTestWorkspaceCtx(t, newRequest(http.MethodGet, "/api/chat/sessions?status=all", nil)))
		for _, want := range []string{ownerWithNoema, ferroToAnvil, ownerWithFerro} {
			if !ids[want] {
				t.Fatalf("owner's list is missing session %s — the fix must not touch member requests", want)
			}
		}
	})

	t.Run("an agent seat sees only its own", func(t *testing.T) {
		ids := listedSessionIDs(t, chatAgentReq(t, ferro, http.MethodGet, "/api/chat/sessions?status=all", nil))
		if ids[ownerWithNoema] {
			t.Fatalf("Ferro's run listed the owner's private session with another agent (%s) — SEC-2026-0078 is not closed", ownerWithNoema)
		}
		if !ids[ferroToAnvil] {
			t.Fatalf("Ferro's run cannot see the session its own run created (%s) — agent-to-agent chat is broken by the gate", ferroToAnvil)
		}
		if !ids[ownerWithFerro] {
			t.Fatalf("Ferro's run cannot see its own pinned conversation (%s)", ownerWithFerro)
		}
	})

	t.Run("a third seat sees neither of the other two", func(t *testing.T) {
		ids := listedSessionIDs(t, chatAgentReq(t, noema, http.MethodGet, "/api/chat/sessions?status=all", nil))
		if ids[ferroToAnvil] {
			t.Fatalf("Noema's run listed a session between two other agents (%s)", ferroToAnvil)
		}
		if ids[ownerWithFerro] {
			t.Fatalf("Noema's run listed the owner's conversation with another agent (%s)", ownerWithFerro)
		}
		if !ids[ownerWithNoema] {
			t.Fatalf("Noema's run cannot see its own pinned conversation (%s)", ownerWithNoema)
		}
	})

	t.Run("the active-only list is scoped the same way", func(t *testing.T) {
		// status="" takes the other query branch in ListChatSessions; a fix
		// applied to one branch and not the other would leave the finding open
		// on the default list the client actually calls.
		ids := listedSessionIDs(t, chatAgentReq(t, ferro, http.MethodGet, "/api/chat/sessions", nil))
		if ids[ownerWithNoema] {
			t.Fatalf("the active-session branch still lists the owner's private session (%s)", ownerWithNoema)
		}
	})
}

// TestChatActorGate_AgentSeatCannotReadAnotherSessionsTranscript is the second
// half of the closure condition: a per-session read against an owner-created
// session is refused. Every per-session chat handler funnels through
// loadChatSessionForUser, so this covers the transcript, the session itself,
// and the paged transcript together.
func TestChatActorGate_AgentSeatCannotReadAnotherSessionsTranscript(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	ferro := createHandlerTestAgent(t, "ActorGateReadFerro", []byte("[]"))
	noema := createHandlerTestAgent(t, "ActorGateReadNoema", []byte("[]"))
	ownerWithNoema := insertChatSessionAs(t, noema, testUserID)

	// A message the owner would recognise: the finding was that this content
	// came back in the response payload.
	if _, err := testPool.Exec(context.Background(), `
		INSERT INTO chat_message (chat_session_id, role, content) VALUES ($1, 'user', 'private to the owner')
	`, ownerWithNoema); err != nil {
		t.Fatalf("seed chat message: %v", err)
	}

	refused := map[string]func(http.ResponseWriter, *http.Request){
		"GetChatSession":       testHandler.GetChatSession,
		"ListChatMessages":     testHandler.ListChatMessages,
		"ListChatMessagesPage": testHandler.ListChatMessagesPage,
		"MarkChatSessionRead":  testHandler.MarkChatSessionRead,
		"SendChatMessage":      testHandler.SendChatMessage,
	}
	for name, handler := range refused {
		t.Run(name+" is refused", func(t *testing.T) {
			w := httptest.NewRecorder()
			req := withURLParam(
				chatAgentReq(t, ferro, http.MethodPost, "/api/chat/sessions/"+ownerWithNoema, map[string]any{"content": "hello"}),
				"sessionId", ownerWithNoema,
			)
			handler(w, req)
			if w.Code != http.StatusNotFound && w.Code != http.StatusForbidden {
				t.Fatalf("%s returned %d, want 404 or 403: %s", name, w.Code, w.Body.String())
			}
			if body := w.Body.String(); strings.Contains(body, "private to the owner") {
				t.Fatalf("%s leaked the owner's message content: %s", name, body)
			}
		})
	}

	t.Run("the owner is unaffected", func(t *testing.T) {
		w := httptest.NewRecorder()
		req := withURLParam(
			withChatTestWorkspaceCtx(t, newRequest(http.MethodGet, "/api/chat/sessions/"+ownerWithNoema+"/messages", nil)),
			"sessionId", ownerWithNoema,
		)
		testHandler.ListChatMessages(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("owner's own transcript read returned %d, want 200: %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "private to the owner") {
			t.Fatalf("owner's transcript no longer contains its own message — the fix over-reached")
		}
	})

	t.Run("the pinned agent still reaches its own session", func(t *testing.T) {
		w := httptest.NewRecorder()
		req := withURLParam(
			chatAgentReq(t, noema, http.MethodGet, "/api/chat/sessions/"+ownerWithNoema+"/messages", nil),
			"sessionId", ownerWithNoema,
		)
		testHandler.ListChatMessages(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("the session's own pinned agent was refused its transcript: %d %s", w.Code, w.Body.String())
		}
	})
}

// TestChatActorGate_CreateStampsTheCreatingAgent proves the column the gate
// depends on is written from the request's own task token, and that the seat
// which opened a session can then read it back while a third seat cannot.
// This is the path an agent-to-agent channel needs: without it the fix would
// close the finding by breaking the feature.
func TestChatActorGate_CreateStampsTheCreatingAgent(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	ferro := createHandlerTestAgent(t, "ActorGateCreateFerro", []byte("[]"))
	anvil := createHandlerTestAgent(t, "ActorGateCreateAnvil", []byte("[]"))
	loom := createHandlerTestAgent(t, "ActorGateCreateLoom", []byte("[]"))

	w := httptest.NewRecorder()
	testHandler.CreateChatSession(w, chatAgentReq(t, ferro, http.MethodPost, "/api/chat/sessions", map[string]any{
		"agent_id": anvil,
		"title":    "Ferro -> Anvil",
	}))
	if w.Code != http.StatusCreated {
		t.Fatalf("CreateChatSession as an agent principal: %d %s", w.Code, w.Body.String())
	}
	var created ChatSessionResponse
	if err := json.NewDecoder(w.Body).Decode(&created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM chat_session WHERE id = $1`, created.ID)
	})

	var storedCreatorAgent *string
	if err := testPool.QueryRow(context.Background(),
		`SELECT creator_agent_id::text FROM chat_session WHERE id = $1`, created.ID).Scan(&storedCreatorAgent); err != nil {
		t.Fatalf("load creator_agent_id: %v", err)
	}
	if storedCreatorAgent == nil || *storedCreatorAgent != ferro {
		t.Fatalf("creator_agent_id = %v, want the calling agent %s", storedCreatorAgent, ferro)
	}
	if created.CreatorAgentID != ferro {
		t.Fatalf("response creator_agent_id = %q, want %s", created.CreatorAgentID, ferro)
	}

	t.Run("the creating seat can read it back", func(t *testing.T) {
		w := httptest.NewRecorder()
		req := withURLParam(chatAgentReq(t, ferro, http.MethodGet, "/api/chat/sessions/"+created.ID, nil), "sessionId", created.ID)
		testHandler.GetChatSession(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("the creating agent was refused its own session: %d %s", w.Code, w.Body.String())
		}
	})

	t.Run("an uninvolved seat cannot", func(t *testing.T) {
		w := httptest.NewRecorder()
		req := withURLParam(chatAgentReq(t, loom, http.MethodGet, "/api/chat/sessions/"+created.ID, nil), "sessionId", created.ID)
		testHandler.GetChatSession(w, req)
		if w.Code != http.StatusNotFound && w.Code != http.StatusForbidden {
			t.Fatalf("a third agent reached a session between two others: %d %s", w.Code, w.Body.String())
		}
	})

	t.Run("a member creating a session stamps nothing", func(t *testing.T) {
		w := httptest.NewRecorder()
		testHandler.CreateChatSession(w, withChatTestWorkspaceCtx(t, newRequest(http.MethodPost, "/api/chat/sessions", map[string]any{
			"agent_id": anvil,
			"title":    "owner -> Anvil",
		})))
		if w.Code != http.StatusCreated {
			t.Fatalf("CreateChatSession as the owner: %d %s", w.Code, w.Body.String())
		}
		var memberCreated ChatSessionResponse
		if err := json.NewDecoder(w.Body).Decode(&memberCreated); err != nil {
			t.Fatalf("decode create response: %v", err)
		}
		t.Cleanup(func() {
			testPool.Exec(context.Background(), `DELETE FROM chat_session WHERE id = $1`, memberCreated.ID)
		})
		var stored *string
		if err := testPool.QueryRow(context.Background(),
			`SELECT creator_agent_id::text FROM chat_session WHERE id = $1`, memberCreated.ID).Scan(&stored); err != nil {
			t.Fatalf("load creator_agent_id: %v", err)
		}
		if stored != nil {
			t.Fatalf("creator_agent_id = %v on a member-created session, want NULL", *stored)
		}
	})
}

// TestChatActorGate_PendingTaskEndpointsAreScoped covers the sibling list
// endpoints. They are not named in the closure condition, but they are the same
// creator_id predicate over the same table: without the scope they hand an
// agent run the session ids of every in-flight chat in the workspace.
func TestChatActorGate_PendingTaskEndpointsAreScoped(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	ferro := createHandlerTestAgent(t, "ActorGatePendingFerro", []byte("[]"))
	noema := createHandlerTestAgent(t, "ActorGatePendingNoema", []byte("[]"))
	ownerWithNoema := insertChatSessionAs(t, noema, testUserID)
	insertPendingChatTask(t, noema, ownerWithNoema, "running")

	t.Run("the owner sees the pending task", func(t *testing.T) {
		w := httptest.NewRecorder()
		testHandler.ListPendingChatTasks(w, chatPendingCtxAs(t, newRequest(http.MethodGet, "/api/chat/pending-tasks", nil), testUserID))
		if w.Code != http.StatusOK {
			t.Fatalf("ListPendingChatTasks: %d %s", w.Code, w.Body.String())
		}
		var resp PendingChatTasksResponse
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(resp.Tasks) == 0 {
			t.Fatal("the owner's own pending chat task disappeared — the fix over-reached")
		}
	})

	t.Run("an uninvolved agent seat sees none of it", func(t *testing.T) {
		w := httptest.NewRecorder()
		testHandler.ListPendingChatTasks(w, chatAgentReq(t, ferro, http.MethodGet, "/api/chat/pending-tasks", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("ListPendingChatTasks: %d %s", w.Code, w.Body.String())
		}
		var resp PendingChatTasksResponse
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		for _, task := range resp.Tasks {
			if task.ChatSessionID == ownerWithNoema {
				t.Fatalf("Ferro's run listed a pending task on the owner's session with another agent (%s)", ownerWithNoema)
			}
		}

		hasW := httptest.NewRecorder()
		testHandler.HasPendingChatTasks(hasW, chatAgentReq(t, ferro, http.MethodGet, "/api/chat/pending-tasks/any", nil))
		if hasW.Code != http.StatusOK {
			t.Fatalf("HasPendingChatTasks: %d %s", hasW.Code, hasW.Body.String())
		}
		var has HasPendingChatTasksResponse
		if err := json.NewDecoder(hasW.Body).Decode(&has); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if has.HasPending {
			t.Fatal("HasPendingChatTasks said true to a seat with no reachable pending task")
		}
	})

	t.Run("the pinned agent still sees its own", func(t *testing.T) {
		w := httptest.NewRecorder()
		testHandler.HasPendingChatTasks(w, chatAgentReq(t, noema, http.MethodGet, "/api/chat/pending-tasks/any", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("HasPendingChatTasks: %d %s", w.Code, w.Body.String())
		}
		var has HasPendingChatTasksResponse
		if err := json.NewDecoder(w.Body).Decode(&has); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !has.HasPending {
			t.Fatal("the session's own pinned agent no longer sees its own pending task")
		}
	})
}

// TestChatActorGate_ScopePredicate pins the rule itself, without a database, so
// a rebase that rewrites the handlers still has to keep the two openings and
// the deny-by-default between them.
func TestChatActorGate_ScopePredicate(t *testing.T) {
	pinned := parseUUID("11111111-1111-1111-1111-111111111111")
	creator := parseUUID("22222222-2222-2222-2222-222222222222")
	other := "33333333-3333-3333-3333-333333333333"

	member := newChatActorScope("member", "a-user-id")
	if !member.allows(pinned, creator) {
		t.Fatal("a member scope must allow everything its creator check already allowed")
	}
	if member.creatorAgentID().Valid {
		t.Fatal("a member scope must stamp NULL as the creating agent")
	}

	asPinned := newChatActorScope("agent", uuidToString(pinned))
	if !asPinned.allows(pinned, creator) {
		t.Fatal("the session's pinned agent must reach its own session")
	}

	asCreator := newChatActorScope("agent", uuidToString(creator))
	if !asCreator.allows(pinned, creator) {
		t.Fatal("the agent that created the session must reach it")
	}
	if got := asCreator.creatorAgentID(); !got.Valid || uuidToString(got) != uuidToString(creator) {
		t.Fatalf("creatorAgentID = %v, want the calling agent", got)
	}

	asOther := newChatActorScope("agent", other)
	if asOther.allows(pinned, creator) {
		t.Fatal("an uninvolved agent must not reach the session")
	}
	if asOther.allows(pinned, pgtype.UUID{}) {
		t.Fatal("a human-created session must be unreachable from an uninvolved agent")
	}

	blank := newChatActorScope("agent", "")
	if blank.allows(pinned, creator) {
		t.Fatal("an agent scope with no agent id must fail closed, not open")
	}
}
