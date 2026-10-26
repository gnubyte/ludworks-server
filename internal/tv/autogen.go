package tv

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"

	"ludworks/internal/core"
	"ludworks/internal/db"
	"ludworks/internal/httpx"
)

type autoGenInput struct {
	Strategy string `json:"strategy"`
	Shuffle  *bool  `json:"shuffle"`
	MinItems *int   `json:"min_items"`
}

// autoPlan is one channel auto-generate wants to create.
type autoPlan struct {
	name, group, lineupName string
	item                    LineupItem
}

// autoCandidates finds the shows/genres/categories of strategy with at least min playable
// files in the owner's implicitly readable spaces.
func (m *Module) autoCandidates(ctx context.Context, p *core.Principal, strategy string, minItems int) ([]autoPlan, error) {
	ready, err := m.mediaReady(ctx)
	if err != nil || !ready {
		return nil, err
	}
	_, implicit, err := m.spaceSets(ctx, p)
	if err != nil || len(implicit) == 0 {
		return nil, err
	}
	sp, _ := json.Marshal(implicit)
	base := ` FROM media_items m JOIN files f ON f.id = m.file_id WHERE f.is_dir = 0 AND f.trashed_at IS NULL
		AND f.space_id IN (SELECT value FROM json_each(?)) AND m.duration_seconds > 0 AND m.duration_seconds <= 1e7`
	q := m.d.DB.SQL
	var plans []autoPlan
	switch strategy {
	case "per_show":
		rs, err := q.QueryContext(ctx, `SELECT min(m.show_title), count(*)`+base+` AND m.show_title <> ''
			GROUP BY lower(m.show_title) HAVING count(*) >= ? ORDER BY min(m.show_title) COLLATE NOCASE LIMIT ?`,
			string(sp), minItems, MaxAutoGenerate)
		if err != nil {
			return nil, err
		}
		defer rs.Close()
		for rs.Next() {
			var title string
			var n int
			if err := rs.Scan(&title, &n); err != nil {
				return nil, err
			}
			plans = append(plans, autoPlan{name: title, group: "Shows", lineupName: title,
				item: LineupItem{Kind: KindShow, ShowTitle: title, Order: OrderDefault}})
		}
		return plans, rs.Err()
	case "per_genre":
		rs, err := q.QueryContext(ctx, `SELECT min(g.value), count(*)`+strings.Replace(base, "JOIN files f ON f.id = m.file_id",
			"JOIN files f ON f.id = m.file_id JOIN json_each(CASE WHEN json_valid(m.genres) THEN m.genres ELSE '[]' END) g", 1)+
			` AND f.category = 'movie' AND trim(g.value) <> '' GROUP BY lower(g.value) HAVING count(*) >= ?
			ORDER BY min(g.value) COLLATE NOCASE LIMIT ?`, string(sp), minItems, MaxAutoGenerate)
		if err != nil {
			return nil, err
		}
		defer rs.Close()
		for rs.Next() {
			var genre string
			var n int
			if err := rs.Scan(&genre, &n); err != nil {
				return nil, err
			}
			if len([]rune(genre)) > queryFieldMax {
				continue
			}
			plans = append(plans, autoPlan{name: genre + " Movies", group: "Movies", lineupName: genre + " Movies",
				item: LineupItem{Kind: KindQuery, Query: &LineupQuery{Category: "movie", Genre: genre}, Order: OrderDefault}})
		}
		return plans, rs.Err()
	case "movies", "all_tv":
		cat, name, group := "movie", "Movies", "Movies"
		if strategy == "all_tv" {
			cat, name, group = "tv", "All TV", "Shows"
		}
		var n int
		if err := q.QueryRowContext(ctx, `SELECT count(*)`+base+` AND f.category = ?`, string(sp), cat).Scan(&n); err != nil {
			return nil, err
		}
		if n >= minItems {
			plans = append(plans, autoPlan{name: name, group: group, lineupName: name,
				item: LineupItem{Kind: KindQuery, Query: &LineupQuery{Category: cat}, Order: OrderDefault}})
		}
		return plans, nil
	}
	return nil, core.Validation(map[string]string{"strategy": "must be per_show, per_genre, movies or all_tv"})
}

// autoGenerate creates one lineup + channel per show/genre/category (API.md §7). Names that
// already exist among the caller's channels are skipped, so repeating the call only adds new
// shows or genres.
func (m *Module) autoGenerate(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	p := core.MustPrincipal(ctx)
	var in autoGenInput
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	errs := fieldErrs{}
	switch in.Strategy {
	case "per_show", "per_genre", "movies", "all_tv":
	default:
		errs.set("strategy", "must be per_show, per_genre, movies or all_tv")
	}
	minItems := 1
	if in.MinItems != nil {
		if *in.MinItems < 1 || *in.MinItems > MaxExpandedFiles {
			errs.set("min_items", "must be 1 or more")
		}
		minItems = *in.MinItems
	}
	shuffle := in.Shuffle != nil && *in.Shuffle
	if err := errs.err(); err != nil {
		return err
	}
	owner, err := m.ownerPrincipal(ctx, p.UserID)
	if err != nil {
		return err
	}
	plans, err := m.autoCandidates(ctx, owner, in.Strategy, minItems)
	if err != nil {
		return err
	}
	var created []string
	err = m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		created = nil
		existing := map[string]bool{}
		rs, err := tx.QueryContext(ctx, `SELECT lower(name) FROM tv_channels WHERE owner_id = ?`, p.UserID)
		if err != nil {
			return err
		}
		for rs.Next() {
			var n string
			if err := rs.Scan(&n); err != nil {
				rs.Close()
				return err
			}
			existing[n] = true
		}
		rs.Close()
		if err := rs.Err(); err != nil {
			return err
		}
		nLineups, err := countRows(ctx, tx, `SELECT count(*) FROM tv_lineups WHERE owner_id = ?`, p.UserID)
		if err != nil {
			return err
		}
		for _, pl := range plans {
			name := truncateRunes(pl.name, nameMax)
			if existing[strings.ToLower(name)] {
				continue
			}
			if nLineups >= MaxLineupsPerUser {
				break
			}
			now := m.now()
			lid := core.NewID()
			if _, err := tx.ExecContext(ctx, `INSERT INTO tv_lineups (id, owner_id, name, description, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?)`, lid, p.UserID, truncateRunes(pl.lineupName, nameMax), "Created by auto-generate",
				db.FormatTime(now), db.FormatTime(now)); err != nil {
				return err
			}
			nLineups++
			it := pl.item
			it.ID = core.NewID()
			if err := writeItems(ctx, tx, lid, []LineupItem{it}); err != nil {
				return err
			}
			c := &channelRow{ID: core.NewID(), OwnerID: p.UserID, Name: name, Group: pl.group, LineupID: lid,
				Shuffle: shuffle, ShuffleMode: "per_cycle", Seed: newSeed(), PlaybackMode: PlaybackLive,
				Visibility: VisibilityPrivate, AnchorAt: now, CreatedAt: now, UpdatedAt: now}
			if err := m.insertChannel(ctx, tx, c); err != nil {
				return err
			}
			existing[strings.ToLower(name)] = true
			created = append(created, c.ID)
		}
		return nil
	})
	if err != nil {
		return err
	}
	out := struct {
		Created []Channel `json:"created"`
	}{Created: []Channel{}}
	for _, id := range created {
		c, err := loadChannel(ctx, m.d.DB.SQL, id)
		if err != nil {
			return err
		}
		if c != nil {
			out.Created = append(out.Created, c.api())
		}
	}
	return httpx.WriteJSON(w, http.StatusOK, out)
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
