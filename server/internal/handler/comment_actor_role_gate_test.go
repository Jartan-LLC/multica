package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

// commentActorGateFixture creates an issue and one comment authored by the
// workspace-owner member (testUserID), for exercising UpdateComment /
// DeleteComment's author-or-admin gate.
func commentActorGateFixture(t *testing.T) (issueID, commentID string) {
	t.Helper()
	ctx := context.Background()

	if err := testPool.QueryRow(ctx, `
		INSERT INTO issue (workspace_id, creator_type, creator_id, title)
		VALUES ($1, 'member', $2, $3)
		RETURNING id
	`, testWorkspaceID, testUserID, "comment actor gate fixture").Scan(&issueID); err != nil {
		t.Fatalf("create issue: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, issueID)
	})

	if err := testPool.QueryRow(ctx, `
		INSERT INTO comment (issue_id, workspace_id, author_type, author_id, content, type)
		VALUES ($1, $2, 'member', $3, 'authored by the workspace owner', 'comment')
		RETURNING id
	`, issueID, testWorkspaceID, testUserID).Scan(&commentID); err != nil {
		t.Fatalf("create comment: %v", err)
	}

	return issueID, commentID
}

func machineActorRequest(method, path string, body any, agentID string) *http.Request {
	r := newRequest(method, path, body)
	r.Header.Set("X-Actor-Source", "task_token")
	r.Header.Set("X-Agent-ID", agentID)
	return r
}

func commentContent(t *testing.T, commentID string) string {
	t.Helper()
	var content string
	if err := testPool.QueryRow(context.Background(),
		`SELECT content FROM comment WHERE id = $1`, commentID).Scan(&content); err != nil {
		t.Fatalf("load comment content: %v", err)
	}
	return content
}

func commentExists(t *testing.T, commentID string) bool {
	t.Helper()
	var exists bool
	if err := testPool.QueryRow(context.Background(),
		`SELECT EXISTS(SELECT 1 FROM comment WHERE id = $1)`, commentID).Scan(&exists); err != nil {
		t.Fatalf("check comment existence: %v", err)
	}
	return exists
}

// TestUpdateComment_MachineCredentialCannotEditOthersComment is closure
// condition (1) for this gate: a task-token request holds no
// workspace role, even though the token's X-User-ID resolves to the
// workspace owner (testUserID). Before the fix, isAdmin read
// roleAllowed(member.Role, "owner", "admin") — true for every agent run in
// this fixture — so the gate never denied. It must now.
func TestUpdateComment_MachineCredentialCannotEditOthersComment(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	_, commentID := commentActorGateFixture(t)

	w := httptest.NewRecorder()
	r := machineActorRequest(http.MethodPut, "/api/comments/"+commentID,
		map[string]any{"content": "rewritten by an agent, attributed to the owner"}, uuid.NewString())
	r = withURLParam(r, "commentId", commentID)

	testHandler.UpdateComment(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("machine actor UpdateComment on a comment it did not author: expected 403, got %d: %s", w.Code, w.Body.String())
	}
	if got := commentContent(t, commentID); got != "authored by the workspace owner" {
		t.Fatalf("comment content must be unchanged after a denied edit, got %q", got)
	}
}

// TestDeleteComment_MachineCredentialCannotDeleteOthersComment is closure
// condition (2), exercised at the handler the CLI's
// `multica issue comment delete` reaches with no crafted request.
func TestDeleteComment_MachineCredentialCannotDeleteOthersComment(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	_, commentID := commentActorGateFixture(t)

	w := httptest.NewRecorder()
	r := machineActorRequest(http.MethodDelete, "/api/comments/"+commentID, nil, uuid.NewString())
	r = withURLParam(r, "commentId", commentID)

	testHandler.DeleteComment(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("machine actor DeleteComment on a comment it did not author: expected 403, got %d: %s", w.Code, w.Body.String())
	}
	if !commentExists(t, commentID) {
		t.Fatal("comment must still exist after a denied delete")
	}
}

// TestUpdateComment_MachineCredentialCanEditOwnComment is condition (3)'s
// agent half: an agent editing its OWN comment must keep working — the fix
// narrows isAdmin, it must not touch isAuthor.
func TestUpdateComment_MachineCredentialCanEditOwnComment(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	ctx := context.Background()
	agentID := uuid.NewString()

	var issueID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO issue (workspace_id, creator_type, creator_id, title)
		VALUES ($1, 'member', $2, $3)
		RETURNING id
	`, testWorkspaceID, testUserID, "agent own comment fixture").Scan(&issueID); err != nil {
		t.Fatalf("create issue: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, issueID)
	})

	var commentID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO comment (issue_id, workspace_id, author_type, author_id, content, type)
		VALUES ($1, $2, 'agent', $3, 'authored by the agent itself', 'comment')
		RETURNING id
	`, issueID, testWorkspaceID, agentID).Scan(&commentID); err != nil {
		t.Fatalf("create comment: %v", err)
	}

	w := httptest.NewRecorder()
	r := machineActorRequest(http.MethodPut, "/api/comments/"+commentID,
		map[string]any{"content": "edited by its own author"}, agentID)
	r = withURLParam(r, "commentId", commentID)

	testHandler.UpdateComment(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("agent editing its own comment: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if got := commentContent(t, commentID); got != "edited by its own author" {
		t.Fatalf("expected content to be updated, got %q", got)
	}
}

// TestUpdateComment_MemberAdminRightsUnchanged is condition (3)'s human
// half: a workspace owner editing another MEMBER's comment (not their own)
// must still succeed — actorHasWorkspaceRole only denies machine
// credentials, never a human request.
func TestUpdateComment_MemberAdminRightsUnchanged(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	ctx := context.Background()
	otherMemberID := addSecondWorkspaceMember(t, "comment-gate-other-"+uuid.NewString()[:8]+"@multica.ai")

	var issueID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO issue (workspace_id, creator_type, creator_id, title)
		VALUES ($1, 'member', $2, $3)
		RETURNING id
	`, testWorkspaceID, otherMemberID, "member admin fixture").Scan(&issueID); err != nil {
		t.Fatalf("create issue: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, issueID)
	})

	var commentID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO comment (issue_id, workspace_id, author_type, author_id, content, type)
		VALUES ($1, $2, 'member', $3, 'authored by the other member', 'comment')
		RETURNING id
	`, issueID, testWorkspaceID, otherMemberID).Scan(&commentID); err != nil {
		t.Fatalf("create comment: %v", err)
	}

	// testUserID is the workspace owner (see setupHandlerTestFixture) and a
	// genuine JWT/PAT request — no X-Actor-Source.
	w := httptest.NewRecorder()
	r := newRequest(http.MethodPut, "/api/comments/"+commentID, map[string]any{"content": "edited by the workspace owner"})
	r = withURLParam(r, "commentId", commentID)

	testHandler.UpdateComment(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("workspace owner editing another member's comment: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if got := commentContent(t, commentID); got != "edited by the workspace owner" {
		t.Fatalf("expected content to be updated, got %q", got)
	}

	// The non-admin author's own comment is still theirs to edit.
	w2 := httptest.NewRecorder()
	r2 := newRequestAs(otherMemberID, http.MethodPut, "/api/comments/"+commentID, map[string]any{"content": "edited by the original author"})
	r2 = withURLParam(r2, "commentId", commentID)
	testHandler.UpdateComment(w2, r2)
	if w2.Code != http.StatusOK {
		t.Fatalf("original author editing their own comment: expected 200, got %d: %s", w2.Code, w2.Body.String())
	}
}

// TestDeleteComment_MachineCredentialCanDeleteOwnComment is
// TestUpdateComment_MachineCredentialCanEditOwnComment's delete-side twin.
func TestDeleteComment_MachineCredentialCanDeleteOwnComment(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	ctx := context.Background()
	agentID := uuid.NewString()

	var issueID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO issue (workspace_id, creator_type, creator_id, title)
		VALUES ($1, 'member', $2, $3)
		RETURNING id
	`, testWorkspaceID, testUserID, "agent own comment delete fixture").Scan(&issueID); err != nil {
		t.Fatalf("create issue: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, issueID)
	})

	var commentID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO comment (issue_id, workspace_id, author_type, author_id, content, type)
		VALUES ($1, $2, 'agent', $3, 'authored by the agent itself', 'comment')
		RETURNING id
	`, issueID, testWorkspaceID, agentID).Scan(&commentID); err != nil {
		t.Fatalf("create comment: %v", err)
	}

	w := httptest.NewRecorder()
	r := machineActorRequest(http.MethodDelete, "/api/comments/"+commentID, nil, agentID)
	r = withURLParam(r, "commentId", commentID)

	testHandler.DeleteComment(w, r)

	if w.Code != http.StatusNoContent {
		t.Fatalf("agent deleting its own comment: expected 204, got %d: %s", w.Code, w.Body.String())
	}
	if commentExists(t, commentID) {
		t.Fatal("comment must be gone after the author deletes it")
	}
}

// TestUpdateComment_NonAdminMemberCannotEditOthersComment is the unchanged-
// behavior sanity check: a plain member was already denied editing another
// member's comment before this fix (isAdmin was already false for them),
// and actorHasWorkspaceRole must not change that.
func TestUpdateComment_NonAdminMemberCannotEditOthersComment(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	issueID, commentID := commentActorGateFixture(t)
	_ = issueID
	otherMemberID := addSecondWorkspaceMember(t, "comment-gate-nonadmin-"+uuid.NewString()[:8]+"@multica.ai")

	w := httptest.NewRecorder()
	r := newRequestAs(otherMemberID, http.MethodPut, "/api/comments/"+commentID, map[string]any{"content": "an outsider tries to edit"})
	r = withURLParam(r, "commentId", commentID)

	testHandler.UpdateComment(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("non-admin member editing another member's comment: expected 403, got %d: %s", w.Code, w.Body.String())
	}
	if got := commentContent(t, commentID); got != "authored by the workspace owner" {
		t.Fatalf("comment content must be unchanged after a denied edit, got %q", got)
	}
}
