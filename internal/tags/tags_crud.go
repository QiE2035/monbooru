package tags

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/leqwin/monbooru/internal/db"
	"github.com/leqwin/monbooru/internal/models"
)

// Tag rows themselves: name validation, get-or-create, listing and
// filtering, alias lookups, per-image reads, and deletion.

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

// NormalizeName folds an externally-sourced tag name into the stored form:
// lowercased, disallowed-character runs collapsed to `_`, ends trimmed. Import
// tokens pass through it so a hydrus tag like `hatsune miku` is stored as
// `hatsune_miku` instead of being rejected by ValidateTagName. Returns "" when
// nothing usable remains.
func NormalizeName(name string) string {
	name = strings.ToLower(name)
	name = disallowedTagCharsRe.ReplaceAllString(name, "_")
	return strings.Trim(name, "_")
}

func (s *Service) GetOrCreateTag(name string, categoryID int64) (*models.Tag, error) {
	return s.GetOrCreateTagFrom(name, categoryID, "user")
}

// GetOrCreateTagFrom is GetOrCreateTag with an explicit creation origin
// (a booru site, "ptr", an import label). The origin is stamped only on
// the insert; an existing row keeps its creator.
func (s *Service) GetOrCreateTagFrom(name string, categoryID int64, origin string) (*models.Tag, error) {
	normalized, err := ValidateTagName(name)
	if err != nil {
		return nil, err
	}
	if s.ratingCatID != 0 && categoryID == s.ratingCatID && !IsCanonicalRating(normalized) {
		return nil, ErrNonCanonicalRating
	}

	var tag *models.Tag
	err = s.inWriteTx(func(tx *sql.Tx) error {
		var txErr error
		tag, txErr = getOrCreateTagTx(tx, normalized, categoryID, origin)
		return txErr
	})
	return tag, err
}

func getOrCreateTagTx(tx *sql.Tx, name string, categoryID int64, origin string) (*models.Tag, error) {
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
			`INSERT INTO tags (name, category_id, origin) VALUES (?, ?, ?) RETURNING id`,
			name, categoryID, origin,
		).Scan(&id); err != nil {
			return nil, fmt.Errorf("inserting tag: %w", err)
		}
		tag = models.Tag{
			ID:         id,
			Name:       name,
			CategoryID: categoryID,
			Origin:     origin,
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
		where += " AND t.name LIKE ? ESCAPE '\\'"
		args = append(args, db.EscapeLike(filter.Prefix)+"%")
	}
	switch filter.Origin {
	case "":
	case "alias":
		// Legacy spelling kept for pre-Type URLs and API callers;
		// alias-ness is structure, not provenance.
		where += " AND t.is_alias = 1"
	default:
		where += " AND t.origin = ?"
		args = append(args, filter.Origin)
	}
	switch filter.Type {
	case "alias":
		where += " AND t.is_alias = 1"
	case "tag":
		where += " AND t.is_alias = 0"
	}
	if filter.CreatedAfter != "" {
		where += " AND t.created_at >= ?"
		args = append(args, filter.CreatedAfter)
	}
	if filter.ConflictsOnly {
		where += ` AND t.is_alias = 0 AND t.name IN (
			SELECT name FROM tags WHERE is_alias = 0
			GROUP BY name HAVING COUNT(DISTINCT category_id) >= 2)`
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
	switch {
	case filter.ConflictsOnly:
		// Colliding pairs must sit adjacent; the category tiebreak keeps
		// each pair's order stable.
		orderBy = "t.name ASC, t.category_id ASC"
	case filter.Sort == "usage" || filter.Sort == "created" || filter.Sort == "last_used":
		// These default to DESC when no order is set (most-used / newest /
		// most recently applied first).
		dir = "DESC"
		if strings.EqualFold(filter.Order, "asc") {
			dir = "ASC"
		}
		switch filter.Sort {
		case "usage":
			orderBy = "t.usage_count " + dir + ", t.name ASC"
		case "created":
			orderBy = "t.created_at " + dir + ", t.id " + dir
		case "last_used":
			// SQLite sorts NULL smallest, so never-applied rows land last
			// on the default DESC and first on ASC.
			orderBy = "t.last_used_at " + dir + ", t.name ASC"
		}
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
		        t.origin, t.last_used_at,
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
	defer func() { _ = rows.Close() }()

	var tagList []models.Tag
	for rows.Next() {
		var t models.Tag
		var isAlias int
		var canonicalID sql.NullInt64
		var createdAt string
		var lastUsed sql.NullString
		var canonName, canonCatName, canonCatColor sql.NullString
		if err := rows.Scan(
			&t.ID, &t.Name, &t.CategoryID, &t.CategoryName, &t.CategoryColor,
			&t.UsageCount, &isAlias, &canonicalID, &createdAt,
			&t.Origin, &lastUsed,
			&canonName, &canonCatName, &canonCatColor,
		); err != nil {
			return nil, 0, err
		}
		if lastUsed.Valid {
			t.LastUsedAt, _ = time.Parse(time.RFC3339, lastUsed.String)
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

	return tagList, total, nil
}

// ConflictsCount reports how many tag names occupy more than one
// category, for the /tags Conflicts filter badge and the Maintenance
// diagnostic.
func (s *Service) ConflictsCount() (int, error) {
	var n int
	err := s.db.Read.QueryRow(`SELECT COUNT(*) FROM (
		SELECT name FROM tags WHERE is_alias = 0
		GROUP BY name HAVING COUNT(DISTINCT category_id) >= 2)`).Scan(&n)
	return n, err
}

// OriginCount pairs a stored creation-origin label with how many tags
// carry it.
type OriginCount struct {
	Label string
	Count int
}

// OriginCounts returns the distinct non-empty creation origins in the
// catalog, most-populated first, for the /tags sidebar filter.
func (s *Service) OriginCounts() ([]OriginCount, error) {
	rows, err := s.db.Read.Query(
		`SELECT origin, COUNT(*) FROM tags WHERE origin <> '' GROUP BY origin ORDER BY COUNT(*) DESC, origin ASC`,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []OriginCount
	for rows.Next() {
		var oc OriginCount
		if err := rows.Scan(&oc.Label, &oc.Count); err != nil {
			return nil, err
		}
		out = append(out, oc)
	}
	return out, rows.Err()
}

// AutoTaggerLabels reports which of labels appear as an auto-tagger
// attribution (an is_auto = 1 tagger_name) so origin chips can color
// machine creators apart from site creators.
func (s *Service) AutoTaggerLabels(labels []string) (map[string]struct{}, error) {
	set := make(map[string]struct{})
	seen := make(map[string]struct{}, len(labels))
	var args []any
	var ph []string
	for _, l := range labels {
		if l == "" {
			continue
		}
		if _, dup := seen[l]; dup {
			continue
		}
		seen[l] = struct{}{}
		args = append(args, l)
		ph = append(ph, "?")
	}
	if len(args) == 0 {
		return set, nil
	}
	// The literal IS NOT NULL / != '' terms restate
	// idx_image_tags_auto_tagger's partial predicate; without them the
	// planner can't prove the index applies and scans image_tags.
	rows, err := s.db.Read.Query(
		`SELECT DISTINCT tagger_name FROM image_tags
		 WHERE is_auto = 1 AND tagger_name IS NOT NULL AND tagger_name != ''
		   AND tagger_name IN (`+strings.Join(ph, ",")+`)`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			return nil, err
		}
		set[l] = struct{}{}
	}
	return set, rows.Err()
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
	defer func() { _ = rows.Close() }()
	return db.ScanIDs(rows)
}

func (s *Service) GetTag(id int64) (*models.Tag, error) {
	var t models.Tag
	var isAlias int
	var canonicalID sql.NullInt64
	var createdAt string

	var lastUsed sql.NullString
	err := s.db.Read.QueryRow(
		`SELECT t.id, t.name, t.category_id, tc.name, tc.color, t.usage_count,
		        t.is_alias, t.canonical_tag_id, t.created_at, t.origin, t.last_used_at
		 FROM tags t
		 JOIN tag_categories tc ON tc.id = t.category_id
		 WHERE t.id = ?`, id,
	).Scan(
		&t.ID, &t.Name, &t.CategoryID, &t.CategoryName, &t.CategoryColor,
		&t.UsageCount, &isAlias, &canonicalID, &createdAt, &t.Origin, &lastUsed,
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
	if lastUsed.Valid {
		t.LastUsedAt, _ = time.Parse(time.RFC3339, lastUsed.String)
	}
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
	err := db.Chunked(canonicalIDs, 500, func(batch []int64) error {
		placeholders, args := db.InPlaceholders(batch)
		rows, err := s.db.Read.Query(
			`SELECT a.id, a.name, a.category_id, ac.name, ac.color,
			        a.canonical_tag_id, a.origin,
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
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var t models.Tag
			var canonicalID int64
			if err := rows.Scan(
				&t.ID, &t.Name, &t.CategoryID, &t.CategoryName, &t.CategoryColor,
				&canonicalID, &t.Origin,
				&t.CanonicalName, &t.CanonicalCategoryName, &t.CanonicalCategoryColor,
			); err != nil {
				return err
			}
			t.IsAlias = true
			t.CanonicalTagID = &canonicalID
			out[canonicalID] = append(out[canonicalID], t)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// AppliedByCount is one attribution group over a tag's image_tags rows:
// which source applies the tag and on how many images.
type AppliedByCount struct {
	Label  string // tagger_name; "" = anonymous UI adds
	IsAuto bool
	Count  int
}

// UsageMonth is one month of a tag's still-present applications, keyed
// by the row's created_at. Removals leave the history and rows moved by
// a merge keep their original dates.
type UsageMonth struct {
	Month string // YYYY-MM
	Count int
}

// UsageBreakdown aggregates a tag's image_tags rows once and returns
// both detail-page views: applied-by attribution groups and the monthly
// histogram. One pass because the row fetch dominates on popular tags -
// a monster tag's hundreds of thousands of rows are read once, not once
// per panel.
func (s *Service) UsageBreakdown(tagID int64) ([]AppliedByCount, []UsageMonth, error) {
	rows, err := s.db.Read.Query(
		`SELECT COALESCE(tagger_name, ''), is_auto, strftime('%Y-%m', created_at), COUNT(*)
		 FROM image_tags WHERE tag_id = ?
		 GROUP BY COALESCE(tagger_name, ''), is_auto, strftime('%Y-%m', created_at)`,
		tagID,
	)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()
	appliedIdx := make(map[[2]any]int)
	monthIdx := make(map[string]int)
	var applied []AppliedByCount
	var months []UsageMonth
	for rows.Next() {
		var label, month string
		var isAuto, count int
		if err := rows.Scan(&label, &isAuto, &month, &count); err != nil {
			return nil, nil, err
		}
		ak := [2]any{label, isAuto}
		if i, ok := appliedIdx[ak]; ok {
			applied[i].Count += count
		} else {
			appliedIdx[ak] = len(applied)
			applied = append(applied, AppliedByCount{Label: label, IsAuto: isAuto == 1, Count: count})
		}
		if i, ok := monthIdx[month]; ok {
			months[i].Count += count
		} else {
			monthIdx[month] = len(months)
			months = append(months, UsageMonth{Month: month, Count: count})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	sort.Slice(applied, func(i, j int) bool {
		if applied[i].Count != applied[j].Count {
			return applied[i].Count > applied[j].Count
		}
		return applied[i].Label < applied[j].Label
	})
	sort.Slice(months, func(i, j int) bool { return months[i].Month < months[j].Month })
	return applied, months, nil
}

// GetImageTags returns the per-image tag list alongside the owning
// image's folder_path. Both pieces are read in one round trip via a
// LEFT JOIN over images so a freshly-uploaded image with no tags still
// surfaces its folder. The folder is empty when the image id is unknown.
func (s *Service) GetImageTags(imageID int64) (string, []models.ImageTag, error) {
	rows, err := s.db.Read.Query(
		`SELECT i.folder_path,
		        it.image_id, it.tag_id, t.name, tc.name, tc.color, t.usage_count,
		        it.is_auto, it.is_implied, it.confidence, it.tagger_name, it.created_at
		 FROM images i
		 LEFT JOIN image_tags it ON it.image_id = i.id
		 LEFT JOIN tags t ON t.id = it.tag_id
		 LEFT JOIN tag_categories tc ON tc.id = t.category_id
		 WHERE i.id = ?
		 ORDER BY tc.name, t.usage_count DESC, t.name`, imageID,
	)
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = rows.Close() }()

	var folder string
	var result []models.ImageTag
	for rows.Next() {
		var (
			folderPath sql.NullString
			imgID      sql.NullInt64
			tagID      sql.NullInt64
			tagName    sql.NullString
			category   sql.NullString
			color      sql.NullString
			usage      sql.NullInt64
			isAuto     sql.NullInt64
			isImplied  sql.NullInt64
			conf       sql.NullFloat64
			tagger     sql.NullString
			createdAt  sql.NullString
		)
		if err := rows.Scan(
			&folderPath,
			&imgID, &tagID, &tagName, &category, &color, &usage,
			&isAuto, &isImplied, &conf, &tagger, &createdAt,
		); err != nil {
			return "", nil, err
		}
		if folderPath.Valid {
			folder = folderPath.String
		}
		if !imgID.Valid || !tagID.Valid {
			// Untagged image - the LEFT JOIN emitted one row with NULL
			// tag columns; skip it but keep the folder.
			continue
		}
		it := models.ImageTag{
			ImageID:    imgID.Int64,
			TagID:      tagID.Int64,
			TagName:    nullStringValue(tagName),
			Category:   nullStringValue(category),
			Color:      nullStringValue(color),
			UsageCount: int(usage.Int64),
			IsAuto:     isAuto.Int64 == 1,
			IsImplied:  isImplied.Int64 == 1,
		}
		if conf.Valid {
			it.Confidence = &conf.Float64
		}
		if tagger.Valid {
			it.TaggerName = tagger.String
		}
		if createdAt.Valid {
			it.CreatedAt, _ = time.Parse(time.RFC3339, createdAt.String)
		}
		result = append(result, it)
	}
	return folder, result, rows.Err()
}

func nullStringValue(s sql.NullString) string {
	if s.Valid {
		return s.String
	}
	return ""
}

// deleteTagsTx strips ids from every image - including each id's transitive
// implied closure - and removes their aliases, inside tx. It does not delete
// the tag rows themselves: the single-tag and whole-category callers delete
// those by different keys. The returned closure is the set of implied
// descendants the caller must RecalcIDs after commit.
func deleteTagsTx(tx *sql.Tx, ids []int64) ([]int64, error) {
	closure, err := transitiveImpliedTx(tx, ids)
	if err != nil {
		return nil, fmt.Errorf("walk implied closure: %w", err)
	}
	for _, id := range ids {
		if _, err := tx.Exec(`DELETE FROM image_tags WHERE tag_id = ?`, id); err != nil {
			return nil, fmt.Errorf("strip parent image_tags: %w", err)
		}
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
			return nil, fmt.Errorf("sweep implied %d: %w", impID, err)
		}
	}
	for _, id := range ids {
		// Imported or legacy rows can chain alias -> alias, even back through
		// the deleted tag. Null the tag's own pointer so a cycle can't
		// dangle-check the sweep, then drop the whole alias subtree in one
		// statement so intra-chain references vanish together.
		if _, err := tx.Exec(`UPDATE tags SET canonical_tag_id = NULL WHERE id = ?`, id); err != nil {
			return nil, fmt.Errorf("unlink canonical: %w", err)
		}
		if _, err := tx.Exec(
			`DELETE FROM tags WHERE id IN (
			     WITH RECURSIVE sub(id) AS (
			         SELECT id FROM tags WHERE canonical_tag_id = ?
			         UNION
			         SELECT t.id FROM tags t JOIN sub ON t.canonical_tag_id = sub.id
			     )
			     SELECT id FROM sub
			 )`, id,
		); err != nil {
			return nil, fmt.Errorf("delete aliases: %w", err)
		}
	}
	return closure, nil
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
	var closure []int64
	err := s.inWriteTx(func(tx *sql.Tx) error {
		var err error
		closure, err = deleteTagsTx(tx, []int64{id})
		if err != nil {
			return err
		}
		res, err := tx.Exec(`DELETE FROM tags WHERE id = ?`, id)
		if err != nil {
			return fmt.Errorf("delete tag: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrTagNotFound
		}
		return nil
	})
	if err != nil {
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
	return s.inWriteTx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM image_tags WHERE tag_id = ?`, tagID); err != nil {
			return fmt.Errorf("strip image_tags: %w", err)
		}
		if _, err := tx.Exec(`UPDATE tags SET usage_count = 0 WHERE id = ?`, tagID); err != nil {
			return fmt.Errorf("zero usage: %w", err)
		}
		return nil
	})
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
	if err := s.db.Read.QueryRow(`SELECT category_id FROM tags WHERE id = ?`, id).Scan(&catID); err != nil {
		// Surface 404 for a missing id. The downstream UPDATE would
		// otherwise no-op silently and the handler would report a
		// successful rename for a tag that doesn't exist.
		if errors.Is(err, sql.ErrNoRows) {
			return ErrTagNotFound
		}
		return fmt.Errorf("look up tag %d: %w", id, err)
	}
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

// RenameTagKeepAlias renames the tag and installs its old name as an
// alias of the renamed row in the same transaction, so searches and
// adds of the old spelling keep resolving. Refused on alias rows - the
// leftover alias would point at an alias, a chain the resolver doesn't
// follow.
func (s *Service) RenameTagKeepAlias(id int64, newName string) error {
	normalized, err := ValidateTagName(newName)
	if err != nil {
		return err
	}
	return s.inWriteTx(func(tx *sql.Tx) error {
		var catID int64
		var oldName string
		var isAlias int
		if err := tx.QueryRow(`SELECT category_id, name, is_alias FROM tags WHERE id = ?`, id).Scan(&catID, &oldName, &isAlias); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrTagNotFound
			}
			return fmt.Errorf("look up tag %d: %w", id, err)
		}
		if isAlias == 1 {
			return fmt.Errorf("cannot keep the old name of an alias; rename it plainly")
		}
		if s.ratingCatID != 0 && catID == s.ratingCatID {
			return ErrRatingTagImmutable
		}
		if oldName == normalized {
			return nil
		}
		var existing int64
		if err := tx.QueryRow(
			`SELECT id FROM tags WHERE name = ? AND category_id = ? AND id != ?`, normalized, catID, id,
		).Scan(&existing); err == nil {
			return fmt.Errorf("a tag named %q already exists in this category", normalized)
		}
		if _, err := tx.Exec(`UPDATE tags SET name = ? WHERE id = ?`, normalized, id); err != nil {
			return err
		}
		// The old (name, category) slot vacated inside this tx, so the
		// alias insert cannot collide.
		_, err := tx.Exec(
			`INSERT INTO tags (name, category_id, is_alias, canonical_tag_id, usage_count, origin) VALUES (?, ?, 1, ?, 0, 'user')`,
			oldName, catID, id,
		)
		return err
	})
}

// ErrCategoryCollision reports the (name, target category) collision a
// category move runs into, carrying the surviving row's id so callers
// can offer to merge into it instead.
type ErrCategoryCollision struct {
	Name       string
	ExistingID int64
}

func (e *ErrCategoryCollision) Error() string {
	return fmt.Sprintf("a tag named %q already exists in the target category", e.Name)
}

// ChangeTagCategoryMerge is ChangeTagCategory that resolves a name
// collision by merging the tag into the target category's existing row
// (the moving tag becomes an alias of the survivor). The bool reports
// whether a merge happened instead of a plain move.
func (s *Service) ChangeTagCategoryMerge(tagID, newCategoryID int64) (bool, error) {
	err := s.ChangeTagCategory(tagID, newCategoryID)
	var coll *ErrCategoryCollision
	if errors.As(err, &coll) {
		if err := s.MergeTags(tagID, coll.ExistingID); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, err
}

// ChangeTagCategory moves a tag to a different category. Returns
// ErrTagNotFound, ErrCategoryNotFound, or ErrCategoryCollision when a
// tag with the same name already lives in the target category.
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
		return &ErrCategoryCollision{Name: name, ExistingID: existing}
	}
	_, err := s.db.Write.Exec(`UPDATE tags SET category_id = ? WHERE id = ?`, newCategoryID, tagID)
	return err
}
