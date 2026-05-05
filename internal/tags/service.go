package tags

import (
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/leqwin/monbooru/internal/db"
	"github.com/leqwin/monbooru/internal/models"
	"github.com/leqwin/monbooru/internal/searchkw"
)

var (
	ErrInvalidTagName       = errors.New("invalid tag name")
	ErrTagNotFound          = errors.New("tag not found")
	ErrCategoryNotFound     = errors.New("category not found")
	ErrBuiltinCategory      = errors.New("cannot delete built-in category")
	ErrReservedCategoryName = errors.New("this name is used by a search filter (e.g. " + reservedCategoryHint() + ")")
	ErrNonCanonicalRating   = errors.New("rating category accepts only general, sensitive, questionable, explicit")
	ErrRatingTagImmutable   = errors.New("rating category tags cannot be renamed, merged, deleted, or moved")

	// Allowed tag name characters. The colon is kept despite doubling
	// as the category:tag separator; the parser falls back to a literal
	// tag when the prefix is neither a filter keyword nor a known
	// category. The emoticon-set (?, <, >, =, ^) is permitted so names
	// like `>_<`, `=3`, `<3`, `^_^`, and `nani?` round-trip end-to-end.
	tagNameRe = regexp.MustCompile(`^[a-z0-9_()!@#$.~+:?<>=^\-]+$`)

	// #rgb or #rrggbb. Anything else gets ZgotmplZ'd in the template's
	// CSS context, so reject it up front with a useful error.
	categoryColorRe = regexp.MustCompile(`^#(?:[0-9a-fA-F]{3}|[0-9a-fA-F]{6})$`)

	ErrInvalidCategoryColor = errors.New("invalid category color (must be #rgb or #rrggbb)")
)

// IsValidCategoryColor reports whether s matches the #rgb / #rrggbb
// shape the UI form enforces.
func IsValidCategoryColor(s string) bool { return categoryColorRe.MatchString(s) }

// SafeCategoryColor returns s when it's a valid hex colour, otherwise
// the neutral fallback ("#888888"). Used on rows arriving from outside
// the UI form layer (JSON / DB imports) so foreign payloads never
// reach the inline-style template context unchecked.
func SafeCategoryColor(s string) string {
	if IsValidCategoryColor(s) {
		return s
	}
	return "#888888"
}

var (
	// reservedCategoryList is the source of truth for category names
	// refused at create/rename time: every search-filter keyword (which
	// would collide with `category:tag` parsing) plus "system" (the
	// search-bar cheat-sheet trigger; a real category by that name would
	// hijack `system:foo` into a category-qualified search). The slice
	// drives both reservedCategoryNames and ErrReservedCategoryName so
	// adding a future filter is a single edit in internal/searchkw.
	reservedCategoryList = append(append([]string{}, searchkw.Keywords...), "system")

	reservedCategoryNames = func() map[string]struct{} {
		m := make(map[string]struct{}, len(reservedCategoryList))
		for _, n := range reservedCategoryList {
			m[n] = struct{}{}
		}
		return m
	}()

	// RatingLevels is the canonical rating vocabulary, ordered low to high.
	// Highest-wins resolution and the cookie ceiling both rely on this order.
	RatingLevels = []string{"general", "sensitive", "questionable", "explicit"}

	ratingCanonicalSet = map[string]struct{}{
		"general":      {},
		"sensitive":    {},
		"questionable": {},
		"explicit":     {},
	}
)

// IsCanonicalRating reports whether name is one of the four allowed
// rating tag names. The rating category refuses any other name.
func IsCanonicalRating(name string) bool {
	_, ok := ratingCanonicalSet[name]
	return ok
}

func isReservedCategoryName(name string) bool {
	_, ok := reservedCategoryNames[name]
	return ok
}

// reservedCategoryHint formats reservedCategoryList as a human-readable
// "fav:, source:, cat:, ..." list for the inline error message. Computed
// at package init so the error string stays a single sentinel value.
func reservedCategoryHint() string {
	parts := make([]string, len(reservedCategoryList))
	for i, n := range reservedCategoryList {
		parts[i] = n + ":"
	}
	return strings.Join(parts, ", ")
}

// TagFilter controls listing behavior.
type TagFilter struct {
	CategoryID *int64
	Prefix     string
	Sort       string // "name" | "usage"
	Order      string // "asc" | "desc" - flips the primary sort direction
	// PageIndex is 0-based - callers supply the requested page number minus
	// one. ListTags multiplies by Limit to derive the SQL OFFSET.
	PageIndex int
	Limit     int
	Origin    string // "" | "user" | "auto" | "alias"
	// ShowZero opts in to surfacing non-alias tags whose usage_count is 0.
	// Default behaviour hides them so the listing reflects what is actually
	// applied to images; alias rows always render regardless because their
	// usage_count is 0 by construction.
	ShowZero bool
	// ZeroOnly narrows the listing to non-alias zero-usage tags only.
	// Implies ShowZero. Used by the /tags Zero-usage Only filter to find
	// declared-but-unused tags for triage.
	ZeroOnly bool
}

// Service provides tag and category CRUD with usage_count and co-occurrence maintenance.
type Service struct {
	db *db.DB
	// ratingCatID is the resolved id of the built-in `rating` category,
	// cached at New time so the GetOrCreateTag guard and the
	// rename/merge/delete/move-category refusals don't pay a SELECT per
	// call. The category row is built-in and never deleted, so this
	// never becomes stale; the four canonical rating tag IDs are
	// resolved per-call (they can be pruned on zero-usage and re-created
	// via GetOrCreateTag, so a long-lived cache would drift).
	ratingCatID int64
}

// New creates a new Service.
func New(database *db.DB) *Service {
	s := &Service{db: database}
	_ = database.Read.QueryRow(
		`SELECT id FROM tag_categories WHERE name = 'rating'`,
	).Scan(&s.ratingCatID)
	return s
}

// RatingCategoryID returns the cached id of the rating category, or 0
// when the category is missing (only possible on a pre-bootstrap DB).
func (s *Service) RatingCategoryID() int64 { return s.ratingCatID }

// RecalcDB recomputes usage_count from image_tags (non-missing images
// only). Call after bulk deletes, imports, or sync. Tag rows are kept
// even at zero usage so user-declared aliases and implications survive
// against an empty library.
func RecalcDB(database *db.DB) {
	_ = RecalcDBCount(database)
}

// RecalcDBCount is RecalcDB with the count of rows whose usage_count
// changed.
//
// A naive correlated-subquery UPDATE recomputes the count twice per
// tag and dominates sync time on tag-heavy libraries. This impl zeros
// out tags whose remaining usages all point at missing images, then
// fills in the rest with one GROUP BY pass over image_tags, chunked
// by tag_id range so the single writer is released between chunks.
func RecalcDBCount(database *db.DB) (updated int64) {
	const chunkSize = 2000

	var maxID int64
	if err := database.Read.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM tags`).Scan(&maxID); err != nil {
		return
	}

	for start := int64(0); start <= maxID; start += chunkSize {
		end := start + chunkSize
		if res, err := database.Write.Exec(`
			UPDATE tags SET usage_count = 0
			WHERE usage_count != 0
			  AND id >= ? AND id < ?
			  AND NOT EXISTS (
			      SELECT 1 FROM image_tags it
			      JOIN images i ON i.id = it.image_id
			      WHERE it.tag_id = tags.id AND i.is_missing = 0
			  )
		`, start, end); err == nil {
			n, _ := res.RowsAffected()
			updated += n
		}
		if res, err := database.Write.Exec(`
			UPDATE tags SET usage_count = c.cnt
			FROM (
			    SELECT it.tag_id, COUNT(*) AS cnt FROM image_tags it
			    JOIN images i ON i.id = it.image_id
			    WHERE i.is_missing = 0 AND it.tag_id >= ? AND it.tag_id < ?
			    GROUP BY it.tag_id
			) c
			WHERE c.tag_id = tags.id AND tags.usage_count != c.cnt
		`, start, end); err == nil {
			n, _ := res.RowsAffected()
			updated += n
		}
	}
	return
}

func (s *Service) Recalc() {
	RecalcDB(s.db)
}

func (s *Service) RecalcCount() (updated int64) {
	return RecalcDBCount(s.db)
}

// RecalcIDs recomputes usage_count for the given tag IDs. Lets bulk
// callers scope the work to tags they actually touched instead of
// walking the whole table. IDs are processed in chunks to stay under
// the SQLite parameter limit.
func (s *Service) RecalcIDs(ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	const chunkSize = 500
	for start := 0; start < len(ids); start += chunkSize {
		end := start + chunkSize
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[start:end]
		placeholders := strings.Repeat("?,", len(chunk))
		placeholders = placeholders[:len(placeholders)-1]
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		if _, err := s.db.Write.Exec(`UPDATE tags SET usage_count = (
			SELECT COUNT(*) FROM image_tags it
			JOIN images i ON i.id = it.image_id
			WHERE it.tag_id = tags.id AND i.is_missing = 0
		) WHERE id IN (`+placeholders+`)`, args...); err != nil {
			return fmt.Errorf("recalc usage_count chunk: %w", err)
		}
	}
	return nil
}

func (s *Service) ListCategories() ([]models.TagCategory, error) {
	rows, err := s.db.Read.Query(
		`SELECT id, name, color, is_builtin FROM tag_categories ORDER BY id`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var cats []models.TagCategory
	for rows.Next() {
		var c models.TagCategory
		var isBuiltin int
		if err := rows.Scan(&c.ID, &c.Name, &c.Color, &isBuiltin); err != nil {
			return nil, err
		}
		c.IsBuiltin = isBuiltin == 1
		cats = append(cats, c)
	}
	return cats, rows.Err()
}

func (s *Service) CreateCategory(name, color string) (*models.TagCategory, error) {
	name = strings.TrimSpace(strings.ToLower(name))
	if name == "" {
		return nil, fmt.Errorf("category name must not be empty")
	}
	if isReservedCategoryName(name) {
		return nil, ErrReservedCategoryName
	}
	color = strings.TrimSpace(color)
	if !categoryColorRe.MatchString(color) {
		return nil, ErrInvalidCategoryColor
	}
	var id int64
	err := s.db.Write.QueryRow(
		`INSERT INTO tag_categories (name, color) VALUES (?, ?) RETURNING id`,
		name, color,
	).Scan(&id)
	if err != nil {
		return nil, fmt.Errorf("creating category: %w", err)
	}
	return &models.TagCategory{ID: id, Name: name, Color: color}, nil
}

func (s *Service) UpdateCategoryColor(id int64, color string) error {
	color = strings.TrimSpace(color)
	if !categoryColorRe.MatchString(color) {
		return ErrInvalidCategoryColor
	}
	_, err := s.db.Write.Exec(
		`UPDATE tag_categories SET color = ? WHERE id = ?`, color, id,
	)
	return err
}

func (s *Service) RenameCategory(id int64, newName string) error {
	newName = strings.TrimSpace(strings.ToLower(newName))
	if newName == "" {
		return fmt.Errorf("category name must not be empty")
	}
	if isReservedCategoryName(newName) {
		return ErrReservedCategoryName
	}
	var isBuiltin int
	if err := s.db.Read.QueryRow(
		`SELECT is_builtin FROM tag_categories WHERE id = ?`, id,
	).Scan(&isBuiltin); err != nil {
		return ErrCategoryNotFound
	}
	if isBuiltin == 1 {
		return ErrBuiltinCategory
	}
	_, err := s.db.Write.Exec(
		`UPDATE tag_categories SET name = ? WHERE id = ?`, newName, id,
	)
	return err
}

// GetCategoryTagCount returns the number of tags in a category.
func (s *Service) GetCategoryTagCount(id int64) (int, error) {
	var count int
	err := s.db.Read.QueryRow(
		`SELECT COUNT(*) FROM tags WHERE category_id = ? AND is_alias = 0`, id,
	).Scan(&count)
	return count, err
}

// DeleteCategoryMoveOrDelete deletes a category. action="delete_all"
// deletes all tags in the category; "move" reparents them to targetID.
func (s *Service) DeleteCategoryMoveOrDelete(id int64, action string, targetID int64) error {
	tx, err := s.db.Write.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var isBuiltin int
	if err := tx.QueryRow(
		`SELECT is_builtin FROM tag_categories WHERE id = ?`, id,
	).Scan(&isBuiltin); err == sql.ErrNoRows {
		return ErrCategoryNotFound
	} else if err != nil {
		return err
	}
	if isBuiltin == 1 {
		return ErrBuiltinCategory
	}

	switch action {
	case "delete_all":
		rows, err := tx.Query(`SELECT id FROM tags WHERE category_id = ?`, id)
		if err != nil {
			return err
		}
		var tagIDs []int64
		for rows.Next() {
			var tid int64
			if scanErr := rows.Scan(&tid); scanErr != nil {
				rows.Close()
				return scanErr
			}
			tagIDs = append(tagIDs, tid)
		}
		if iterErr := rows.Err(); iterErr != nil {
			rows.Close()
			return iterErr
		}
		rows.Close()
		for _, tid := range tagIDs {
			if _, err := tx.Exec(`DELETE FROM image_tags WHERE tag_id = ?`, tid); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`DELETE FROM tags WHERE category_id = ?`, id); err != nil {
			return err
		}
	default: // "move"
		if targetID == 0 {
			if err := tx.QueryRow(
				`SELECT id FROM tag_categories WHERE name = 'general'`,
			).Scan(&targetID); err != nil {
				return fmt.Errorf("finding general category: %w", err)
			}
		}
		if _, err := tx.Exec(
			`UPDATE tags SET category_id = ? WHERE category_id = ?`, targetID, id,
		); err != nil {
			return err
		}
	}

	if _, err := tx.Exec(`DELETE FROM tag_categories WHERE id = ?`, id); err != nil {
		return err
	}

	return tx.Commit()
}

// ValidateTagName lowercases + trims name and checks it against the
// documented allowlist. Returns the normalised name or an
// ErrInvalidTagName-wrapped error. Exposed so non-UI sources (the
// auto-tagger label loader, the JSON import path) apply the same rules.
func ValidateTagName(name string) (string, error) {
	name = strings.TrimSpace(name)
	name = strings.ToLower(name)

	if len(name) == 0 || len(name) > 200 {
		return "", fmt.Errorf("%w: length must be 1-200 characters", ErrInvalidTagName)
	}

	if !tagNameRe.MatchString(name) {
		return "", fmt.Errorf("%w: contains invalid characters (allowed: a-z 0-9 _ ( ) ! @ # $ . ~ + - : ? < > = ^)", ErrInvalidTagName)
	}

	// Reject names made entirely of separator-class punctuation (e.g. "---")
	// while still admitting emoticon-only tags like ">_<", "^_^", "<3".
	hasContent := false
	for _, r := range name {
		if unicode.IsLetter(r) || unicode.IsDigit(r) ||
			r == '?' || r == '<' || r == '>' || r == '=' || r == '^' {
			hasContent = true
			break
		}
	}
	if !hasContent {
		return "", fmt.Errorf("%w: name must contain at least one letter, digit, or emoticon character", ErrInvalidTagName)
	}

	return name, nil
}

func (s *Service) GetOrCreateTag(name string, categoryID int64) (*models.Tag, error) {
	normalized, err := ValidateTagName(name)
	if err != nil {
		return nil, err
	}
	if s.ratingCatID != 0 && categoryID == s.ratingCatID && !IsCanonicalRating(normalized) {
		return nil, ErrNonCanonicalRating
	}

	tx, err := s.db.Write.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	tag, err := getOrCreateTagTx(tx, normalized, categoryID)
	if err != nil {
		return nil, err
	}
	return tag, tx.Commit()
}

func getOrCreateTagTx(tx *sql.Tx, name string, categoryID int64) (*models.Tag, error) {
	var tag models.Tag
	var createdAt string
	var canonicalID sql.NullInt64
	// Look up by (name, category_id) so the same name can live in
	// multiple categories.
	err := tx.QueryRow(
		`SELECT id, name, category_id, usage_count, is_alias, canonical_tag_id, created_at FROM tags WHERE name = ? AND category_id = ?`,
		name, categoryID,
	).Scan(&tag.ID, &tag.Name, &tag.CategoryID, &tag.UsageCount, &tag.IsAlias, &canonicalID, &createdAt)

	if err == sql.ErrNoRows {
		var id int64
		if err := tx.QueryRow(
			`INSERT INTO tags (name, category_id) VALUES (?, ?) RETURNING id`,
			name, categoryID,
		).Scan(&id); err != nil {
			return nil, fmt.Errorf("inserting tag: %w", err)
		}
		tag = models.Tag{
			ID:         id,
			Name:       name,
			CategoryID: categoryID,
			CreatedAt:  time.Now().UTC(),
		}
		return &tag, nil
	}
	if err != nil {
		return nil, err
	}

	// If this row is an alias, redirect to its canonical. MergeTags
	// refuses to point an alias at another alias, so one hop is enough.
	if tag.IsAlias && canonicalID.Valid {
		var canon models.Tag
		var canonCreated string
		if err := tx.QueryRow(
			`SELECT id, name, category_id, usage_count, is_alias, created_at FROM tags WHERE id = ?`,
			canonicalID.Int64,
		).Scan(&canon.ID, &canon.Name, &canon.CategoryID, &canon.UsageCount, &canon.IsAlias, &canonCreated); err == nil {
			canon.CreatedAt, _ = time.Parse(time.RFC3339, canonCreated)
			return &canon, nil
		}
	}

	tag.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	return &tag, nil
}

// tagFilterWhere builds the WHERE clause and bound args shared by
// ListTags and ListTagIDs so both views see exactly the same set.
func tagFilterWhere(filter TagFilter) (string, []any) {
	args := []any{}
	where := "1=1"

	if filter.CategoryID != nil {
		where += " AND t.category_id = ?"
		args = append(args, *filter.CategoryID)
	}
	if filter.Prefix != "" {
		where += " AND t.name LIKE ?"
		args = append(args, filter.Prefix+"%")
	}
	switch filter.Origin {
	case "auto":
		// Require at least one row so a zero-usage tag (no rows at all) is
		// not silently classified as auto-only by the negative existential.
		where += " AND t.is_alias = 0 AND t.usage_count > 0 AND NOT EXISTS (SELECT 1 FROM image_tags it WHERE it.tag_id = t.id AND it.is_auto = 0)"
	case "user":
		where += " AND t.is_alias = 0 AND EXISTS (SELECT 1 FROM image_tags it WHERE it.tag_id = t.id AND it.is_auto = 0)"
	case "alias":
		where += " AND t.is_alias = 1"
	}
	switch {
	case filter.ZeroOnly:
		// Strictly zero-usage non-alias rows. Aliases are excluded because
		// their usage_count is 0 by construction and would otherwise drown
		// the actual triage targets.
		where += " AND t.usage_count = 0 AND t.is_alias = 0"
	case !filter.ShowZero:
		// Hide non-alias zero-usage rows. Aliases always pass because
		// their usage_count is 0 by construction.
		where += " AND (t.usage_count > 0 OR t.is_alias = 1)"
	}
	return where, args
}

func (s *Service) ListTags(filter TagFilter) ([]models.Tag, int, error) {
	where, args := tagFilterWhere(filter)

	dir := "ASC"
	if strings.EqualFold(filter.Order, "desc") {
		dir = "DESC"
	}
	orderBy := "t.name " + dir
	if filter.Sort == "usage" {
		// Default usage to DESC when no order is set (most-used first).
		dir = "DESC"
		if strings.EqualFold(filter.Order, "asc") {
			dir = "ASC"
		}
		orderBy = "t.usage_count " + dir + ", t.name ASC"
	}

	var total int
	if err := s.db.Read.QueryRow(
		"SELECT COUNT(*) FROM tags t WHERE "+where, args...,
	).Scan(&total); err != nil {
		return nil, 0, err
	}

	limit := filter.Limit
	if limit <= 0 {
		limit = 40
	}
	offset := filter.PageIndex * limit

	// LEFT JOIN pulls the canonical name/category when t.is_alias = 1
	// so the caller can render "alias -> canonical" without a second
	// round trip.
	query := fmt.Sprintf(
		`SELECT t.id, t.name, t.category_id, tc.name, tc.color,
		        t.usage_count, t.is_alias, t.canonical_tag_id, t.created_at,
		        c.name, cc.name, cc.color
		 FROM tags t
		 JOIN tag_categories tc ON tc.id = t.category_id
		 LEFT JOIN tags c ON c.id = t.canonical_tag_id
		 LEFT JOIN tag_categories cc ON cc.id = c.category_id
		 WHERE %s ORDER BY %s LIMIT ? OFFSET ?`,
		where, orderBy,
	)
	args = append(args, limit, offset)

	rows, err := s.db.Read.Query(query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var tagList []models.Tag
	for rows.Next() {
		var t models.Tag
		var isAlias int
		var canonicalID sql.NullInt64
		var createdAt string
		var canonName, canonCatName, canonCatColor sql.NullString
		if err := rows.Scan(
			&t.ID, &t.Name, &t.CategoryID, &t.CategoryName, &t.CategoryColor,
			&t.UsageCount, &isAlias, &canonicalID, &createdAt,
			&canonName, &canonCatName, &canonCatColor,
		); err != nil {
			return nil, 0, err
		}
		t.IsAlias = isAlias == 1
		if canonicalID.Valid {
			t.CanonicalTagID = &canonicalID.Int64
		}
		if canonName.Valid {
			t.CanonicalName = canonName.String
		}
		if canonCatName.Valid {
			t.CanonicalCategoryName = canonCatName.String
		}
		if canonCatColor.Valid {
			t.CanonicalCategoryColor = canonCatColor.String
		}
		t.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
		tagList = append(tagList, t)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	// Aliases have no image_tags of their own; the origin badge for
	// them is "alias", not "auto-only".
	var ids []any
	for _, t := range tagList {
		if !t.IsAlias {
			ids = append(ids, t.ID)
		}
	}
	if len(ids) > 0 {
		placeholders := strings.Repeat("?,", len(ids))
		placeholders = placeholders[:len(placeholders)-1]
		userRows, err := s.db.Read.Query(
			`SELECT DISTINCT tag_id FROM image_tags WHERE is_auto = 0 AND tag_id IN (`+placeholders+`)`,
			ids...,
		)
		if err != nil {
			return nil, 0, err
		}
		hasUser := map[int64]struct{}{}
		for userRows.Next() {
			var id int64
			if err := userRows.Scan(&id); err != nil {
				userRows.Close()
				return nil, 0, err
			}
			hasUser[id] = struct{}{}
		}
		userRows.Close()
		for i := range tagList {
			if tagList[i].IsAlias {
				continue
			}
			if _, ok := hasUser[tagList[i].ID]; !ok {
				tagList[i].IsAutoOnly = true
			}
		}
	}

	return tagList, total, nil
}

// ListTagIDs returns every tag id matching the filter, ignoring
// PageIndex / Limit. Used by /tags' bulk delete-in-current-search so
// the dialog count and the actual delete set agree.
func (s *Service) ListTagIDs(filter TagFilter) ([]int64, error) {
	where, args := tagFilterWhere(filter)
	rows, err := s.db.Read.Query(`SELECT t.id FROM tags t WHERE `+where+` ORDER BY t.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Service) GetTag(id int64) (*models.Tag, error) {
	var t models.Tag
	var isAlias int
	var canonicalID sql.NullInt64
	var createdAt string

	err := s.db.Read.QueryRow(
		`SELECT t.id, t.name, t.category_id, tc.name, tc.color, t.usage_count,
		        t.is_alias, t.canonical_tag_id, t.created_at
		 FROM tags t
		 JOIN tag_categories tc ON tc.id = t.category_id
		 WHERE t.id = ?`, id,
	).Scan(
		&t.ID, &t.Name, &t.CategoryID, &t.CategoryName, &t.CategoryColor,
		&t.UsageCount, &isAlias, &canonicalID, &createdAt,
	)
	if err == sql.ErrNoRows {
		return nil, ErrTagNotFound
	}
	if err != nil {
		return nil, err
	}

	t.IsAlias = isAlias == 1
	if canonicalID.Valid {
		t.CanonicalTagID = &canonicalID.Int64
	}
	t.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	return &t, nil
}

// AliasesForTagIDs returns alias rows keyed by the canonical tag id
// they point at, with display fields joined for chip rendering on the
// detail page (where viewers benefit from seeing which alternate names
// also surface this image in search). One query regardless of input
// size, chunked at the SQLite parameter cap.
func (s *Service) AliasesForTagIDs(canonicalIDs []int64) (map[int64][]models.Tag, error) {
	out := make(map[int64][]models.Tag, len(canonicalIDs))
	if len(canonicalIDs) == 0 {
		return out, nil
	}
	const chunk = 500
	for start := 0; start < len(canonicalIDs); start += chunk {
		end := start + chunk
		if end > len(canonicalIDs) {
			end = len(canonicalIDs)
		}
		batch := canonicalIDs[start:end]
		placeholders := strings.Repeat("?,", len(batch))
		placeholders = placeholders[:len(placeholders)-1]
		args := make([]any, len(batch))
		for i, id := range batch {
			args[i] = id
		}
		rows, err := s.db.Read.Query(
			`SELECT a.id, a.name, a.category_id, ac.name, ac.color,
			        a.canonical_tag_id,
			        c.name, cc.name, cc.color
			 FROM tags a
			 JOIN tag_categories ac ON ac.id = a.category_id
			 JOIN tags c ON c.id = a.canonical_tag_id
			 JOIN tag_categories cc ON cc.id = c.category_id
			 WHERE a.is_alias = 1 AND a.canonical_tag_id IN (`+placeholders+`)
			 ORDER BY a.name`,
			args...,
		)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var t models.Tag
			var canonicalID int64
			if err := rows.Scan(
				&t.ID, &t.Name, &t.CategoryID, &t.CategoryName, &t.CategoryColor,
				&canonicalID,
				&t.CanonicalName, &t.CanonicalCategoryName, &t.CanonicalCategoryColor,
			); err != nil {
				rows.Close()
				return nil, err
			}
			t.IsAlias = true
			t.CanonicalTagID = &canonicalID
			out[canonicalID] = append(out[canonicalID], t)
		}
		rows.Close()
	}
	return out, nil
}

func (s *Service) GetImageTags(imageID int64) ([]models.ImageTag, error) {
	rows, err := s.db.Read.Query(
		`SELECT it.image_id, it.tag_id, t.name, tc.name, tc.color, t.usage_count,
		        it.is_auto, it.is_implied, it.confidence, it.tagger_name, it.created_at
		 FROM image_tags it
		 JOIN tags t ON t.id = it.tag_id
		 JOIN tag_categories tc ON tc.id = t.category_id
		 WHERE it.image_id = ?
		 ORDER BY tc.name, t.usage_count DESC, t.name`, imageID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []models.ImageTag
	for rows.Next() {
		var it models.ImageTag
		var isAuto, isImplied int
		var conf sql.NullFloat64
		var taggerName sql.NullString
		var createdAt string
		if err := rows.Scan(
			&it.ImageID, &it.TagID, &it.TagName, &it.Category, &it.Color, &it.UsageCount,
			&isAuto, &isImplied, &conf, &taggerName, &createdAt,
		); err != nil {
			return nil, err
		}
		it.IsAuto = isAuto == 1
		it.IsImplied = isImplied == 1
		if conf.Valid {
			it.Confidence = &conf.Float64
		}
		if taggerName.Valid {
			it.TaggerName = taggerName.String
		}
		it.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
		result = append(result, it)
	}
	return result, rows.Err()
}

func (s *Service) AddTagToImage(imageID, tagID int64, isAuto bool, confidence *float64) error {
	_, _, err := s.AddTagToImageReportingDup(imageID, tagID, isAuto, confidence, "")
	return err
}

// AddTagToImageFromTagger is AddTagToImage with a source identifier
// stored alongside the row (auto-tagger subfolder name when isAuto, or
// any caller-supplied string for manual/API adds).
func (s *Service) AddTagToImageFromTagger(imageID, tagID int64, isAuto bool, confidence *float64, taggerName string) error {
	_, _, err := s.AddTagToImageReportingDup(imageID, tagID, isAuto, confidence, taggerName)
	return err
}

// AddTagToImageReportingDup runs INSERT OR IGNORE inside a write-pool
// transaction. The first bool reports whether a brand-new row was
// inserted; the second reports whether an existing implied row was
// promoted to user-owned (is_implied flipped 1 → 0). Both false means
// the row already existed as user-owned.
func (s *Service) AddTagToImageReportingDup(imageID, tagID int64, isAuto bool, confidence *float64, taggerName string) (bool, bool, error) {
	tx, err := s.db.Write.Begin()
	if err != nil {
		return false, false, err
	}
	defer tx.Rollback()

	added, promoted, err := addTagToImageTxReportingDup(tx, imageID, tagID, isAuto, confidence, taggerName)
	if err != nil {
		return false, false, err
	}
	// Highest-rank-wins among rating tags. The PK lookup is cheap and
	// the prune is a no-op when the image carries 0 or 1 rating tags
	// (the typical case after this rule has been applied to a row).
	if (added || promoted) && s.ratingCatID != 0 {
		var catID int64
		if err := tx.QueryRow(`SELECT category_id FROM tags WHERE id = ?`, tagID).Scan(&catID); err == nil && catID == s.ratingCatID {
			if err := pruneLowerRatingsTx(tx, s.ratingCatID, imageID); err != nil {
				return false, false, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return false, false, err
	}
	return added, promoted, nil
}

func addTagToImageTxReportingDup(tx *sql.Tx, imageID, tagID int64, isAuto bool, confidence *float64, taggerName string) (bool, bool, error) {
	isAutoInt := 0
	if isAuto {
		isAutoInt = 1
	}
	// tagger_name doubles as a generic source identifier: the tagger
	// subfolder name on auto rows, any caller-supplied string on manual
	// rows, NULL for UI-driven user adds.
	var tname any
	if taggerName != "" {
		tname = taggerName
	}

	res, err := tx.Exec(
		`INSERT OR IGNORE INTO image_tags (image_id, tag_id, is_auto, is_implied, confidence, tagger_name) VALUES (?, ?, ?, 0, ?, ?)`,
		imageID, tagID, isAutoInt, confidence, tname,
	)
	if err != nil {
		return false, false, fmt.Errorf("inserting image_tag: %w", err)
	}
	added, _ := res.RowsAffected()
	var promoted int64
	if added == 0 {
		// Row already present. If it was an implication-side row, the
		// caller's explicit add converts it to user-owned so removing a
		// parent later won't sweep it out from under them.
		upd, err := tx.Exec(
			`UPDATE image_tags SET is_implied = 0, is_auto = ?, confidence = ?, tagger_name = ?
			 WHERE image_id = ? AND tag_id = ? AND is_implied = 1`,
			isAutoInt, confidence, tname, imageID, tagID,
		)
		if err != nil {
			return false, false, err
		}
		promoted, _ = upd.RowsAffected()
	} else {
		// usage_count is the visible-image count for the tag; RecalcDB
		// rebuilds it that way. Adding a tag to a missing image must
		// not bump it, otherwise the next unrelated mutation that
		// triggers RecalcIDs silently drops the count back down.
		if _, err := tx.Exec(
			`UPDATE tags SET usage_count = usage_count + 1
			 WHERE id = ? AND (SELECT is_missing FROM images WHERE id = ?) = 0`,
			tagID, imageID,
		); err != nil {
			return false, false, err
		}
	}

	if err := fanOutImpliedTxImpl(tx, imageID, tagID, isAutoInt); err != nil {
		return false, false, err
	}

	if added == 0 {
		return false, promoted > 0, nil
	}
	return true, false, nil
}

// TransitiveImpliedTx is the exported entrypoint for callers outside
// the tags package (the implication propagation job in internal/web)
// that need to walk the implication graph inside the same transaction
// they already hold open. Mirrors Service.ImpliedTagIDs but tx-bound
// so a freshly-added edge is visible to the walk.
func TransitiveImpliedTx(tx *sql.Tx, parents []int64) ([]int64, error) {
	return transitiveImpliedTx(tx, parents)
}

// transitiveImpliedTx is the package-internal twin of
// Service.ImpliedTagIDs: the implication graph walked inside the same
// transaction the caller already holds open, so a freshly-added edge
// is visible.
func transitiveImpliedTx(tx *sql.Tx, parents []int64) ([]int64, error) {
	if len(parents) == 0 {
		return nil, nil
	}
	seen := make(map[int64]struct{}, len(parents))
	for _, p := range parents {
		seen[p] = struct{}{}
	}
	frontier := append([]int64(nil), parents...)
	var out []int64
	for depth := 0; depth < MaxImplicationDepth && len(frontier) > 0; depth++ {
		placeholders := strings.Repeat("?,", len(frontier))
		placeholders = placeholders[:len(placeholders)-1]
		args := make([]any, len(frontier))
		for i, id := range frontier {
			args[i] = id
		}
		rows, err := tx.Query(
			`SELECT DISTINCT implied_tag_id FROM tag_implications WHERE parent_tag_id IN (`+placeholders+`)`,
			args...,
		)
		if err != nil {
			return nil, err
		}
		var next []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			next = append(next, id)
			out = append(out, id)
		}
		rows.Close()
		frontier = next
	}
	return out, nil
}

func (s *Service) RemoveTagFromImage(imageID, tagID int64) error {
	tx, err := s.db.Write.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := removeTagFromImageTx(tx, imageID, tagID); err != nil {
		return err
	}
	return tx.Commit()
}

func removeTagFromImageTx(tx *sql.Tx, imageID, tagID int64) error {
	// Walk the parent's implication closure before deleting so we know
	// which implied rows might lose their last justifying parent. The
	// closure only matters when the row being removed is itself a
	// parent in the graph; for ordinary tags the SELECT comes back empty.
	implied, err := transitiveImpliedTx(tx, []int64{tagID})
	if err != nil {
		return err
	}

	res, err := tx.Exec(
		`DELETE FROM image_tags WHERE image_id = ? AND tag_id = ?`, imageID, tagID,
	)
	if err != nil {
		return err
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return nil
	}

	// Symmetric to the add path: a missing image was never counted in
	// usage_count, so removing the row must not decrement it either.
	if _, err := tx.Exec(
		`UPDATE tags SET usage_count = MAX(0, usage_count - 1)
		 WHERE id = ? AND (SELECT is_missing FROM images WHERE id = ?) = 0`,
		tagID, imageID,
	); err != nil {
		return err
	}

	// For every transitively implied tag still sitting on the image as
	// is_implied=1, drop it unless another parent currently on the image
	// still implies it. is_implied=0 rows are user-owned and untouched.
	for _, impID := range implied {
		var rowImplied int
		err := tx.QueryRow(
			`SELECT is_implied FROM image_tags WHERE image_id = ? AND tag_id = ?`, imageID, impID,
		).Scan(&rowImplied)
		if err == sql.ErrNoRows {
			continue
		} else if err != nil {
			return err
		}
		if rowImplied != 1 {
			continue
		}
		stillImplied, err := implicationParentsOnImageExcluding(tx, imageID, impID, tagID)
		if err != nil {
			return err
		}
		if len(stillImplied) > 0 {
			continue
		}
		if _, err := tx.Exec(
			`DELETE FROM image_tags WHERE image_id = ? AND tag_id = ?`, imageID, impID,
		); err != nil {
			return err
		}
		if _, err := tx.Exec(
			`UPDATE tags SET usage_count = MAX(0, usage_count - 1)
			 WHERE id = ? AND (SELECT is_missing FROM images WHERE id = ?) = 0`,
			impID, imageID,
		); err != nil {
			return err
		}
	}
	return nil
}

// RemoveUserTagsFromImage drops every manual image_tags row for one
// image and adjusts usage counts.
func (s *Service) RemoveUserTagsFromImage(imageID int64) error {
	tx, err := s.db.Write.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	rows, err := tx.Query(`SELECT tag_id FROM image_tags WHERE image_id = ? AND is_auto = 0`, imageID)
	if err != nil {
		return err
	}
	var tagIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		tagIDs = append(tagIDs, id)
	}
	rows.Close()

	for _, tagID := range tagIDs {
		if err := removeTagFromImageTx(tx, imageID, tagID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// RemoveAutoTagsFromImage drops auto-tagged image_tags rows for one
// image. A non-empty taggerNames restricts the deletion to rows whose
// tagger_name matches.
func (s *Service) RemoveAutoTagsFromImage(imageID int64, taggerNames []string) error {
	tx, err := s.db.Write.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var (
		rows *sql.Rows
	)
	if len(taggerNames) == 0 {
		rows, err = tx.Query(`SELECT tag_id FROM image_tags WHERE image_id = ? AND is_auto = 1`, imageID)
	} else {
		placeholders := strings.Repeat("?,", len(taggerNames))
		placeholders = placeholders[:len(placeholders)-1]
		args := []any{imageID}
		for _, n := range taggerNames {
			args = append(args, n)
		}
		rows, err = tx.Query(
			`SELECT tag_id FROM image_tags WHERE image_id = ? AND is_auto = 1 AND tagger_name IN (`+placeholders+`)`,
			args...,
		)
	}
	if err != nil {
		return err
	}
	var tagIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		tagIDs = append(tagIDs, id)
	}
	rows.Close()

	for _, tagID := range tagIDs {
		if err := removeTagFromImageTx(tx, imageID, tagID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Service) RemoveAllTagsFromImage(imageID int64) error {
	tx, err := s.db.Write.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	rows, err := tx.Query(`SELECT tag_id FROM image_tags WHERE image_id = ?`, imageID)
	if err != nil {
		return err
	}
	var tagIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		tagIDs = append(tagIDs, id)
	}
	rows.Close()

	if len(tagIDs) > 0 {
		// Skip the bulk decrement when the image was missing: its rows
		// were never counted in usage_count to begin with.
		var isMissing int
		if err := tx.QueryRow(`SELECT is_missing FROM images WHERE id = ?`, imageID).Scan(&isMissing); err != nil && err != sql.ErrNoRows {
			return err
		}
		if isMissing == 0 {
			placeholders := strings.Repeat("?,", len(tagIDs))
			placeholders = placeholders[:len(placeholders)-1]
			args := make([]any, len(tagIDs))
			for i, id := range tagIDs {
				args[i] = id
			}
			if _, err := tx.Exec(
				`UPDATE tags SET usage_count = MAX(0, usage_count - 1) WHERE id IN (`+placeholders+`)`,
				args...,
			); err != nil {
				return err
			}
		}
	}

	if _, err := tx.Exec(`DELETE FROM image_tags WHERE image_id = ?`, imageID); err != nil {
		return err
	}

	return tx.Commit()
}

// relatedGeneralTagsCap bounds the general-category portion of the
// probe set so a source carrying `1girl` doesn't drag every image_tags
// row for that tag into the candidate GROUP BY. Non-general non-meta
// categories pass through uncapped because they carry distinguishing
// signal worth the scan even when common.
const relatedGeneralTagsCap = 15

// ratingRank returns the position of name in RatingLevels (0-indexed,
// general < sensitive < questionable < explicit). Returns -1 for any
// non-canonical name. Mirrors the same helper in internal/search; kept
// package-private here so tags doesn't depend on search.
func ratingRank(name string) int {
	for i, l := range RatingLevels {
		if l == name {
			return i
		}
	}
	return -1
}

// PruneLowerRatingsTx keeps only the highest-rank rating tag on imageID.
// When the image carries multiple rating-category rows (general <
// sensitive < questionable < explicit) the lower-rank rows are removed
// via removeTagFromImageTx so usage_count adjustment and the implied
// closure cleanup match the rest of the tag-removal path. Idempotent:
// after the call the image carries at most one rating tag.
//
// Both the manual add path (AddTagToImageReportingDup) and the auto-
// tagger's storeResults call this so highest-rank-wins is the durable
// invariant a fresh write upholds. fastCountCeiling and fastCountRating
// rely on the invariant for their constant-time bounds.
//
// ratingCatID is the rating category id; pass 0 to skip (only possible
// against a pre-bootstrap DB, where the four canonical rating rows
// don't yet exist).
func PruneLowerRatingsTx(tx *sql.Tx, ratingCatID, imageID int64) error {
	return pruneLowerRatingsTx(tx, ratingCatID, imageID)
}

func pruneLowerRatingsTx(tx *sql.Tx, ratingCatID, imageID int64) error {
	if ratingCatID == 0 {
		return nil
	}
	rows, err := tx.Query(
		`SELECT it.tag_id, t.name FROM image_tags it
		 JOIN tags t ON t.id = it.tag_id
		 WHERE it.image_id = ? AND t.category_id = ? AND t.is_alias = 0`,
		imageID, ratingCatID,
	)
	if err != nil {
		return fmt.Errorf("scan rating rows for prune: %w", err)
	}
	type ratingRow struct {
		tagID int64
		name  string
	}
	var present []ratingRow
	for rows.Next() {
		var r ratingRow
		if err := rows.Scan(&r.tagID, &r.name); err != nil {
			rows.Close()
			return err
		}
		present = append(present, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(present) <= 1 {
		return nil
	}
	bestRank := -1
	for _, r := range present {
		if rank := ratingRank(r.name); rank > bestRank {
			bestRank = rank
		}
	}
	for _, r := range present {
		if ratingRank(r.name) >= bestRank {
			continue
		}
		if err := removeTagFromImageTx(tx, imageID, r.tagID); err != nil {
			return fmt.Errorf("prune lower rating %d: %w", r.tagID, err)
		}
	}
	return nil
}

// RatingTagIDsAbove returns the canonical rating tag ids whose level
// ranks strictly above ceiling (e.g. ceiling="sensitive" returns the ids
// of "questionable" and "explicit"). An empty or unknown ceiling, or
// "explicit" (the no-ceiling sentinel), returns nil. The lookup runs a
// fresh SELECT each call so a tag pruned and re-created at runtime is
// resolved to its current id.
func (s *Service) RatingTagIDsAbove(ceiling string) []int64 {
	if s.ratingCatID == 0 {
		return nil
	}
	rank := -1
	for i, l := range RatingLevels {
		if l == ceiling {
			rank = i
			break
		}
	}
	if rank < 0 || rank >= len(RatingLevels)-1 {
		return nil
	}
	above := RatingLevels[rank+1:]
	placeholders := strings.Repeat("?,", len(above))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, 0, len(above)+1)
	args = append(args, s.ratingCatID)
	for _, n := range above {
		args = append(args, n)
	}
	rows, err := s.db.Read.Query(
		`SELECT id FROM tags WHERE category_id = ? AND name IN (`+placeholders+`)`,
		args...,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil
		}
		ids = append(ids, id)
	}
	return ids
}

// relatedMaxTagUsage drops seed tags whose global usage_count exceeds
// the cap. A tag carried by more than this many images doesn't add
// discriminative signal - the candidate set it brings in is mostly
// noise - and on a 1M-image library it's the difference between a 2 s
// GROUP BY and a sub-second one. Seed images whose every tag is
// over-cap render an empty related panel rather than a slow one.
const relatedMaxTagUsage = 10000

// RelatedImages returns up to limit images that share the most tags
// with imageID, ranked by shared tag count. The source image, missing
// images, and meta-only matches are excluded.
//
// Staged so the images join only runs against the top-N candidates:
// my_tags resolves once (general capped to the K rarest, popular tags
// dropped via relatedMaxTagUsage so the candidate scan stays bounded);
// candidates aggregate from image_tags alone with an inner LIMIT; the
// images row is joined last so is_missing filtering costs O(buffer).
// The SELECT only fetches what partials/related_images.html actually
// consumes (currently `.ID`), since the rest of the row would be
// unused payload across the SQL boundary.
//
// ratingCeiling, when non-empty, drops candidates carrying any rating
// tag above the ceiling level (highest-wins). Pass "" or "explicit" to
// disable the filter.
func (s *Service) RelatedImages(imageID int64, limit int, ratingCeiling string) ([]models.Image, error) {
	excluded := s.RatingTagIDsAbove(ratingCeiling)

	candidatesExtra := ""
	args := []any{imageID, relatedMaxTagUsage, relatedGeneralTagsCap, imageID}
	if len(excluded) > 0 {
		placeholders := strings.Repeat("?,", len(excluded))
		placeholders = placeholders[:len(placeholders)-1]
		candidatesExtra = ` AND NOT EXISTS (
		         SELECT 1 FROM image_tags x
		         WHERE x.image_id = theirs.image_id
		           AND x.tag_id IN (` + placeholders + `)
		     )`
		for _, id := range excluded {
			args = append(args, id)
		}
	}
	args = append(args, limit*2+5, limit)

	rows, err := s.db.Read.Query(
		`WITH my_tags AS (
		     SELECT tag_id FROM (
		         SELECT it.tag_id, tc.name AS cat_name,
		                ROW_NUMBER() OVER (PARTITION BY tc.name
		                                   ORDER BY t.usage_count ASC, t.id ASC) AS rn
		         FROM image_tags it
		         JOIN tags t ON t.id = it.tag_id
		         JOIN tag_categories tc ON tc.id = t.category_id
		         WHERE it.image_id = ? AND tc.name != 'meta'
		           AND t.usage_count <= ?
		     )
		     WHERE cat_name != 'general' OR rn <= ?
		 ),
		 candidates AS (
		     SELECT theirs.image_id, COUNT(*) AS shared
		     FROM image_tags theirs
		     WHERE theirs.tag_id IN (SELECT tag_id FROM my_tags)
		       AND theirs.image_id != ?`+candidatesExtra+`
		     GROUP BY theirs.image_id
		     ORDER BY shared DESC, theirs.image_id DESC
		     LIMIT ?
		 )
		 SELECT i.id
		 FROM candidates c
		 JOIN images i ON i.id = c.image_id
		 WHERE i.is_missing = 0
		 ORDER BY c.shared DESC, c.image_id DESC
		 LIMIT ?`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.Image
	for rows.Next() {
		var img models.Image
		if err := rows.Scan(&img.ID); err != nil {
			return nil, err
		}
		out = append(out, img)
	}
	return out, rows.Err()
}

// SuggestTags returns tags matching prefix, sorted by usage_count DESC.
// Two-pass shape: prefix matches first, then substring matches.
func (s *Service) SuggestTags(prefix string, limit int) ([]models.Tag, error) {
	prefixLimit := limit
	rows, err := s.db.Read.Query(
		`SELECT t.id, t.name, tc.name, tc.color, t.usage_count
		 FROM tags t
		 JOIN tag_categories tc ON tc.id = t.category_id
		 WHERE t.name LIKE ? AND t.is_alias = 0
		 ORDER BY t.usage_count DESC
		 LIMIT ?`,
		prefix+"%", prefixLimit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	seen := map[int64]struct{}{}
	var result []models.Tag
	for rows.Next() {
		var t models.Tag
		if err := rows.Scan(&t.ID, &t.Name, &t.CategoryName, &t.CategoryColor, &t.UsageCount); err != nil {
			return nil, err
		}
		seen[t.ID] = struct{}{}
		result = append(result, t)
	}
	rows.Close()

	if len(result) < limit {
		remaining := limit - len(result)
		rows2, err := s.db.Read.Query(
			`SELECT t.id, t.name, tc.name, tc.color, t.usage_count
			 FROM tags t
			 JOIN tag_categories tc ON tc.id = t.category_id
			 WHERE t.name LIKE ? AND t.name NOT LIKE ? AND t.is_alias = 0
			 ORDER BY t.usage_count DESC
			 LIMIT ?`,
			"%"+prefix+"%", prefix+"%", remaining,
		)
		if err != nil {
			return result, nil
		}
		defer rows2.Close()

		for rows2.Next() {
			var t models.Tag
			if err := rows2.Scan(&t.ID, &t.Name, &t.CategoryName, &t.CategoryColor, &t.UsageCount); err != nil {
				return nil, err
			}
			if _, ok := seen[t.ID]; !ok {
				result = append(result, t)
			}
		}
	}

	return result, nil
}

// SuggestTagsInCategory returns tags matching prefix in the named
// category, sorted by usage_count DESC.
func (s *Service) SuggestTagsInCategory(prefix, categoryName string, limit int) ([]models.Tag, error) {
	rows, err := s.db.Read.Query(
		`SELECT t.id, t.name, tc.name, tc.color, t.usage_count
		 FROM tags t
		 JOIN tag_categories tc ON tc.id = t.category_id
		 WHERE tc.name = ? AND t.name LIKE ? AND t.is_alias = 0
		 ORDER BY t.usage_count DESC
		 LIMIT ?`,
		categoryName, prefix+"%", limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []models.Tag
	for rows.Next() {
		var t models.Tag
		if err := rows.Scan(&t.ID, &t.Name, &t.CategoryName, &t.CategoryColor, &t.UsageCount); err != nil {
			return nil, err
		}
		result = append(result, t)
	}
	return result, rows.Err()
}

// DeleteTag removes a tag from every image and drops the tag row. Alias
// rows pointing at it are removed too (their canonical_tag_id would
// otherwise dangle). image_tags rows cascade on the tags FK, but the
// per-image removal runs first so the implied closure is swept - the
// FK cascade alone would drop the parent row and leave its
// is_implied=1 dependents on every carrier image with nothing on the
// image to justify them.
//
// Implementation: one bulk DELETE for the parent's image_tags rows,
// then a tier-by-tier sweep of the transitive implied closure where
// each tier deletes is_implied=1 rows whose only justification was the
// (now-gone) parent or an upstream implied. Same end-state as the
// per-image walk but linear in the number of distinct implied tags
// rather than the number of carrier images. Final RecalcIDs reconciles
// usage_count for every tag the cascade touched.
//
// Rating-category tags are immutable in the catalog (the four canonical
// names are part of the data model) so the row itself stays. Delete on
// one of them strips its image_tags rows instead - the user-visible
// "remove this rating from every image" the UI exposes.
func (s *Service) DeleteTag(id int64) error {
	if s.isRatingTag(id) {
		return s.stripTagFromAllImages(id)
	}
	tx, err := s.db.Write.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	closure, err := transitiveImpliedTx(tx, []int64{id})
	if err != nil {
		return fmt.Errorf("walk implied closure: %w", err)
	}

	if _, err := tx.Exec(`DELETE FROM image_tags WHERE tag_id = ?`, id); err != nil {
		return fmt.Errorf("strip parent image_tags: %w", err)
	}

	// Tier order: transitiveImpliedTx returns BFS, so dropping rows in
	// that order makes deeper tiers see the now-gone upstream rows when
	// they re-check whether any remaining parent on the image still
	// justifies them.
	for _, impID := range closure {
		if _, err := tx.Exec(
			`DELETE FROM image_tags
			 WHERE tag_id = ? AND is_implied = 1
			   AND NOT EXISTS (
			       SELECT 1 FROM tag_implications ti
			       JOIN image_tags it_p ON it_p.tag_id = ti.parent_tag_id
			       WHERE ti.implied_tag_id = ? AND it_p.image_id = image_tags.image_id
			   )`,
			impID, impID,
		); err != nil {
			return fmt.Errorf("sweep implied %d: %w", impID, err)
		}
	}

	if _, err := tx.Exec(`DELETE FROM tags WHERE canonical_tag_id = ?`, id); err != nil {
		return fmt.Errorf("delete aliases: %w", err)
	}
	res, err := tx.Exec(`DELETE FROM tags WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete tag: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrTagNotFound
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// Recompute usage_count over the swept set so the cached value
	// reflects the post-state without a full Recalc.
	if len(closure) > 0 {
		return s.RecalcIDs(closure)
	}
	return nil
}

// stripTagFromAllImages clears every image_tags row for tagID and zeros
// the tag's usage_count. Used by DeleteTag's rating-tag branch where
// the catalog row must stay intact.
func (s *Service) stripTagFromAllImages(tagID int64) error {
	tx, err := s.db.Write.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM image_tags WHERE tag_id = ?`, tagID); err != nil {
		return fmt.Errorf("strip image_tags: %w", err)
	}
	if _, err := tx.Exec(`UPDATE tags SET usage_count = 0 WHERE id = ?`, tagID); err != nil {
		return fmt.Errorf("zero usage: %w", err)
	}
	return tx.Commit()
}

// isRatingTag reports whether the tag with this id lives in the rating
// category. Returns false on lookup error so a missing row falls through
// to the existing ErrTagNotFound path in the caller.
func (s *Service) isRatingTag(id int64) bool {
	if s.ratingCatID == 0 {
		return false
	}
	var catID int64
	if err := s.db.Read.QueryRow(`SELECT category_id FROM tags WHERE id = ?`, id).Scan(&catID); err != nil {
		return false
	}
	return catID == s.ratingCatID
}

// RenameTag renames a tag. The new name must pass validation and must
// not collide with another tag in the same category.
func (s *Service) RenameTag(id int64, newName string) error {
	normalized, err := ValidateTagName(newName)
	if err != nil {
		return err
	}
	var catID int64
	s.db.Read.QueryRow(`SELECT category_id FROM tags WHERE id = ?`, id).Scan(&catID)
	if s.ratingCatID != 0 && catID == s.ratingCatID {
		return ErrRatingTagImmutable
	}
	var existing int64
	if err := s.db.Read.QueryRow(
		`SELECT id FROM tags WHERE name = ? AND category_id = ? AND id != ?`, normalized, catID, id,
	).Scan(&existing); err == nil {
		return fmt.Errorf("a tag named %q already exists in this category", normalized)
	}
	_, err = s.db.Write.Exec(`UPDATE tags SET name = ? WHERE id = ?`, normalized, id)
	return err
}

// MergeGeneralIntoCategorized scans general-category tags whose name has
// exactly one non-general non-meta counterpart and merges the general
// tag into that counterpart via MergeTags. General tags carrying any
// manual image_tags row are skipped (explicit user intent). Returns the
// number of tags merged.
func (s *Service) MergeGeneralIntoCategorized() (int, error) {
	rows, err := s.db.Read.Query(`
		SELECT g.id, c.id
		FROM tags g
		JOIN tag_categories gc ON gc.id = g.category_id
		JOIN tags c ON c.name = g.name AND c.is_alias = 0 AND c.id != g.id
		JOIN tag_categories cc ON cc.id = c.category_id
		WHERE g.is_alias = 0
		  AND gc.name = 'general'
		  AND cc.name NOT IN ('general', 'meta')
		  AND NOT EXISTS (
		      SELECT 1 FROM image_tags it
		      WHERE it.tag_id = g.id AND it.is_auto = 0
		  )
		  AND (
		      SELECT COUNT(*) FROM tags t2
		      JOIN tag_categories tc2 ON tc2.id = t2.category_id
		      WHERE t2.name = g.name AND t2.is_alias = 0
		        AND tc2.name NOT IN ('general', 'meta')
		  ) = 1`)
	if err != nil {
		return 0, err
	}
	type pair struct{ aliasID, canonID int64 }
	var pairs []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.aliasID, &p.canonID); err != nil {
			rows.Close()
			return 0, err
		}
		pairs = append(pairs, p)
	}
	rows.Close()

	merged := 0
	for _, p := range pairs {
		if err := s.MergeTags(p.aliasID, p.canonID); err != nil {
			return merged, err
		}
		merged++
	}
	return merged, nil
}

// ChangeTagCategory moves a tag to a different category. Returns
// ErrTagNotFound, ErrCategoryNotFound, or a clean error when a tag with
// the same name already lives in the target category.
func (s *Service) ChangeTagCategory(tagID, newCategoryID int64) error {
	var currentCatID int64
	var name string
	if err := s.db.Read.QueryRow(
		`SELECT category_id, name FROM tags WHERE id = ?`, tagID,
	).Scan(&currentCatID, &name); err != nil {
		return ErrTagNotFound
	}
	if currentCatID == newCategoryID {
		return nil
	}
	if s.ratingCatID != 0 && (currentCatID == s.ratingCatID || newCategoryID == s.ratingCatID) {
		return ErrRatingTagImmutable
	}
	var catExists int
	if err := s.db.Read.QueryRow(
		`SELECT COUNT(*) FROM tag_categories WHERE id = ?`, newCategoryID,
	).Scan(&catExists); err != nil || catExists == 0 {
		return ErrCategoryNotFound
	}
	// Reject up front so the user gets a clean message rather than the
	// raw UNIQUE(name, category_id) constraint error.
	var existing int64
	if err := s.db.Read.QueryRow(
		`SELECT id FROM tags WHERE name = ? AND category_id = ? AND id != ?`,
		name, newCategoryID, tagID,
	).Scan(&existing); err == nil {
		return fmt.Errorf("a tag named %q already exists in the target category", name)
	}
	_, err := s.db.Write.Exec(`UPDATE tags SET category_id = ? WHERE id = ?`, newCategoryID, tagID)
	return err
}
