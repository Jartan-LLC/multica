package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
)

// Tests for agent-sent chat attribution (Jartan fork).
//
// The finding these cover: an agent run could send a chat message that the
// record attributed to the workspace owner. The sending principal was resolved
// on the way in and then dropped — the queue row was stamped
// `initiator_user_id = <owner>` with `originator_source = 'direct_human'`
// ("a member's own action enqueued the run", which the attribution package
// classifies as compliance-grade precise), and at claim the daemon rendered
// "This task was initiated by <owner>, a member of this workspace" into the one
// prompt block that exists to answer "who am I answering".
//
// So the record did not omit the sender, it asserted a false one. The
// assertions below are the closure condition: a message sent
// from an agent run reaches the recipient with the sending agent named — never
// as a member — and the queue row for that task does not carry source
// `direct_human`.

// insertSendingTask seeds the run that is doing the sending, with its own
// attribution, so the delegation copy has something real to copy from.
func insertSendingTask(t *testing.T, agentID, originatorUserID string) string {
	t.Helper()
	var taskID string
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO agent_task_queue (
			agent_id, runtime_id, status, priority,
			originator_user_id, accountable_user_id, originator_source
		)
		VALUES ($1, $2, 'running', 2, $3, $3, 'direct_human')
		RETURNING id
	`, agentID, handlerTestRuntimeID(t), originatorUserID).Scan(&taskID); err != nil {
		t.Fatalf("insert sending task: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id = $1`, taskID)
	})
	return taskID
}

type queueAttribution struct {
	InitiatorAgentID  *string
	InitiatorUserID   *string
	OriginatorSource  *string
	DelegatedFromTask *string
	AccountableUserID *string
}

func loadQueueAttribution(t *testing.T, taskID string) queueAttribution {
	t.Helper()
	var row queueAttribution
	if err := testPool.QueryRow(context.Background(), `
		SELECT initiator_agent_id::text, initiator_user_id::text, originator_source,
		       delegated_from_task_id::text, accountable_user_id::text
		FROM agent_task_queue WHERE id = $1
	`, taskID).Scan(&row.InitiatorAgentID, &row.InitiatorUserID, &row.OriginatorSource,
		&row.DelegatedFromTask, &row.AccountableUserID); err != nil {
		t.Fatalf("load queue attribution for %s: %v", taskID, err)
	}
	return row
}

// sendChatAs drives SendChatMessage and returns the enqueued task id.
func sendChatAs(t *testing.T, req *http.Request, sessionID string) string {
	t.Helper()
	w := httptest.NewRecorder()
	testHandler.SendChatMessage(w, withURLParam(req, "sessionId", sessionID))
	if w.Code != http.StatusCreated {
		t.Fatalf("SendChatMessage: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var resp SendChatMessageResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode send response: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id = $1`, resp.TaskID)
	})
	return resp.TaskID
}

// TestChatSenderAttribution_AgentSendRecordsTheSendingAgent is the queue-row
// half of the closure condition.
func TestChatSenderAttribution_AgentSendRecordsTheSendingAgent(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	ferro := createHandlerTestAgent(t, "SenderAttrFerro", []byte("[]"))
	anvil := createHandlerTestAgent(t, "SenderAttrAnvil", []byte("[]"))
	sendingTask := insertSendingTask(t, ferro, testUserID)
	session := insertAgentCreatedChatSession(t, anvil, ferro)

	req := chatAgentReq(t, ferro, http.MethodPost, "/api/chat/sessions/"+session+"/messages", map[string]any{
		"content": "Anvil, can you confirm the adapter contract?",
	})
	req.Header.Set("X-Task-ID", sendingTask)
	taskID := sendChatAs(t, req, session)

	row := loadQueueAttribution(t, taskID)

	if row.InitiatorAgentID == nil || *row.InitiatorAgentID != ferro {
		t.Fatalf("initiator_agent_id = %v, want the sending agent %s — the sender is still not recorded", row.InitiatorAgentID, ferro)
	}
	if row.InitiatorUserID != nil {
		t.Fatalf("initiator_user_id = %v on an agent send, want NULL — no human sent this message", *row.InitiatorUserID)
	}
	if row.OriginatorSource == nil || *row.OriginatorSource == "direct_human" {
		t.Fatalf("originator_source = %v, want anything but direct_human — the false member claim is intact", row.OriginatorSource)
	}
	if *row.OriginatorSource != "delegation" {
		t.Fatalf("originator_source = %q, want delegation for a send off an attributed run", *row.OriginatorSource)
	}
	if row.DelegatedFromTask == nil || *row.DelegatedFromTask != sendingTask {
		t.Fatalf("delegated_from_task_id = %v, want the sending run %s — the hop is not traceable", row.DelegatedFromTask, sendingTask)
	}
	if row.AccountableUserID == nil || *row.AccountableUserID != testUserID {
		t.Fatalf("accountable_user_id = %v, want the sending run's human %s copied down", row.AccountableUserID, testUserID)
	}
}

// TestChatSenderAttribution_MemberSendIsUnchanged is the control: the fix must
// not touch the path it was not about.
func TestChatSenderAttribution_MemberSendIsUnchanged(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	anvil := createHandlerTestAgent(t, "SenderAttrMemberAnvil", []byte("[]"))
	session := createHandlerTestChatSession(t, anvil)

	taskID := sendChatAs(t, withChatTestWorkspaceCtx(t, newRequest(http.MethodPost,
		"/api/chat/sessions/"+session+"/messages", map[string]any{"content": "hello from the owner"})), session)

	row := loadQueueAttribution(t, taskID)
	if row.InitiatorAgentID != nil {
		t.Fatalf("initiator_agent_id = %v on a member send, want NULL", *row.InitiatorAgentID)
	}
	if row.InitiatorUserID == nil || *row.InitiatorUserID != testUserID {
		t.Fatalf("initiator_user_id = %v, want the sending member %s", row.InitiatorUserID, testUserID)
	}
	if row.OriginatorSource == nil || *row.OriginatorSource != "direct_human" {
		t.Fatalf("originator_source = %v, want direct_human on a member send", row.OriginatorSource)
	}
}

// TestChatSenderAttribution_ClaimNamesTheSendingAgent is the recipient half of
// the closure condition: what the woken run is actually told. It drives the
// real claim endpoint, because the finding was not a wrong column — it was a
// wrong sentence in the `## Task Initiator` block, and that sentence is built
// from what claim hands the daemon.
func TestChatSenderAttribution_ClaimNamesTheSendingAgent(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	ferro := createHandlerTestAgent(t, "SenderAttrClaimFerro", []byte("[]"))
	anvil := createHandlerTestAgent(t, "SenderAttrClaimAnvil", []byte("[]"))
	sendingTask := insertSendingTask(t, ferro, testUserID)
	session := insertAgentCreatedChatSession(t, anvil, ferro)

	req := chatAgentReq(t, ferro, http.MethodPost, "/api/chat/sessions/"+session+"/messages", map[string]any{
		"content": "who sent this?",
	})
	req.Header.Set("X-Task-ID", sendingTask)
	taskID := sendChatAs(t, req, session)

	// Claim it the way the recipient's daemon does.
	runtimeID := handlerTestRuntimeID(t)
	w := httptest.NewRecorder()
	claimReq := withURLParam(newDaemonTokenRequest(
		http.MethodPost,
		"/api/daemon/runtimes/"+runtimeID+"/claim",
		nil,
		testWorkspaceID,
		"chat-sender-attribution-test",
	), "runtimeId", runtimeID)
	testHandler.ClaimTaskByRuntime(w, claimReq)
	if w.Code != http.StatusOK {
		t.Fatalf("ClaimTaskByRuntime: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var response struct {
		Task *struct {
			ID             string `json:"id"`
			InitiatorType  string `json:"initiator_type"`
			InitiatorID    string `json:"initiator_id"`
			InitiatorName  string `json:"initiator_name"`
			InitiatorEmail string `json:"initiator_email"`
		} `json:"task"`
	}
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatalf("decode claim response: %v", err)
	}
	if response.Task == nil || response.Task.ID != taskID {
		t.Fatalf("claimed task = %+v, want the chat task %s", response.Task, taskID)
	}
	if response.Task.InitiatorType != "agent" {
		t.Fatalf("initiator_type = %q, want agent — the recipient is still told a member sent it", response.Task.InitiatorType)
	}
	if response.Task.InitiatorID != ferro {
		t.Fatalf("initiator_id = %q, want the sending agent %s", response.Task.InitiatorID, ferro)
	}
	if response.Task.InitiatorName != "SenderAttrClaimFerro" {
		t.Fatalf("initiator_name = %q, want the sending agent's name", response.Task.InitiatorName)
	}

	// And the block the recipient actually reads.
	block := execenv.BuildTaskInitiatorBlock(
		response.Task.InitiatorType, response.Task.InitiatorName, response.Task.InitiatorEmail)
	if !strings.Contains(block, "another agent in this workspace") {
		t.Fatalf("## Task Initiator does not name an agent sender:\n%s", block)
	}
	if strings.Contains(block, "a member of this workspace") {
		t.Fatalf("## Task Initiator still claims a member sent the message:\n%s", block)
	}
}

// TestChatSenderAttribution_UnattributedSenderNeverClaimsAHuman covers the
// degraded case: a send from a run with no resolvable human must not silently
// fall back to naming one. It may degrade to owner_fallback (which the package
// marks as NOT compliance-grade), but it must never read direct_human and must
// never carry a human initiator.
func TestChatSenderAttribution_UnattributedSenderNeverClaimsAHuman(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	ferro := createHandlerTestAgent(t, "SenderAttrOrphanFerro", []byte("[]"))
	anvil := createHandlerTestAgent(t, "SenderAttrOrphanAnvil", []byte("[]"))
	session := insertAgentCreatedChatSession(t, anvil, ferro)

	// No X-Task-ID at all: the sending run cannot be read, so no human is
	// recoverable from the chain.
	taskID := sendChatAs(t, chatAgentReq(t, ferro, http.MethodPost,
		"/api/chat/sessions/"+session+"/messages", map[string]any{"content": "orphaned send"}), session)

	row := loadQueueAttribution(t, taskID)
	if row.InitiatorAgentID == nil || *row.InitiatorAgentID != ferro {
		t.Fatalf("initiator_agent_id = %v, want the sending agent %s", row.InitiatorAgentID, ferro)
	}
	if row.InitiatorUserID != nil {
		t.Fatalf("initiator_user_id = %v, want NULL — no human sent this", *row.InitiatorUserID)
	}
	if row.OriginatorSource == nil || *row.OriginatorSource == "direct_human" {
		t.Fatalf("originator_source = %v, want anything but direct_human", row.OriginatorSource)
	}
}
