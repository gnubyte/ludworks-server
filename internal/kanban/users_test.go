package kanban

import (
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestAccountDeletion covers user.deleted: the account's own boards go (attachment files
// included), its memberships and assignments are removed from other boards (whose versions move
// on), and its comments and activity show "Deleted user".
func TestAccountDeletion(t *testing.T) {
	v := newEnv(t)
	owner := v.admin
	aliceID, alice := v.user("alice")
	bobID, _ := v.user("bob")

	// alice's own board, with an attachment on disk.
	own := v.board(alice, "Alice's board")
	ownCard := v.card(alice, own.Columns[0].ID, "Mine")
	var ownAtt Attachment
	v.upload(alice, ownCard.ID, "a.png", pngBytes(t, 8, 8)).expect(t, 201).decode(t, &ownAtt)
	if _, err := os.Stat(filepath.Join(v.m.attachDir, ownAtt.ID)); err != nil {
		t.Fatal(err)
	}

	// A shared board where alice is an editor with assignments and a comment.
	shared := v.board(owner, "Shared")
	v.members(owner, shared.ID, map[string]string{aliceID: "editor", bobID: "viewer"})
	card := v.card(owner, shared.Columns[0].ID, "Task", map[string]any{"assignees": []string{bobID, aliceID}})
	var sub Subtask
	owner.Do("POST", "/api/v1/kanban/cards/"+card.ID+"/subtasks", map[string]any{"title": "step", "assignee_id": aliceID}).Expect(201).JSON(&sub)
	var cm Comment
	alice.Do("POST", "/api/v1/kanban/cards/"+card.ID+"/comments", map[string]any{"body": "on it"}).Expect(201).JSON(&cm)
	alice.Do("PATCH", "/api/v1/kanban/cards/"+card.ID, map[string]any{"priority": "urgent"}).Expect(200)
	before := v.getBoard(owner, shared.ID)
	var cardBefore Card
	owner.Do("GET", "/api/v1/kanban/cards/"+card.ID, nil).Expect(200).JSON(&cardBefore)

	owner.Do("DELETE", "/api/v1/admin/users/"+aliceID, nil).Expect(http.StatusNoContent)

	// Her board is gone, with its attachment file.
	var n int
	v.e.Deps.DB.SQL.QueryRow(`SELECT count(*) FROM kanban_boards WHERE id = ?`, own.ID).Scan(&n)
	if n != 0 {
		t.Fatal("owned board survived account deletion")
	}
	eventually(t, "attachment file removal", func() bool {
		_, err := os.Stat(filepath.Join(v.m.attachDir, ownAtt.ID))
		return os.IsNotExist(err)
	})

	after := v.getBoard(owner, shared.ID)
	if after.Version <= before.Version {
		t.Fatalf("board version %d → %d", before.Version, after.Version)
	}
	for _, mb := range after.Members {
		if mb.UserID == aliceID {
			t.Fatal("deleted account is still a member")
		}
	}
	var d CardDetail
	owner.Do("GET", "/api/v1/kanban/cards/"+card.ID, nil).Expect(200).JSON(&d)
	if !slices.Equal(d.Assignees, []string{bobID}) || d.Version <= cardBefore.Version {
		t.Fatalf("assignees %v version %d → %d", d.Assignees, cardBefore.Version, d.Version)
	}
	if d.Subtasks[0].AssigneeID != nil {
		t.Fatal("subtask still assigned to the deleted account")
	}
	if len(d.Comments) != 1 || d.Comments[0].AuthorName != deletedUser || d.Comments[0].AuthorID != aliceID {
		t.Fatalf("comments %+v", d.Comments)
	}
	var page struct{ Items []Activity }
	owner.Do("GET", "/api/v1/kanban/boards/"+shared.ID+"/activity", nil).Expect(200).JSON(&page)
	if a := page.Items[0]; a.Action != "member.removed" || a.Meta["account_deleted"] != true || a.ActorName != deletedUser {
		t.Fatalf("newest activity %+v", a)
	}
	foundUpdate := false
	for _, a := range page.Items {
		if a.Action == "card.updated" && a.ActorID == aliceID && a.ActorName == deletedUser {
			foundUpdate = true
		}
	}
	if !foundUpdate {
		t.Fatal("the deleted account's activity lost its actor")
	}
}
