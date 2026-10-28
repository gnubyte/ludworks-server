package kanban

import (
	"fmt"
	"math/rand/v2"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"ludworks/internal/auth/authtest"
	"ludworks/internal/core"
	"ludworks/internal/db"
)

func TestCardOrderingAndMoves(t *testing.T) {
	v := newEnv(t)
	c := v.admin
	b := v.board(c, "Order")
	todo, doing := b.Columns[0].ID, b.Columns[1].ID
	for _, title := range []string{"a", "b", "c", "d"} {
		v.card(c, todo, title)
	}
	first := v.card(c, todo, "first", map[string]any{"position": -5}) // clamped to 0
	v.card(c, todo, "last", map[string]any{"position": 999})          // clamped to the end
	v.card(c, todo, "middle", map[string]any{"position": 3})
	if got := columnTitles(v.getBoard(c, b.ID), todo); !slices.Equal(got, []string{"first", "a", "b", "middle", "c", "d", "last"}) {
		t.Fatalf("after creates %v", got)
	}
	move := func(id, column string, pos any) Card {
		t.Helper()
		body := map[string]any{"column_id": column}
		if pos != nil {
			body["position"] = pos
		}
		var out Card
		c.Do("POST", "/api/v1/kanban/cards/"+id+"/move", body).Expect(http.StatusOK).JSON(&out)
		return out
	}
	moved := move(first.ID, todo, 4) // down within the column
	if moved.Position != 4 || moved.Version != first.Version {
		t.Fatalf("moved %+v (a move must not change the card version)", moved)
	}
	if got := columnTitles(v.getBoard(c, b.ID), todo); !slices.Equal(got, []string{"a", "b", "middle", "c", "first", "d", "last"}) {
		t.Fatalf("after move down %v", got)
	}
	move(first.ID, todo, 1) // up
	if got := columnTitles(v.getBoard(c, b.ID), todo); !slices.Equal(got, []string{"a", "first", "b", "middle", "c", "d", "last"}) {
		t.Fatalf("after move up %v", got)
	}
	move(first.ID, doing, nil) // other column, default end
	move(first.ID, todo, 99)   // back, clamped to the end
	if got := columnTitles(v.getBoard(c, b.ID), todo); !slices.Equal(got, []string{"a", "b", "middle", "c", "d", "last", "first"}) {
		t.Fatalf("after round trip %v", got)
	}
	bd := v.getBoard(c, b.ID)
	var mid Card
	for _, x := range bd.Cards {
		if x.Title == "middle" {
			mid = x
		}
	}
	move(mid.ID, doing, 0)
	bd = v.getBoard(c, b.ID)
	if got := columnTitles(bd, doing); !slices.Equal(got, []string{"middle"}) {
		t.Fatalf("doing %v", got)
	}
	assertDense(t, bd)
	// Archived cards keep their place.
	var arch Card
	for _, x := range bd.Cards {
		if x.Title == "b" {
			arch = x
		}
	}
	c.Do("PATCH", "/api/v1/kanban/cards/"+arch.ID, map[string]any{"archived": true}).Expect(200)
	bd = v.getBoard(c, b.ID)
	if got := columnTitles(bd, todo); !slices.Equal(got, []string{"a", "c", "d", "last", "first"}) {
		t.Fatalf("visible %v", got)
	}
	all := v.getBoard(c, b.ID, "include_archived=true")
	if got := columnTitles(all, todo); !slices.Equal(got, []string{"a", "b", "c", "d", "last", "first"}) {
		t.Fatalf("with archived %v", got)
	}
	assertDense(t, all)
	// Column moves.
	var cols []Column
	c.Do("POST", "/api/v1/kanban/columns/"+todo+"/move", map[string]any{"position": 7}).Expect(200).JSON(&cols)
	if len(cols) != 3 || cols[2].ID != todo || cols[0].Name != "In progress" || cols[2].Position != 2 {
		t.Fatalf("columns %+v", cols)
	}
	c.Do("POST", "/api/v1/kanban/columns/"+todo+"/move", map[string]any{}).ExpectError(422, "validation_failed")
}

// assertDense checks that every column's cards (archived included) and the columns themselves
// are numbered 0..n-1.
func assertDense(t *testing.T, b Board) {
	t.Helper()
	per := map[string][]int{}
	for _, c := range b.Cards {
		per[c.ColumnID] = append(per[c.ColumnID], c.Position)
	}
	for col, ps := range per {
		slices.Sort(ps)
		for i, p := range ps {
			if p != i {
				t.Fatalf("column %s positions %v not dense", col, ps)
			}
		}
	}
	for i, c := range b.Columns {
		if c.Position != i {
			t.Fatalf("column positions not dense: %+v", b.Columns)
		}
	}
}

// TestRandomMovesStayDense runs random creates, moves (within and across columns) and deletes
// against a model of the expected order.
func TestRandomMovesStayDense(t *testing.T) {
	v := newEnv(t)
	c := v.admin
	b := v.board(c, "Random")
	cols := []string{b.Columns[0].ID, b.Columns[1].ID, b.Columns[2].ID}
	model := map[string][]string{} // column → card ids in order
	titles := map[string]string{}
	rnd := rand.New(rand.NewPCG(7, 11))
	insert := func(list []string, pos int, id string) []string {
		pos = min(max(pos, 0), len(list))
		return slices.Insert(list, pos, id)
	}
	for step := 0; step < 120; step++ {
		total := len(model[cols[0]]) + len(model[cols[1]]) + len(model[cols[2]])
		switch op := rnd.IntN(10); {
		case op < 3 || total < 3:
			col := cols[rnd.IntN(3)]
			pos := rnd.IntN(len(model[col])+3) - 1
			title := fmt.Sprintf("card %d", step)
			card := v.card(c, col, title, map[string]any{"position": pos})
			model[col] = insert(model[col], pos, card.ID)
			titles[card.ID] = title
		case op < 9:
			from := cols[rnd.IntN(3)]
			if len(model[from]) == 0 {
				continue
			}
			i := rnd.IntN(len(model[from]))
			id := model[from][i]
			to := cols[rnd.IntN(3)]
			pos := rnd.IntN(len(model[to])+3) - 1
			c.Do("POST", "/api/v1/kanban/cards/"+id+"/move", map[string]any{"column_id": to, "position": pos}).Expect(200)
			model[from] = slices.Delete(model[from], i, i+1)
			if from == to {
				pos = min(max(pos, 0), len(model[to]))
			}
			model[to] = insert(model[to], pos, id)
		default:
			col := cols[rnd.IntN(3)]
			if len(model[col]) == 0 {
				continue
			}
			i := rnd.IntN(len(model[col]))
			c.Do("DELETE", "/api/v1/kanban/cards/"+model[col][i], nil).Expect(http.StatusNoContent)
			model[col] = slices.Delete(model[col], i, i+1)
		}
	}
	bd := v.getBoard(c, b.ID)
	assertDense(t, bd)
	for _, col := range cols {
		var want []string
		for _, id := range model[col] {
			want = append(want, titles[id])
		}
		if got := columnTitles(bd, col); !slices.Equal(got, want) {
			t.Fatalf("column %s:\n got %v\nwant %v", col, got, want)
		}
	}
}

func TestCardFieldsVersionsAndConflicts(t *testing.T) {
	v := newEnv(t)
	c := v.admin
	b := v.board(c, "Fields")
	col := b.Columns[0].ID
	var bug, ui Label
	c.Do("POST", "/api/v1/kanban/boards/"+b.ID+"/labels", map[string]any{"name": "Bug", "color": "red"}).Expect(201).JSON(&bug)
	c.Do("POST", "/api/v1/kanban/boards/"+b.ID+"/labels", map[string]any{"name": "UI", "color": "purple"}).Expect(201).JSON(&ui)
	c.Do("POST", "/api/v1/kanban/boards/"+b.ID+"/labels", map[string]any{"name": "bug", "color": "red"}).ExpectError(409, "conflict")
	c.Do("POST", "/api/v1/kanban/boards/"+b.ID+"/labels", map[string]any{"name": "x", "color": "beige"}).ExpectError(422, "validation_failed")

	card := v.card(c, col, "Fix login", map[string]any{
		"description": "Steps:\n1. open", "labels": []string{ui.ID, bug.ID}, "priority": "high",
		"start_at": "2026-11-01T08:00:00+02:00", "due_at": "2026-11-02T21:59:59.999Z", "all_day": true,
	})
	if card.Number != 1 || card.Version != 1 || card.Priority != "high" || *card.StartAt != "2026-11-01T06:00:00.000Z" || !card.AllDay {
		t.Fatalf("created %+v", card)
	}
	if !slices.Equal(card.Labels, []string{bug.ID, ui.ID}) { // by label name
		t.Fatalf("labels %v", card.Labels)
	}
	if n := v.card(c, col, "Second"); n.Number != 2 {
		t.Fatalf("number %d", n.Number)
	}
	// Validation.
	bad := []map[string]any{
		{"title": ""}, {"title": strings.Repeat("x", 501)}, {"priority": "asap"}, {"due_at": "tomorrow"},
		{"labels": []string{"nope"}}, {"description": strings.Repeat("y", 100_001)},
		{"start_at": "2026-11-03T00:00:00Z", "due_at": "2026-11-02T00:00:00Z"},
	}
	for _, body := range bad {
		if body["title"] == nil {
			body["title"] = "ok"
		}
		c.Do("POST", "/api/v1/kanban/columns/"+col+"/cards", body).ExpectError(422, "validation_failed")
	}

	// Patch with the right base version.
	var p Card
	c.Do("PATCH", "/api/v1/kanban/cards/"+card.ID, map[string]any{"title": "Fix login form", "base_version": 1}).Expect(200).JSON(&p)
	if p.Version != 2 || p.Title != "Fix login form" {
		t.Fatalf("patched %+v", p)
	}
	// A no-op patch keeps the version.
	c.Do("PATCH", "/api/v1/kanban/cards/"+card.ID, map[string]any{"title": "Fix login form", "labels": []string{ui.ID, bug.ID}, "base_version": 2}).Expect(200).JSON(&p)
	if p.Version != 2 {
		t.Fatalf("no-op bumped to %d", p.Version)
	}
	// Stale base version → 409 with the current card.
	r := c.Do("PATCH", "/api/v1/kanban/cards/"+card.ID, map[string]any{"description": "mine", "base_version": 1}).ExpectError(409, "conflict")
	cur, _ := errDetails(t, r)["current"].(map[string]any)
	if cur["id"] != card.ID || cur["version"] != float64(2) || cur["description"] != "Steps:\n1. open" {
		t.Fatalf("conflict details %v", cur)
	}
	// Completing sets completed_at; clearing dates with null.
	c.Do("PATCH", "/api/v1/kanban/cards/"+card.ID, map[string]any{"completed": true, "due_at": nil, "start_at": nil, "labels": nil}).Expect(200).JSON(&p)
	if !p.Completed || p.CompletedAt == nil || p.DueAt != nil || p.StartAt != nil || len(p.Labels) != 0 || p.Version != 3 {
		t.Fatalf("completed %+v", p)
	}
	c.Do("PATCH", "/api/v1/kanban/cards/"+card.ID, map[string]any{"completed": false}).Expect(200).JSON(&p)
	if p.Completed || p.CompletedAt != nil {
		t.Fatalf("reopened %+v", p)
	}
	// Deleting a label removes it from cards and moves their version on.
	c.Do("PATCH", "/api/v1/kanban/cards/"+card.ID, map[string]any{"labels": []string{bug.ID}}).Expect(200).JSON(&p)
	c.Do("DELETE", "/api/v1/kanban/labels/"+bug.ID, nil).Expect(http.StatusNoContent)
	var after Card
	c.Do("GET", "/api/v1/kanban/cards/"+card.ID, nil).Expect(200).JSON(&after)
	if len(after.Labels) != 0 || after.Version != p.Version+1 {
		t.Fatalf("after label delete %+v (was v%d)", after, p.Version)
	}
	// Label rename/recolor; rename onto an existing name conflicts.
	var l Label
	c.Do("PATCH", "/api/v1/kanban/labels/"+ui.ID, map[string]any{"name": "Frontend", "color": "indigo"}).Expect(200).JSON(&l)
	if l.Name != "Frontend" || l.Color != "indigo" {
		t.Fatalf("label %+v", l)
	}
	var other Label
	c.Do("POST", "/api/v1/kanban/boards/"+b.ID+"/labels", map[string]any{"name": "Backend", "color": "green"}).Expect(201).JSON(&other)
	c.Do("PATCH", "/api/v1/kanban/labels/"+other.ID, map[string]any{"name": "FRONTEND"}).ExpectError(409, "conflict")

	// Every write moved the board version on.
	before := v.getBoard(c, b.ID).Version
	c.Do("POST", "/api/v1/kanban/cards/"+card.ID+"/comments", map[string]any{"body": "noted"}).Expect(201)
	if got := v.getBoard(c, b.ID).Version; got != before+1 {
		t.Fatalf("comment did not bump the board version: %d → %d", before, got)
	}

	// Delete.
	c.Do("DELETE", "/api/v1/kanban/cards/"+card.ID, nil).Expect(http.StatusNoContent)
	c.Do("GET", "/api/v1/kanban/cards/"+card.ID, nil).ExpectError(404, "not_found")
	assertDense(t, v.getBoard(c, b.ID, "include_archived=1"))
}

func TestColumnsCRUDAndDelete(t *testing.T) {
	v := newEnv(t)
	c := v.admin
	b := v.board(c, "Columns", map[string]any{"template": "blank"})
	var backlog, review Column
	c.Do("POST", "/api/v1/kanban/boards/"+b.ID+"/columns", map[string]any{"name": "Backlog", "color": "gray", "wip_limit": 5}).Expect(201).JSON(&backlog)
	c.Do("POST", "/api/v1/kanban/boards/"+b.ID+"/columns", map[string]any{"name": "Review", "position": 0}).Expect(201).JSON(&review)
	if backlog.WIPLimit == nil || *backlog.WIPLimit != 5 || *backlog.Color != "gray" || review.Position != 0 {
		t.Fatalf("columns %+v %+v", backlog, review)
	}
	c.Do("POST", "/api/v1/kanban/boards/"+b.ID+"/columns", map[string]any{"name": "x", "wip_limit": -1}).ExpectError(422, "validation_failed")
	var col Column
	c.Do("PATCH", "/api/v1/kanban/columns/"+backlog.ID, map[string]any{"name": "Ideas", "wip_limit": nil, "color": nil, "collapsed": true}).Expect(200).JSON(&col)
	if col.Name != "Ideas" || col.WIPLimit != nil || col.Color != nil || !col.Collapsed || col.Position != 1 {
		t.Fatalf("patched %+v", col)
	}
	// WIP limits are advisory.
	c.Do("PATCH", "/api/v1/kanban/columns/"+review.ID, map[string]any{"wip_limit": 1}).Expect(200)
	v.card(c, review.ID, "r1")
	v.card(c, review.ID, "r2")
	arch := v.card(c, review.ID, "r3")
	c.Do("PATCH", "/api/v1/kanban/cards/"+arch.ID, map[string]any{"archived": true}).Expect(200)
	v.card(c, backlog.ID, "i1")

	// A column with cards (archived ones too) cannot simply be deleted.
	r := c.Do("DELETE", "/api/v1/kanban/columns/"+review.ID, nil).ExpectError(409, "conflict")
	if errDetails(t, r)["card_count"] != float64(3) {
		t.Fatalf("details %s", r.Body)
	}
	c.Do("DELETE", "/api/v1/kanban/columns/"+review.ID+"?move_cards_to="+review.ID, nil).ExpectError(422, "validation_failed")
	other := v.board(c, "Elsewhere")
	c.Do("DELETE", "/api/v1/kanban/columns/"+review.ID+"?move_cards_to="+other.Columns[0].ID, nil).ExpectError(422, "validation_failed")
	c.Do("DELETE", "/api/v1/kanban/columns/"+review.ID+"?move_cards_to="+backlog.ID, nil).Expect(http.StatusNoContent)
	bd := v.getBoard(c, b.ID, "include_archived=1")
	if len(bd.Columns) != 1 || bd.Columns[0].Position != 0 {
		t.Fatalf("columns after delete %+v", bd.Columns)
	}
	if got := columnTitles(bd, backlog.ID); !slices.Equal(got, []string{"i1", "r1", "r2", "r3"}) {
		t.Fatalf("moved cards %v", got)
	}
	assertDense(t, bd)
	empty := v.board(c, "Empty")
	c.Do("DELETE", "/api/v1/kanban/columns/"+empty.Columns[1].ID, nil).Expect(http.StatusNoContent)
	if eb := v.getBoard(c, empty.ID); len(eb.Columns) != 2 || eb.Columns[1].Name != "Done" || eb.Columns[1].Position != 1 {
		t.Fatalf("columns %+v", eb.Columns)
	}
}

func TestSubtasks(t *testing.T) {
	v := newEnv(t)
	c := v.admin
	bobID, _ := v.user("bob")
	b := v.board(c, "Checklists")
	v.members(c, b.ID, map[string]string{bobID: "editor"})
	col := b.Columns[0].ID
	above := v.card(c, col, "Above")
	card := v.card(c, col, "Release")
	below := v.card(c, col, "Below")
	add := func(title string, extra map[string]any) Subtask {
		t.Helper()
		body := map[string]any{"title": title}
		for k, x := range extra {
			body[k] = x
		}
		var s Subtask
		c.Do("POST", "/api/v1/kanban/cards/"+card.ID+"/subtasks", body).Expect(201).JSON(&s)
		return s
	}
	s1 := add("Changelog", nil)
	s2 := add("Tag", map[string]any{"due_at": "2026-12-01T10:00:00Z", "assignee_id": bobID})
	s0 := add("Freeze", map[string]any{"position": 0})
	if s0.Position != 0 || s2.Position != 1 || *s2.AssigneeID != bobID {
		t.Fatalf("subtasks %+v %+v", s0, s2)
	}
	c.Do("POST", "/api/v1/kanban/cards/"+card.ID+"/subtasks", map[string]any{"title": "x", "assignee_id": "nosuchuser0001"}).ExpectError(422, "validation_failed")

	var done Subtask
	c.Do("PATCH", "/api/v1/kanban/subtasks/"+s1.ID, map[string]any{"done": true}).Expect(200).JSON(&done)
	if !done.Done || done.CompletedAt == nil {
		t.Fatalf("done %+v", done)
	}
	var got Card
	c.Do("GET", "/api/v1/kanban/cards/"+card.ID, nil).Expect(200).JSON(&got)
	if got.SubtasksTotal != 3 || got.SubtasksDone != 1 || got.Version != card.Version {
		t.Fatalf("counters %+v (subtasks must not change the card version)", got)
	}
	var order []Subtask
	c.Do("POST", "/api/v1/kanban/subtasks/"+s0.ID+"/move", map[string]any{"position": 2}).Expect(200).JSON(&order)
	if len(order) != 3 || order[0].ID != s1.ID || order[1].ID != s2.ID || order[2].ID != s0.ID {
		t.Fatalf("order %+v", order)
	}

	// Promote: a card right below the parent, same column, title, due date and assignee.
	var promoted Card
	c.Do("POST", "/api/v1/kanban/subtasks/"+s2.ID+"/promote", nil).Expect(201).JSON(&promoted)
	if promoted.Title != "Tag" || promoted.ColumnID != col || promoted.Position != card.Position+1 ||
		promoted.DueAt == nil || *promoted.DueAt != "2026-12-01T10:00:00.000Z" || !slices.Equal(promoted.Assignees, []string{bobID}) {
		t.Fatalf("promoted %+v", promoted)
	}
	bd := v.getBoard(c, b.ID)
	if titles := columnTitles(bd, col); !slices.Equal(titles, []string{above.Title, "Release", "Tag", below.Title}) {
		t.Fatalf("after promote %v", titles)
	}
	var d CardDetail
	c.Do("GET", "/api/v1/kanban/cards/"+card.ID, nil).Expect(200).JSON(&d)
	if len(d.Subtasks) != 2 || d.Subtasks[0].Position != 0 || d.Subtasks[1].Position != 1 {
		t.Fatalf("remaining %+v", d.Subtasks)
	}
	c.Do("DELETE", "/api/v1/kanban/subtasks/"+s1.ID, nil).Expect(http.StatusNoContent)
	c.Do("GET", "/api/v1/kanban/cards/"+card.ID, nil).Expect(200).JSON(&d)
	if len(d.Subtasks) != 1 || d.Subtasks[0].ID != s0.ID || d.Subtasks[0].Position != 0 {
		t.Fatalf("after delete %+v", d.Subtasks)
	}
}

func TestComments(t *testing.T) {
	v := newEnv(t)
	owner := v.admin
	aliceID, alice := v.user("alice")
	bobID, bob := v.user("bob")
	b := v.board(owner, "Talk")
	v.members(owner, b.ID, map[string]string{aliceID: "viewer", bobID: "admin"})
	card := v.card(owner, b.Columns[0].ID, "Discuss")
	var cm Comment
	alice.Do("POST", "/api/v1/kanban/cards/"+card.ID+"/comments", map[string]any{"body": "**Viewers** can comment"}).Expect(201).JSON(&cm)
	if cm.AuthorID != aliceID || cm.AuthorName != "User alice" || cm.Edited {
		t.Fatalf("comment %+v", cm)
	}
	alice.Do("POST", "/api/v1/kanban/cards/"+card.ID+"/comments", map[string]any{"body": "  "}).ExpectError(422, "validation_failed")
	alice.Do("POST", "/api/v1/kanban/cards/"+card.ID+"/comments", map[string]any{"body": strings.Repeat("z", 20_001)}).ExpectError(422, "validation_failed")
	owner.Do("PATCH", "/api/v1/kanban/comments/"+cm.ID, map[string]any{"body": "hijack"}).ExpectError(403, "forbidden")
	var edited Comment
	alice.Do("PATCH", "/api/v1/kanban/comments/"+cm.ID, map[string]any{"body": "edited"}).Expect(200).JSON(&edited)
	if !edited.Edited || edited.Body != "edited" {
		t.Fatalf("edited %+v", edited)
	}
	for i := 0; i < 4; i++ {
		owner.Do("POST", "/api/v1/kanban/cards/"+card.ID+"/comments", map[string]any{"body": fmt.Sprintf("c%d", i)}).Expect(201)
	}
	var p1, p2 struct {
		Items      []Comment `json:"items"`
		NextCursor *string   `json:"next_cursor"`
	}
	owner.Do("GET", "/api/v1/kanban/cards/"+card.ID+"/comments?limit=3", nil).Expect(200).JSON(&p1)
	owner.Do("GET", "/api/v1/kanban/cards/"+card.ID+"/comments?limit=3&cursor="+*p1.NextCursor, nil).Expect(200).JSON(&p2)
	if len(p1.Items) != 3 || p1.Items[0].ID != cm.ID || len(p2.Items) != 2 || p2.Items[1].Body != "c3" || p2.NextCursor != nil {
		t.Fatalf("pages %+v %+v", p1, p2)
	}
	// Authors delete their own comments; board admins delete anyone's; others cannot.
	alice.Do("DELETE", "/api/v1/kanban/comments/"+p2.Items[0].ID, nil).ExpectError(403, "forbidden")
	bob.Do("DELETE", "/api/v1/kanban/comments/"+p2.Items[0].ID, nil).Expect(http.StatusNoContent)
	alice.Do("DELETE", "/api/v1/kanban/comments/"+cm.ID, nil).Expect(http.StatusNoContent)
	var got Card
	owner.Do("GET", "/api/v1/kanban/cards/"+card.ID, nil).Expect(200).JSON(&got)
	if got.CommentCount != 3 {
		t.Fatalf("comment_count %d", got.CommentCount)
	}
}

func TestCrossBoardIsolation(t *testing.T) {
	v := newEnv(t)
	c := v.admin
	_, other := v.user("other")
	mine := v.board(c, "Mine")
	theirs := v.board(other, "Theirs")
	var theirLabel Label
	other.Do("POST", "/api/v1/kanban/boards/"+theirs.ID+"/labels", map[string]any{"name": "L", "color": "red"}).Expect(201).JSON(&theirLabel)
	theirCard := v.card(other, theirs.Columns[0].ID, "Their card")
	var theirSub Subtask
	other.Do("POST", "/api/v1/kanban/cards/"+theirCard.ID+"/subtasks", map[string]any{"title": "s"}).Expect(201).JSON(&theirSub)
	var theirComment Comment
	other.Do("POST", "/api/v1/kanban/cards/"+theirCard.ID+"/comments", map[string]any{"body": "b"}).Expect(201).JSON(&theirComment)
	var theirAtt Attachment
	v.upload(other, theirCard.ID, "a.txt", []byte("secret")).expect(t, 201).decode(t, &theirAtt)

	myCard := v.card(c, mine.Columns[0].ID, "My card")
	// Ids of another board in my requests → 422.
	c.Do("POST", "/api/v1/kanban/columns/"+mine.Columns[0].ID+"/cards", map[string]any{"title": "x", "labels": []string{theirLabel.ID}}).ExpectError(422, "validation_failed")
	c.Do("POST", "/api/v1/kanban/cards/"+myCard.ID+"/move", map[string]any{"column_id": theirs.Columns[1].ID}).ExpectError(422, "validation_failed")
	c.Do("PATCH", "/api/v1/kanban/cards/"+myCard.ID, map[string]any{"cover_attachment_id": theirAtt.ID}).ExpectError(422, "validation_failed")
	c.Do("PATCH", "/api/v1/kanban/cards/"+myCard.ID, map[string]any{"assignees": []string{theirs.OwnerID}}).ExpectError(422, "validation_failed")
	// Objects of another board → 404, never 403.
	for _, rq := range []struct{ m, p string }{
		{"GET", "/api/v1/kanban/cards/" + theirCard.ID}, {"PATCH", "/api/v1/kanban/cards/" + theirCard.ID},
		{"DELETE", "/api/v1/kanban/cards/" + theirCard.ID}, {"POST", "/api/v1/kanban/cards/" + theirCard.ID + "/move"},
		{"GET", "/api/v1/kanban/cards/" + theirCard.ID + "/comments"}, {"POST", "/api/v1/kanban/cards/" + theirCard.ID + "/comments"},
		{"PATCH", "/api/v1/kanban/subtasks/" + theirSub.ID}, {"POST", "/api/v1/kanban/subtasks/" + theirSub.ID + "/promote"},
		{"PATCH", "/api/v1/kanban/comments/" + theirComment.ID}, {"DELETE", "/api/v1/kanban/comments/" + theirComment.ID},
		{"PATCH", "/api/v1/kanban/labels/" + theirLabel.ID}, {"DELETE", "/api/v1/kanban/labels/" + theirLabel.ID},
		{"PATCH", "/api/v1/kanban/columns/" + theirs.Columns[0].ID}, {"POST", "/api/v1/kanban/columns/" + theirs.Columns[0].ID + "/cards"},
		{"GET", "/api/v1/kanban/attachments/" + theirAtt.ID}, {"GET", "/api/v1/kanban/attachments/" + theirAtt.ID + "/thumbnail"},
		{"DELETE", "/api/v1/kanban/attachments/" + theirAtt.ID}, {"POST", "/api/v1/kanban/attachments/" + theirAtt.ID + "/save-to-files"},
		{"POST", "/api/v1/kanban/boards/" + theirs.ID + "/leave"}, {"PUT", "/api/v1/kanban/boards/" + theirs.ID + "/members"},
	} {
		body := map[string]any{"title": "t", "body": "b", "name": "n", "column_id": mine.Columns[0].ID, "members": []any{}}
		if r := c.Do(rq.m, rq.p, body); r.Status != http.StatusNotFound {
			t.Errorf("%s %s: %d %s", rq.m, rq.p, r.Status, r.Body)
		}
	}
	var page struct{ Items []CardListItem }
	c.Do("GET", "/api/v1/kanban/cards", nil).Expect(200).JSON(&page)
	if len(page.Items) != 1 || page.Items[0].ID != myCard.ID {
		t.Fatalf("cards list leaks %+v", page.Items)
	}
	c.Do("GET", "/api/v1/kanban/cards?board_id="+theirs.ID, nil).ExpectError(404, "not_found")
}

func TestCardListFilters(t *testing.T) {
	v := newEnv(t)
	c := v.admin
	meID := v.getBoard(c, v.board(c, "Probe").ID).OwnerID
	bobID, bob := v.user("bob")
	b1 := v.board(c, "Alpha board")
	b2 := v.board(c, "Beta board")
	v.members(c, b2.ID, map[string]string{bobID: "editor"})
	var lbl Label
	c.Do("POST", "/api/v1/kanban/boards/"+b1.ID+"/labels", map[string]any{"name": "Ops", "color": "teal"}).Expect(201).JSON(&lbl)
	tnow := time.Now().UTC()
	at := func(d time.Duration) string { return tnow.Add(d).Format(time.RFC3339) }
	overdue := v.card(c, b1.Columns[0].ID, "Overdue invoice", map[string]any{"due_at": at(-48 * time.Hour), "assignees": []string{meID}, "labels": []string{lbl.ID}})
	today := v.card(c, b1.Columns[1].ID, "Call today", map[string]any{"due_at": at(3 * time.Hour), "assignees": []string{meID}})
	week := v.card(c, b2.Columns[0].ID, "Weekly report", map[string]any{"due_at": at(72 * time.Hour), "assignees": []string{bobID}})
	later := v.card(c, b2.Columns[0].ID, "Later", map[string]any{"due_at": at(30 * 24 * time.Hour), "description": "mentions invoice"})
	nodue := v.card(c, b2.Columns[2].ID, "No due date")
	doneOverdue := v.card(c, b1.Columns[2].ID, "Done but late", map[string]any{"due_at": at(-24 * time.Hour)})
	c.Do("PATCH", "/api/v1/kanban/cards/"+doneOverdue.ID, map[string]any{"completed": true}).Expect(200)
	archived := v.card(c, b1.Columns[0].ID, "Archived", map[string]any{"due_at": at(-time.Hour)})
	c.Do("PATCH", "/api/v1/kanban/cards/"+archived.ID, map[string]any{"archived": true}).Expect(200)

	ids := func(c *authtest.Client, query string) []string {
		t.Helper()
		var p struct {
			Items      []CardListItem `json:"items"`
			NextCursor *string        `json:"next_cursor"`
		}
		c.Do("GET", "/api/v1/kanban/cards"+query, nil).Expect(200).JSON(&p)
		var out []string
		for _, it := range p.Items {
			out = append(out, it.ID)
		}
		return out
	}
	// Default order: due date (nulls last), then board/column/position.
	if got := ids(c, ""); !slices.Equal(got, []string{overdue.ID, doneOverdue.ID, today.ID, week.ID, later.ID, nodue.ID}) {
		t.Fatalf("all %v", got)
	}
	cases := map[string][]string{
		"?due=overdue":             {overdue.ID},
		"?due=today":               {today.ID},
		"?due=week":                {today.ID, week.ID},
		"?due=none":                {nodue.ID},
		"?due=any&completed=false": {overdue.ID, today.ID, week.ID, later.ID},
		"?completed=true":          {doneOverdue.ID},
		"?assignee=me":             {overdue.ID, today.ID},
		"?assignee=" + bobID:       {week.ID},
		"?label=" + lbl.ID:         {overdue.ID},
		"?q=INVOICE":               {overdue.ID, later.ID},
		"?board_id=" + b1.ID + "&q=%23" + itoa(today.Number): {today.ID},
		"?board_id=" + b2.ID: {week.ID, later.ID, nodue.ID},
		"?archived=true":     {archived.ID},
	}
	for q, want := range cases {
		if got := ids(c, q); !slices.Equal(got, want) {
			t.Errorf("%s: got %v want %v", q, got, want)
		}
	}
	if got := ids(bob, "?assignee=me"); !slices.Equal(got, []string{week.ID}) {
		t.Fatalf("bob's cards %v", got)
	}
	c.Do("GET", "/api/v1/kanban/cards?due=soon", nil).ExpectError(422, "validation_failed")
	var item struct{ Items []CardListItem }
	c.Do("GET", "/api/v1/kanban/cards?due=overdue", nil).Expect(200).JSON(&item)
	if item.Items[0].BoardName != "Alpha board" || item.Items[0].ColumnName != "To do" {
		t.Fatalf("list item %+v", item.Items[0])
	}
	// Cards of archived boards only show when asked for by board.
	c.Do("PATCH", "/api/v1/kanban/boards/"+b2.ID, map[string]any{"archived": true}).Expect(200)
	if got := ids(c, "?due=week"); !slices.Equal(got, []string{today.ID}) {
		t.Fatalf("archived board leaked %v", got)
	}
	if got := ids(c, "?board_id="+b2.ID+"&due=week"); !slices.Equal(got, []string{week.ID}) {
		t.Fatalf("archived board by id %v", got)
	}
}

func TestCardListPagination(t *testing.T) {
	v := newEnv(t)
	c := v.admin
	b := v.board(c, "Many")
	base := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	var want []string
	for i := 0; i < 23; i++ {
		extra := map[string]any{}
		if i%3 != 0 { // a third without due date, at the end
			extra["due_at"] = base.Add(time.Duration(i%5) * time.Hour).Format(time.RFC3339)
		}
		v.card(c, b.Columns[i%3].ID, fmt.Sprintf("card %02d", i), extra)
	}
	var all struct{ Items []CardListItem }
	c.Do("GET", "/api/v1/kanban/cards?limit=200", nil).Expect(200).JSON(&all)
	for _, it := range all.Items {
		want = append(want, it.ID)
	}
	var got []string
	cursor := ""
	for pages := 0; ; pages++ {
		var p struct {
			Items      []CardListItem `json:"items"`
			NextCursor *string        `json:"next_cursor"`
		}
		c.Do("GET", "/api/v1/kanban/cards?limit=4"+cursor, nil).Expect(200).JSON(&p)
		for _, it := range p.Items {
			got = append(got, it.ID)
		}
		if p.NextCursor == nil {
			break
		}
		if pages > 10 {
			t.Fatal("pagination does not end")
		}
		cursor = "&cursor=" + *p.NextCursor
	}
	if len(want) != 23 || !slices.Equal(got, want) {
		t.Fatalf("paged %d items, want %d (same order)", len(got), len(want))
	}
	var p struct {
		NextCursor *string `json:"next_cursor"`
	}
	c.Do("GET", "/api/v1/kanban/cards?limit=4", nil).Expect(200).JSON(&p)
	c.Do("GET", "/api/v1/kanban/cards?limit=4&due=none&cursor="+*p.NextCursor, nil).ExpectError(400, "bad_request")
	c.Do("GET", "/api/v1/kanban/cards?cursor=forged", nil).ExpectError(400, "bad_request")
}

// TestLimits fills tables directly up to the limits and checks the next write via HTTP.
func TestLimits(t *testing.T) {
	v := newEnv(t)
	c := v.admin
	b := v.board(c, "Full")
	col := b.Columns[0].ID
	card := v.card(c, col, "Card")
	sqlDB := v.e.Deps.DB.SQL
	ts := db.FormatTime(core.Now())
	fill := func(n int, query string, args func(i int) []any) {
		t.Helper()
		tx, err := sqlDB.Begin()
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < n; i++ {
			if _, err := tx.Exec(query, args(i)...); err != nil {
				t.Fatal(err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	fill(MaxLabels, `INSERT INTO kanban_labels (id, board_id, name, name_key, color, created_at) VALUES (?, ?, ?, ?, 'red', ?)`,
		func(i int) []any { return []any{core.NewID(), b.ID, fmt.Sprint("l", i), fmt.Sprint("l", i), ts} })
	c.Do("POST", "/api/v1/kanban/boards/"+b.ID+"/labels", map[string]any{"name": "one more", "color": "red"}).ExpectError(409, "conflict")
	fill(MaxColumns-3, `INSERT INTO kanban_columns (id, board_id, name, position, created_at, updated_at) VALUES (?, ?, 'c', ?, ?, ?)`,
		func(i int) []any { return []any{core.NewID(), b.ID, 3 + i, ts, ts} })
	c.Do("POST", "/api/v1/kanban/boards/"+b.ID+"/columns", map[string]any{"name": "one more"}).ExpectError(409, "conflict")
	fill(MaxSubtasks, `INSERT INTO kanban_subtasks (id, card_id, title, position, created_at, updated_at) VALUES (?, ?, 's', ?, ?, ?)`,
		func(i int) []any { return []any{core.NewID(), card.ID, i, ts, ts} })
	c.Do("POST", "/api/v1/kanban/cards/"+card.ID+"/subtasks", map[string]any{"title": "one more"}).ExpectError(409, "conflict")
	fill(MaxComments, `INSERT INTO kanban_comments (id, card_id, author_id, body, created_at, updated_at) VALUES (?, ?, ?, 'c', ?, ?)`,
		func(i int) []any { return []any{core.NewID(), card.ID, b.OwnerID, ts, ts} })
	c.Do("POST", "/api/v1/kanban/cards/"+card.ID+"/comments", map[string]any{"body": "one more"}).ExpectError(409, "conflict")
	fill(MaxCards-1, `INSERT INTO kanban_cards (id, board_id, column_id, number, title, position, created_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'x', ?, ?, ?, ?)`,
		func(i int) []any { return []any{core.NewID(), b.ID, b.Columns[1].ID, 1000 + i, i, b.OwnerID, ts, ts} })
	c.Do("POST", "/api/v1/kanban/columns/"+col+"/cards", map[string]any{"title": "one more"}).ExpectError(409, "conflict")
	fill(MaxBoardsPerUser-1, `INSERT INTO kanban_boards (id, owner_id, name, color, created_at, updated_at) VALUES (?, ?, 'b', 'blue', ?, ?)`,
		func(i int) []any { return []any{core.NewID(), b.OwnerID, ts, ts} })
	c.Do("POST", "/api/v1/kanban/boards", map[string]any{"name": "one more"}).ExpectError(409, "conflict")
	// A card read with 10 000 siblings stays fast enough and the board payload cuts descriptions.
	long := strings.Repeat("d", 5000)
	c.Do("PATCH", "/api/v1/kanban/cards/"+card.ID, map[string]any{"description": long}).Expect(200)
	bd := v.getBoard(c, b.ID)
	for _, x := range bd.Cards {
		if x.ID == card.ID && len(x.Description) != listDescriptionLen {
			t.Fatalf("board description length %d", len(x.Description))
		}
	}
	var full Card
	c.Do("GET", "/api/v1/kanban/cards/"+card.ID, nil).Expect(200).JSON(&full)
	if len(full.Description) != 5000 {
		t.Fatalf("card description length %d", len(full.Description))
	}
	// Members: at most 100 including the owner.
	var many []map[string]any
	for i := 0; i < MaxMembers; i++ {
		many = append(many, map[string]any{"user_id": core.NewID(), "role": "viewer"})
	}
	c.Do("PUT", "/api/v1/kanban/boards/"+b.ID+"/members", map[string]any{"members": many}).ExpectError(422, "validation_failed")
}
