# Kanban boards

**Kanban** organises work as cards on boards: columns from left to right ("To do", "In progress", "Done", or
whatever your process needs) and cards that move across them. Boards can be shared with other people on the
server, and everybody sees changes within a few seconds.

## Boards

The **Boards** page lists the boards you belong to with their color, open and overdue cards and members.

* **New board** — give it a name, an optional description and a color, and pick a template: **Basic** starts
  with the columns "To do", "In progress" and "Done", **Blank** with none.
* From a board's **⋯** menu you can rename it, change its color, archive or restore it, leave it, or (as its
  owner) delete it. Archived boards move to the **Archived** tab; their cards no longer show up in *My cards*.
* **My cards** collects every card assigned to you across all boards, grouped into *Overdue*, *Today*,
  *This week*, *Later* and *No due date*. Tick a card to complete it, or click it to open it on its board.

## Working on a board

* **Columns** — add one with **Add column** at the right end. A column's **⋯** menu renames it, changes its
  color, sets a **WIP limit**, moves it left or right, collapses it to a narrow strip, or deletes it. When a
  column still holds cards, deleting it asks which column receives them. A WIP limit is advisory: the counter
  turns amber at the limit and red above it, but you can always add more cards.
* **Cards** — **Add a card** at the bottom of a column (or press **N**) opens a quick composer: type a title and
  press Enter; it stays open for the next one. Click a card to open it.
* **Drag and drop** — drag cards within a column or to another one, and drag columns by their header. On a
  phone or tablet, press and hold a card for a moment, then drag. The board scrolls when you drag near its edges.
  Press **Esc** to cancel a drag.
* **Without a mouse** — focus a card with Tab and use the arrow keys to move between cards. **Alt + ↑/↓** moves
  the focused card within its column, **Alt + ←/→** to the neighbouring column, and **Enter** opens it. The card
  menu (**⋯**, a right-click, or **Shift + F10**) has *Move to…*, *Move up/down/left/right*, *Mark as done*,
  *Archive* and *Delete*.
* **Tiles** show the card's labels, title, cover image, due date (red when overdue, amber when due today, green
  when done), priority, subtask progress, comments, attachments and assignees.
* **Filters** — the search box in the top bar filters by title, description or card number (`#12`). Next to the
  board name you can filter by labels, assignee (me, unassigned or a member) and due date (overdue, today, this
  week, no date), and show archived cards. Filters are part of the address, so a filtered board can be
  bookmarked or shared.

## Cards

A card opens over its board (full screen on a phone) and has its own address — copy the link from the card's
menu to point someone straight at it.

* **Title and description** — click the title to edit it. The description is Markdown (headings, lists, task
  lists, tables, code blocks with highlighting); **Edit** opens the editor, **Ctrl + Enter** (**⌘ + Enter** on a
  Mac) saves. Task-list boxes can be ticked right in the description.
* **Labels** — pick labels from the board's set, or type a new name to create one on the spot. Labels can be
  renamed, recolored and deleted (deleting removes the label from every card).
* **Assignees, priority and dates** — assign board members, set a priority (urgent, high, medium, low) and a
  start and due date. **All day** dates are whole days in your time zone; otherwise pick a time. Tick the box
  next to the title to mark the card as done.
* **Subtasks** — a checklist with a progress bar. Each subtask can have its own due date and assignee; drag the
  handle (or **Alt + ↑/↓** on it) to reorder, and **Turn into card** promotes a subtask to a card placed right
  below the current one.
* **Attachments** — drop files onto the card, use **Upload**, or **From Files** to copy a file from your Files
  (up to 50 MiB each). Images get previews and open in a viewer; any image can become the card's **cover**.
  **Save to Files** copies an attachment into your Files (your home folder by default).
* **Comments** — Markdown comments with a preview. You can edit and delete your own; board admins can delete any.
* **Activity** — every change to the card, newest first. The board's full activity log is under the
  **Activity** button in the top bar.
* **Archive** keeps a card but hides it from the board (show archived cards with the **Archived** switch);
  **Delete** removes it for good, with its subtasks, comments and attachments.

If two people edit the same card at the same time, Ludworks notices: when you save a title or description that
someone else changed meanwhile, it shows their version and lets you keep theirs, save yours, or continue editing.

## Sharing and roles

Open **Board settings** (the gear in the top bar, or click the member avatars) to manage the board.

| Role | Can |
|---|---|
| **Viewer** | read the board and comment |
| **Editor** | also change columns, cards, labels, subtasks and attachments |
| **Admin** | also rename and archive the board and manage its members |
| **Owner** | also delete the board or transfer it to another member (exactly one per board) |

Add people by searching for their name, choose their role, and change or remove members at any time — a
removed member is unassigned from the board's cards. Anyone but the owner can **leave** a board; the owner
**transfers** it to another member first (and stays on as an admin). Boards are private to their members:
other accounts, server administrators included, cannot see them.

## Limits

A user can own up to 500 boards. A board holds up to 100 members, 100 columns, 200 labels and 10 000 cards
(archived cards included), and 2 GiB of attachments. A card holds up to 200 subtasks, 5000 comments and 100
attachments of up to 50 MiB each. Titles are limited to 500 characters, descriptions to 100 000 and comments to
20 000.

## Keyboard shortcuts

| Key | Action |
|---|---|
| **N** | New card (on the boards page: new board) |
| **/** | Filter cards |
| **Shift + A** | Show or hide archived cards |
| **↑ ↓ ← →** | Move between cards |
| **Alt + ↑ ↓ ← →** | Move the focused card |
| **Enter** | Open the focused card |
| **G**, then **B** | Back to all boards |
| **Ctrl + K** / **⌘ + K** | Command palette (boards, new card, board settings, activity…) |

## API

Everything in the app is available over the [HTTP API](../api/index.md) (scopes `kanban:read` and
`kanban:write` for personal access tokens): boards, columns, cards, labels, subtasks, comments, attachments and
the activity log. Clients that keep a board open poll `GET /api/v1/kanban/boards/{id}` with `If-None-Match`; an
unchanged board answers `304 Not Modified` without a body. See the [endpoint reference](../api/reference.md).
