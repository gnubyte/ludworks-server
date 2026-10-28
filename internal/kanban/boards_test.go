package kanban

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"ludworks/internal/auth/authtest"
)

func TestBoardCRUDTemplatesAndETag(t *testing.T) {
	v := newEnv(t)
	c := v.admin

	b := v.board(c, "  Roadmap\t2027 ", map[string]any{"description": "Plans", "color": "teal"})
	if b.Name != "Roadmap 2027" || b.Color != "teal" || b.Role != "owner" || b.Version != 1 || b.Description != "Plans" {
		t.Fatalf("created %+v", b)
	}
	if got := []string{b.Columns[0].Name, b.Columns[1].Name, b.Columns[2].Name}; len(b.Columns) != 3 ||
		!slices.Equal(got, []string{"To do", "In progress", "Done"}) || b.Columns[2].Position != 2 {
		t.Fatalf("basic template columns %+v", b.Columns)
	}
	if len(b.Members) != 1 || b.Members[0].Role != "owner" || b.Members[0].Username != authtest.AdminUser {
		t.Fatalf("members %+v", b.Members)
	}
	blank := v.board(c, "Alpha", map[string]any{"template": "blank"})
	if len(blank.Columns) != 0 || blank.Color != "blue" {
		t.Fatalf("blank %+v", blank)
	}

	// Validation.
	c.Do("POST", "/api/v1/kanban/boards", map[string]any{"name": " "}).ExpectError(422, "validation_failed")
	c.Do("POST", "/api/v1/kanban/boards", map[string]any{"name": "x", "color": "chartreuse"}).ExpectError(422, "validation_failed")
	c.Do("POST", "/api/v1/kanban/boards", map[string]any{"name": "x", "template": "scrum"}).ExpectError(422, "validation_failed")
	c.Do("POST", "/api/v1/kanban/boards", map[string]any{"name": strings.Repeat("é", 201)}).ExpectError(422, "validation_failed")
	c.Do("POST", "/api/v1/kanban/boards", map[string]any{"name": "a\x00b"}).ExpectError(422, "validation_failed")

	// List: by name, archived filter, counters.
	var list []BoardSummary
	c.Do("GET", "/api/v1/kanban/boards", nil).Expect(http.StatusOK).JSON(&list)
	if len(list) != 2 || list[0].Name != "Alpha" || list[1].Name != "Roadmap 2027" || list[1].MemberCount != 1 || len(list[1].Members) != 1 {
		t.Fatalf("list %+v", list)
	}

	// ETag / If-None-Match.
	r := c.Do("GET", "/api/v1/kanban/boards/"+b.ID, nil).Expect(http.StatusOK)
	etag := r.Header.Get("ETag")
	if etag != `"1"` {
		t.Fatalf("etag %q", etag)
	}
	c.Header.Set("If-None-Match", etag)
	r = c.Do("GET", "/api/v1/kanban/boards/"+b.ID, nil).Expect(http.StatusNotModified)
	if len(r.Body) != 0 || r.Header.Get("ETag") != etag {
		t.Fatalf("304 body %q etag %q", r.Body, r.Header.Get("ETag"))
	}
	c.Header.Set("If-None-Match", `W/"1", "7"`)
	c.Do("GET", "/api/v1/kanban/boards/"+b.ID, nil).Expect(http.StatusNotModified)
	c.Header.Del("If-None-Match")

	// Patch bumps the version; the old ETag no longer matches.
	var pb Board
	c.Do("PATCH", "/api/v1/kanban/boards/"+b.ID, map[string]any{"name": "Roadmap", "color": "pink"}).Expect(http.StatusOK).JSON(&pb)
	if pb.Name != "Roadmap" || pb.Color != "pink" || pb.Version != 2 {
		t.Fatalf("patched %+v", pb)
	}
	c.Header.Set("If-None-Match", etag)
	c.Do("GET", "/api/v1/kanban/boards/"+b.ID, nil).Expect(http.StatusOK)
	c.Header.Del("If-None-Match")
	// A no-op patch changes nothing.
	c.Do("PATCH", "/api/v1/kanban/boards/"+b.ID, map[string]any{"name": "Roadmap"}).Expect(http.StatusOK).JSON(&pb)
	if pb.Version != 2 {
		t.Fatalf("no-op patch bumped version to %d", pb.Version)
	}

	// Archive moves the board to the archived list.
	c.Do("PATCH", "/api/v1/kanban/boards/"+b.ID, map[string]any{"archived": true}).Expect(http.StatusOK).JSON(&pb)
	if !pb.Archived {
		t.Fatal("not archived")
	}
	c.Do("GET", "/api/v1/kanban/boards", nil).Expect(http.StatusOK).JSON(&list)
	if len(list) != 1 || list[0].ID != blank.ID {
		t.Fatalf("active list %+v", list)
	}
	c.Do("GET", "/api/v1/kanban/boards?archived=true", nil).Expect(http.StatusOK).JSON(&list)
	if len(list) != 1 || list[0].ID != b.ID || !list[0].Archived {
		t.Fatalf("archived list %+v", list)
	}
	c.Do("GET", "/api/v1/kanban/boards?archived=maybe", nil).ExpectError(422, "validation_failed")

	// Delete.
	c.Do("DELETE", "/api/v1/kanban/boards/"+b.ID, nil).Expect(http.StatusNoContent)
	c.Do("GET", "/api/v1/kanban/boards/"+b.ID, nil).ExpectError(404, "not_found")
	c.Do("GET", "/api/v1/kanban/boards/not-a-valid-id!", nil).ExpectError(404, "not_found")
}

func TestBoardSummaryCounters(t *testing.T) {
	v := newEnv(t)
	c := v.admin
	b := v.board(c, "Counts")
	todo := b.Columns[0].ID
	v.card(c, todo, "overdue", map[string]any{"due_at": "2020-01-01T00:00:00Z"})
	v.card(c, todo, "future", map[string]any{"due_at": "2999-01-01T00:00:00Z"})
	done := v.card(c, todo, "done & overdue", map[string]any{"due_at": "2020-01-01T00:00:00Z"})
	c.Do("PATCH", "/api/v1/kanban/cards/"+done.ID, map[string]any{"completed": true}).Expect(http.StatusOK)
	arch := v.card(c, todo, "archived")
	c.Do("PATCH", "/api/v1/kanban/cards/"+arch.ID, map[string]any{"archived": true}).Expect(http.StatusOK)
	var list []BoardSummary
	c.Do("GET", "/api/v1/kanban/boards", nil).Expect(http.StatusOK).JSON(&list)
	s := list[0]
	if s.CardCount != 3 || s.OpenCardCount != 2 || s.OverdueCount != 1 {
		t.Fatalf("summary counters %+v", s)
	}
	got := v.getBoard(c, b.ID)
	if got.CardCount != 3 || len(got.Cards) != 3 || got.Columns[0].CardCount != 3 {
		t.Fatalf("board counters card_count=%d cards=%d column=%d", got.CardCount, len(got.Cards), got.Columns[0].CardCount)
	}
	all := v.getBoard(c, b.ID, "include_archived=1")
	if len(all.Cards) != 4 || all.CardCount != 3 {
		t.Fatalf("include_archived: %d cards", len(all.Cards))
	}
}

func TestRolesMatrix(t *testing.T) {
	v := newEnv(t)
	owner := v.admin
	adminID, adminC := v.user("boardadmin")
	editorID, editorC := v.user("editor")
	viewerID, viewerC := v.user("viewer")
	_, outsider := v.user("outsider")

	b := v.board(owner, "Team")
	v.members(owner, b.ID, map[string]string{adminID: "admin", editorID: "editor", viewerID: "viewer"})
	col := b.Columns[0].ID
	card := v.card(owner, col, "Shared card")

	type op struct {
		name   string
		method string
		path   string
		body   any
		min    string // least role that may do it: viewer, editor, admin, owner
	}
	ops := []op{
		{"read board", "GET", "/api/v1/kanban/boards/" + b.ID, nil, "viewer"},
		{"read card", "GET", "/api/v1/kanban/cards/" + card.ID, nil, "viewer"},
		{"read activity", "GET", "/api/v1/kanban/boards/" + b.ID + "/activity", nil, "viewer"},
		{"comment", "POST", "/api/v1/kanban/cards/" + card.ID + "/comments", map[string]any{"body": "hi"}, "viewer"},
		{"create card", "POST", "/api/v1/kanban/columns/" + col + "/cards", map[string]any{"title": "x"}, "editor"},
		{"patch card", "PATCH", "/api/v1/kanban/cards/" + card.ID, map[string]any{"priority": "high"}, "editor"},
		{"move card", "POST", "/api/v1/kanban/cards/" + card.ID + "/move", map[string]any{"column_id": col}, "editor"},
		{"add subtask", "POST", "/api/v1/kanban/cards/" + card.ID + "/subtasks", map[string]any{"title": "s"}, "editor"},
		{"create column", "POST", "/api/v1/kanban/boards/" + b.ID + "/columns", map[string]any{"name": "c"}, "editor"},
		{"patch column", "PATCH", "/api/v1/kanban/columns/" + col, map[string]any{"wip_limit": 3}, "editor"},
		{"move column", "POST", "/api/v1/kanban/columns/" + col + "/move", map[string]any{"position": 0}, "editor"},
		{"patch board", "PATCH", "/api/v1/kanban/boards/" + b.ID, map[string]any{"description": "d"}, "admin"},
	}
	rank := map[string]int{"viewer": 1, "editor": 2, "admin": 3, "owner": 4}
	clients := []struct {
		role string
		c    *authtest.Client
	}{{"viewer", viewerC}, {"editor", editorC}, {"admin", adminC}, {"owner", owner}}
	for _, o := range ops {
		for _, cl := range clients {
			r := cl.c.Do(o.method, o.path, o.body)
			allowed := rank[cl.role] >= rank[o.min]
			switch {
			case allowed && r.Status >= 300:
				t.Errorf("%s as %s: %d %s", o.name, cl.role, r.Status, r.Body)
			case !allowed && r.Status != http.StatusForbidden:
				t.Errorf("%s as %s: %d, want 403", o.name, cl.role, r.Status)
			}
		}
		// Non-members never learn the board exists.
		if r := outsider.Do(o.method, o.path, o.body); r.Status != http.StatusNotFound {
			t.Errorf("%s as outsider: %d, want 404", o.name, r.Status)
		}
	}
	// Server admins get no implicit access: a board of another user is 404 for them.
	other := v.board(editorC, "Private")
	owner.Do("GET", "/api/v1/kanban/boards/"+other.ID, nil).ExpectError(404, "not_found")

	// Members, transfer and delete.
	body := map[string]any{"members": []map[string]any{{"user_id": adminID, "role": "admin"}, {"user_id": editorID, "role": "editor"}, {"user_id": viewerID, "role": "viewer"}}}
	editorC.Do("PUT", "/api/v1/kanban/boards/"+b.ID+"/members", body).ExpectError(403, "forbidden")
	adminC.Do("PUT", "/api/v1/kanban/boards/"+b.ID+"/members", body).Expect(http.StatusOK)
	adminC.Do("DELETE", "/api/v1/kanban/boards/"+b.ID, nil).ExpectError(403, "forbidden")
	adminC.Do("POST", "/api/v1/kanban/boards/"+b.ID+"/transfer", map[string]any{"user_id": editorID}).ExpectError(403, "forbidden")
	outsider.Do("DELETE", "/api/v1/kanban/boards/"+b.ID, nil).ExpectError(404, "not_found")

	// Token scopes: kanban:read cannot write; kanban:write is required for writes.
	ro := v.token(editorC, "kanban:read")
	ro.Do("GET", "/api/v1/kanban/boards/"+b.ID, nil).Expect(http.StatusOK)
	ro.Do("POST", "/api/v1/kanban/columns/"+col+"/cards", map[string]any{"title": "x"}).ExpectError(403, "forbidden")
	ro.Do("POST", "/api/v1/kanban/boards/"+b.ID+"/leave", nil).Expect(http.StatusNoContent) // leave needs only kanban:read
	none := v.token(viewerC, "notes:read")
	none.Do("GET", "/api/v1/kanban/boards", nil).ExpectError(403, "forbidden")
}

func TestMembersLeaveTransfer(t *testing.T) {
	v := newEnv(t)
	owner := v.admin
	aliceID, alice := v.user("alice")
	bobID, bob := v.user("bob")
	b := v.board(owner, "Members")
	col := b.Columns[0].ID

	// Validation.
	put := func(c *authtest.Client, list ...map[string]any) *authtest.Response {
		return c.Do("PUT", "/api/v1/kanban/boards/"+b.ID+"/members", map[string]any{"members": list})
	}
	put(owner, map[string]any{"user_id": aliceID, "role": "owner"}).ExpectError(422, "validation_failed")
	put(owner, map[string]any{"user_id": aliceID, "role": "editor"}, map[string]any{"user_id": aliceID, "role": "viewer"}).ExpectError(422, "validation_failed")
	put(owner, map[string]any{"user_id": "nosuchuser0001", "role": "editor"}).ExpectError(422, "validation_failed")
	owner.Do("PUT", "/api/v1/kanban/boards/"+b.ID+"/members", map[string]any{}).ExpectError(422, "validation_failed")

	bd := v.members(owner, b.ID, map[string]string{aliceID: "editor", bobID: "admin"})
	if len(bd.Members) != 3 || bd.Members[0].Role != "owner" || bd.Members[1].Role != "admin" || bd.Members[1].DisplayName != "User bob" {
		t.Fatalf("members %+v", bd.Members)
	}
	// The owner entry in a replacement list is ignored; an admin cannot remove themselves.
	owner.Do("PUT", "/api/v1/kanban/boards/"+b.ID+"/members", map[string]any{"members": []map[string]any{
		{"user_id": bd.OwnerID, "role": "viewer"}, {"user_id": aliceID, "role": "editor"}, {"user_id": bobID, "role": "admin"}}}).Expect(http.StatusOK)
	bob.Do("PUT", "/api/v1/kanban/boards/"+b.ID+"/members", map[string]any{"members": []map[string]any{
		{"user_id": aliceID, "role": "editor"}}}).ExpectError(422, "validation_failed")

	// Assignments go away with the membership.
	card := v.card(owner, col, "Assigned", map[string]any{"assignees": []string{aliceID, bobID}})
	var sub Subtask
	owner.Do("POST", "/api/v1/kanban/cards/"+card.ID+"/subtasks", map[string]any{"title": "s", "assignee_id": aliceID}).Expect(201).JSON(&sub)
	if !slices.Equal(card.Assignees, []string{aliceID, bobID}) {
		t.Fatalf("assignees %v", card.Assignees)
	}
	alice.Do("POST", "/api/v1/kanban/boards/"+b.ID+"/leave", nil).Expect(http.StatusNoContent)
	var after Card
	owner.Do("GET", "/api/v1/kanban/cards/"+card.ID, nil).Expect(200).JSON(&after)
	if !slices.Equal(after.Assignees, []string{bobID}) || after.Version <= card.Version {
		t.Fatalf("after leave: assignees %v version %d→%d", after.Assignees, card.Version, after.Version)
	}
	var detail CardDetail
	owner.Do("GET", "/api/v1/kanban/cards/"+card.ID, nil).Expect(200).JSON(&detail)
	if detail.Subtasks[0].AssigneeID != nil {
		t.Fatal("subtask still assigned after leave")
	}
	alice.Do("GET", "/api/v1/kanban/boards/"+b.ID, nil).ExpectError(404, "not_found")
	// Removing via PUT also unassigns.
	v.members(owner, b.ID, map[string]string{})
	owner.Do("GET", "/api/v1/kanban/cards/"+card.ID, nil).Expect(200).JSON(&after)
	if len(after.Assignees) != 0 {
		t.Fatalf("assignees after removal %v", after.Assignees)
	}

	// The owner cannot leave; transfer makes the old owner an admin.
	owner.Do("POST", "/api/v1/kanban/boards/"+b.ID+"/leave", nil).ExpectError(409, "conflict")
	owner.Do("POST", "/api/v1/kanban/boards/"+b.ID+"/transfer", map[string]any{"user_id": bobID}).ExpectError(422, "validation_failed")
	v.members(owner, b.ID, map[string]string{bobID: "viewer"})
	var tb Board
	owner.Do("POST", "/api/v1/kanban/boards/"+b.ID+"/transfer", map[string]any{"user_id": bobID}).Expect(http.StatusOK).JSON(&tb)
	if tb.OwnerID != bobID || tb.Role != "admin" || tb.Members[0].UserID != bobID || tb.Members[0].Role != "owner" {
		t.Fatalf("after transfer %+v", tb)
	}
	owner.Do("DELETE", "/api/v1/kanban/boards/"+b.ID, nil).ExpectError(403, "forbidden")
	owner.Do("POST", "/api/v1/kanban/boards/"+b.ID+"/leave", nil).Expect(http.StatusNoContent)
	bob.Do("DELETE", "/api/v1/kanban/boards/"+b.ID, nil).Expect(http.StatusNoContent)
}

func TestUserSearch(t *testing.T) {
	v := newEnv(t)
	_, _ = v.user("marta")
	_, _ = v.user("martin")
	disabledID, _ := v.user("marty")
	v.admin.Do("PATCH", "/api/v1/admin/users/"+disabledID, map[string]any{"disabled": true}).Expect(http.StatusOK)
	var res []map[string]any
	v.admin.Do("GET", "/api/v1/kanban/users?q=mart", nil).Expect(http.StatusOK).JSON(&res)
	if len(res) != 2 || res[0]["username"] != "marta" || res[1]["username"] != "martin" {
		t.Fatalf("search %v", res)
	}
	for _, u := range res {
		if _, ok := u["email"]; ok || len(u) != 3 {
			t.Fatalf("result leaks fields: %v", u)
		}
	}
	v.admin.Do("GET", "/api/v1/kanban/users?q=User%20mar", nil).Expect(http.StatusOK).JSON(&res) // display name prefix
	if len(res) != 2 {
		t.Fatalf("display-name search %v", res)
	}
	v.admin.Do("GET", "/api/v1/kanban/users?q=m", nil).Expect(http.StatusOK).JSON(&res)
	if len(res) != 0 {
		t.Fatalf("one-letter query answered %v", res)
	}
	v.admin.Do("GET", "/api/v1/kanban/users?q=%25%25", nil).Expect(http.StatusOK).JSON(&res) // LIKE wildcards are literal
	if len(res) != 0 {
		t.Fatalf("wildcard query answered %v", res)
	}
}

func TestActivityLog(t *testing.T) {
	v := newEnv(t)
	c := v.admin
	b := v.board(c, "Log")
	todo, doing := b.Columns[0].ID, b.Columns[1].ID
	card := v.card(c, todo, "Write tests")
	c.Do("PATCH", "/api/v1/kanban/cards/"+card.ID, map[string]any{"title": "Write more tests", "priority": "high"}).Expect(200)
	c.Do("POST", "/api/v1/kanban/cards/"+card.ID+"/move", map[string]any{"column_id": doing}).Expect(200)
	c.Do("PATCH", "/api/v1/kanban/cards/"+card.ID, map[string]any{"completed": true}).Expect(200)
	var page struct {
		Items      []Activity `json:"items"`
		NextCursor *string    `json:"next_cursor"`
	}
	c.Do("GET", "/api/v1/kanban/boards/"+b.ID+"/activity?card_id="+card.ID, nil).Expect(200).JSON(&page)
	var actions []string
	for _, a := range page.Items {
		actions = append(actions, a.Action)
	}
	if !slices.Equal(actions, []string{"card.completed", "card.moved", "card.updated", "card.created"}) {
		t.Fatalf("actions %v", actions)
	}
	moved := page.Items[1]
	if moved.Meta["from_column"] != "To do" || moved.Meta["to_column"] != "In progress" || moved.ActorName != authtest.AdminUser {
		t.Fatalf("moved %+v", moved)
	}
	if f, _ := page.Items[2].Meta["fields"].([]any); len(f) != 2 || page.Items[2].Meta["old_title"] != "Write tests" {
		t.Fatalf("updated meta %v", page.Items[2].Meta)
	}
	// Pagination, newest first.
	var p1, p2 struct {
		Items      []Activity `json:"items"`
		NextCursor *string    `json:"next_cursor"`
	}
	c.Do("GET", "/api/v1/kanban/boards/"+b.ID+"/activity?limit=2", nil).Expect(200).JSON(&p1)
	if len(p1.Items) != 2 || p1.NextCursor == nil {
		t.Fatalf("page 1 %+v", p1)
	}
	c.Do("GET", "/api/v1/kanban/boards/"+b.ID+"/activity?limit=50&cursor="+*p1.NextCursor, nil).Expect(200).JSON(&p2)
	if len(p2.Items) != 3 || p2.NextCursor != nil || p2.Items[2].Action != "board.created" || p2.Items[0].CreatedAt > p1.Items[1].CreatedAt {
		t.Fatalf("page 2 %+v", p2)
	}
	// A cursor of another board (or filter) is refused.
	other := v.board(c, "Other")
	c.Do("GET", "/api/v1/kanban/boards/"+other.ID+"/activity?cursor="+*p1.NextCursor, nil).ExpectError(400, "bad_request")
	// The card detail carries the newest entries too.
	var d CardDetail
	c.Do("GET", "/api/v1/kanban/cards/"+card.ID, nil).Expect(200).JSON(&d)
	if len(d.Activity) != 4 || d.Activity[0].Action != "card.completed" {
		t.Fatalf("detail activity %+v", d.Activity)
	}
}

func TestActivityPrune(t *testing.T) {
	v := newEnv(t)
	b := v.board(v.admin, "Busy")
	ctx := t.Context()
	tx, err := v.e.Deps.DB.SQL.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxActivityPerBoard+20; i++ {
		if err := logActivity(ctx, tx, b.ID, "", b.OwnerID, "test.entry", map[string]any{"i": i}, now()); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := v.m.pruneActivity(ctx); err != nil {
		t.Fatal(err)
	}
	var n int
	var newest string
	v.e.Deps.DB.SQL.QueryRow(`SELECT count(*) FROM kanban_activity WHERE board_id = ?`, b.ID).Scan(&n)
	v.e.Deps.DB.SQL.QueryRow(`SELECT meta FROM kanban_activity WHERE board_id = ? ORDER BY n DESC LIMIT 1`, b.ID).Scan(&newest)
	if n != maxActivityPerBoard || !strings.Contains(newest, itoa(maxActivityPerBoard+19)) {
		t.Fatalf("after prune: %d entries, newest %s", n, newest)
	}
}
