package files

import (
	"context"
	"database/sql"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"ludworks/internal/core"
	"ludworks/internal/db"
	"ludworks/internal/httpx"
	"ludworks/internal/settings"
)

// Subcategory is a second-level category.
type Subcategory struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// Category is a node of the file taxonomy (API.md §5).
type Category struct {
	Slug          string        `json:"slug"`
	Name          string        `json:"name"`
	Icon          string        `json:"icon"`
	Subcategories []Subcategory `json:"subcategories"`
}

var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// taxonomyCache holds the categories table in memory (invalidated on PUT).
type taxonomyCache struct {
	mu   sync.RWMutex
	cats []Category
}

func (s *Service) taxCache() *taxonomyCache { return &s.tax }

// Categories returns the taxonomy in display order.
func (s *Service) Categories(ctx context.Context) ([]Category, error) {
	tc := s.taxCache()
	tc.mu.RLock()
	cats := tc.cats
	tc.mu.RUnlock()
	if cats != nil {
		return cats, nil
	}
	rs, err := s.d.DB.SQL.QueryContext(ctx, `SELECT slug, name, icon, subcategories FROM categories ORDER BY position, slug`)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	cats = []Category{}
	for rs.Next() {
		var c Category
		var subs db.JSON[[]Subcategory]
		if err := rs.Scan(&c.Slug, &c.Name, &c.Icon, &subs); err != nil {
			return nil, err
		}
		c.Subcategories = subs.V
		if c.Subcategories == nil {
			c.Subcategories = []Subcategory{}
		}
		cats = append(cats, c)
	}
	if err := rs.Err(); err != nil {
		return nil, err
	}
	tc.mu.Lock()
	tc.cats = cats
	tc.mu.Unlock()
	return cats, nil
}

// checkCategory validates a category/subcategory pair against the taxonomy ("" = none).
// Values already stored on files keep working after the taxonomy changes; only new
// assignments are validated.
func (s *Service) checkCategory(ctx context.Context, cat, sub string) error {
	if cat == "" {
		if sub != "" {
			return core.Validation(map[string]string{"subcategory": "subcategory requires a category"})
		}
		return nil
	}
	cats, err := s.Categories(ctx)
	if err != nil {
		return err
	}
	for _, c := range cats {
		if c.Slug != cat {
			continue
		}
		if sub == "" {
			return nil
		}
		for _, sc := range c.Subcategories {
			if sc.Slug == sub {
				return nil
			}
		}
		return core.Validation(map[string]string{"subcategory": "unknown subcategory for " + cat})
	}
	return core.Validation(map[string]string{"category": "unknown category"})
}

// validateTaxonomy checks a replacement taxonomy.
func validateTaxonomy(cats []Category) error {
	fe := map[string]string{}
	if len(cats) > 100 {
		fe["categories"] = "at most 100 categories"
	}
	seen := map[string]bool{}
	for i, c := range cats {
		key := "categories[" + strconv.Itoa(i) + "]"
		c.Name = strings.TrimSpace(c.Name)
		switch {
		case !slugRe.MatchString(c.Slug):
			fe[key+".slug"] = "slug must be 1-64 lowercase letters, digits, - or _"
		case seen[c.Slug]:
			fe[key+".slug"] = "duplicate slug"
		}
		seen[c.Slug] = true
		if c.Name == "" || utf8.RuneCountInString(c.Name) > 64 || settings.HasControl(c.Name) {
			fe[key+".name"] = "name must be 1-64 characters"
		}
		if len(c.Icon) > 64 || (c.Icon != "" && !slugRe.MatchString(c.Icon)) {
			fe[key+".icon"] = "invalid icon name"
		}
		if len(c.Subcategories) > 100 {
			fe[key+".subcategories"] = "at most 100 subcategories"
		}
		subSeen := map[string]bool{}
		for j, sc := range c.Subcategories {
			skey := key + ".subcategories[" + strconv.Itoa(j) + "]"
			if !slugRe.MatchString(sc.Slug) {
				fe[skey+".slug"] = "slug must be 1-64 lowercase letters, digits, - or _"
			} else if subSeen[sc.Slug] {
				fe[skey+".slug"] = "duplicate slug"
			}
			subSeen[sc.Slug] = true
			n := strings.TrimSpace(sc.Name)
			if n == "" || utf8.RuneCountInString(n) > 64 || settings.HasControl(n) {
				fe[skey+".name"] = "name must be 1-64 characters"
			}
		}
	}
	if len(fe) > 0 {
		return core.Validation(fe)
	}
	return nil
}

// ReplaceCategories replaces the taxonomy.
func (s *Service) ReplaceCategories(ctx context.Context, cats []Category) ([]Category, error) {
	if err := validateTaxonomy(cats); err != nil {
		return nil, err
	}
	err := s.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM categories`); err != nil {
			return err
		}
		for i, c := range cats {
			subs := make([]Subcategory, len(c.Subcategories))
			for j, sc := range c.Subcategories {
				subs[j] = Subcategory{Slug: sc.Slug, Name: strings.TrimSpace(sc.Name)}
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO categories (slug, name, icon, position, subcategories) VALUES (?, ?, ?, ?, ?)`,
				c.Slug, strings.TrimSpace(c.Name), c.Icon, i, db.JSON[[]Subcategory]{V: subs}); err != nil {
				return err
			}
		}
		return nil
	})
	tc := s.taxCache()
	tc.mu.Lock()
	tc.cats = nil
	tc.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return s.Categories(ctx)
}

func (m *Module) listCategories(w http.ResponseWriter, r *http.Request) error {
	cats, err := m.svc.Categories(r.Context())
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, cats)
}

func (m *Module) replaceCategories(w http.ResponseWriter, r *http.Request) error {
	var cats []Category
	if err := httpx.DecodeJSON(r, &cats); err != nil {
		return err
	}
	if cats == nil {
		return core.BadRequest("body must be a JSON array of categories")
	}
	out, err := m.svc.ReplaceCategories(r.Context(), cats)
	if err != nil {
		return err
	}
	m.d.Audit.Record(r.Context(), "admin.categories.update", "categories", "", map[string]any{"count": len(out)})
	return httpx.WriteJSON(w, http.StatusOK, out)
}
