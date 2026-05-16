package web

import (
	"fmt"
	"html"
	"net/http"
	"strconv"

	"github.com/leqwin/monbooru/internal/config"
	"github.com/leqwin/monbooru/internal/logx"
	"github.com/leqwin/monbooru/internal/relations"
)

// relationsPhashDistance returns the operator-configured Find-pairs
// default Hamming distance (clamped to the documented 0..12 range).
// Reads through cfgMu so a Settings -> Relations save is honoured on
// the next page render without a restart.
func (s *Server) relationsPhashDistance() int {
	s.cfgMu.Lock()
	d := s.cfg.Relations.DefaultDistance
	s.cfgMu.Unlock()
	if d < 0 {
		return 0
	}
	if d > 12 {
		return 12
	}
	return d
}

// applyRelationsConfig mirrors the operator's [relations] block onto
// the relations package's runtime atomics. Called at boot and after
// every settings edit so a TOML save propagates without restart.
func applyRelationsConfig(rc config.RelationsConfig) {
	d := rc.DefaultDistance
	if d < 0 || d > 12 {
		d = 4
	}
	relations.IncrementalProbeDistance.Store(int32(d))
	relations.IncrementalProbeEnabled.Store(rc.IncrementalOnIngest)
}

// settingsRelationsPost reads the form, persists to TOML, then
// re-applies the atomics. IncrementalOnIngest stays true (the
// on-ingest probe is always on); only the find-pairs default
// distance and the default session order are operator-tunable.
func (s *Server) settingsRelationsPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	d, err := strconv.Atoi(r.FormValue("default_distance"))
	if err != nil || d < 0 || d > 12 {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`<div class="flash flash-err">Distance must be an integer 0..12.</div>`))
		return
	}
	order := r.FormValue("default_session_order")
	if !validOrderModes[order] {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`<div class="flash flash-err">Unknown session order.</div>`))
		return
	}
	s.cfgMu.Lock()
	s.cfg.Relations.DefaultDistance = d
	s.cfg.Relations.DefaultSessionOrder = order
	s.cfg.Relations.IncrementalOnIngest = true
	rc := s.cfg.Relations
	s.cfgMu.Unlock()
	if err := s.saveConfig(); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`<div class="flash flash-err">` + html.EscapeString(err.Error()) + `</div>`))
		return
	}
	applyRelationsConfig(rc)
	logx.Infof("settings: relations { distance=%d order=%s }", d, order)
	w.Write([]byte(fmt.Sprintf(`<div class="flash flash-ok">Saved. distance=%d order=%s</div>`, d, order)))
}

// relationsCounts is the cheap rollup the Relations page header
// renders. Each query rides a covering index; the whole page header
// builds in well under a millisecond on a 1M-image library.
type relationsCounts struct {
	PhashMissing    int
	QueueOpen       int
	QueueSkipped    int
	DupGroups       int
	AltGroups       int
	VersionEdges    int
	Derivatives     int
	NotRelatedPairs int
}

// browseCard is the per-group / per-edge row the Relations page lists
// in its browse section. Only the data the template renders is loaded
// up front - the rest comes from the user clicking through.
type browseCard struct {
	Kind     string  // "dup", "alt", "version", "derivative"
	GroupID  int64   // group id (dup / alt) or 0 for edges
	Members  []int64 // member ids; for version edges [parent, child]; for derivative [source, derivative]
	Original int64   // dup-group original; 0 for the other kinds
}

// relationsPage serves /relations: header counters and the per-section
// CTAs. Per-group cards live on /relations/browse-groups.
func (s *Server) relationsPage(w http.ResponseWriter, r *http.Request) {
	cx := s.Active()
	if cx == nil || cx.DB == nil {
		http.Error(w, "no gallery", http.StatusServiceUnavailable)
		return
	}
	counts := loadRelationsCounts(s, cx)
	s.renderTemplate(w, "relations.html", relationsPageData{
		baseData:      s.base(r, "relations", "Relations - Monbooru"),
		Counts:        counts,
		ActiveGallery: s.activeName,
	})
}

type relationsPageData struct {
	baseData
	Counts        relationsCounts
	ActiveGallery string
}

// browseGroupsPage renders /relations/browse-groups with one tab per
// group / edge kind. Each tab loads up to 30 cards via
// loadBrowseCardsByKind so the page render stays inside the latency
// budget on a 1M-image library.
func (s *Server) browseGroupsPage(w http.ResponseWriter, r *http.Request) {
	cx := s.Active()
	if cx == nil || cx.DB == nil {
		http.Error(w, "no gallery", http.StatusServiceUnavailable)
		return
	}
	kind := r.URL.Query().Get("kind")
	if kind == "" {
		kind = "dup"
	}
	if !validBrowseGroupKinds[kind] {
		http.NotFound(w, r)
		return
	}
	cards, err := loadBrowseCardsByKind(cx, kind, 30)
	if err != nil {
		logx.Warnf("browse-groups cards %s: %v", kind, err)
		http.Error(w, "load cards", http.StatusInternalServerError)
		return
	}
	counts := loadRelationsCounts(s, cx)
	s.renderTemplate(w, "relations_browse_groups.html", browseGroupsData{
		baseData:      s.base(r, "relations", "Browse groups - Monbooru"),
		ActiveGallery: s.activeName,
		Kind:          kind,
		Cards:         cards,
		Counts:        counts,
	})
}

type browseGroupsData struct {
	baseData
	ActiveGallery string
	Kind          string
	Cards         []browseCard
	Counts        relationsCounts
}

// validBrowseGroupKinds is the closed vocabulary the browse-groups page
// accepts as the ?kind= query parameter.
var validBrowseGroupKinds = map[string]bool{
	"dup":        true,
	"alt":        true,
	"version":    true,
	"derivative": true,
}

// loadRelationsCounts runs the seven small count queries the header
// renders. Errors during the rollup degrade to a zero count on that
// row - the page still renders the rest.
func loadRelationsCounts(s *Server, cx *galleryCtx) relationsCounts {
	var c relationsCounts
	get := func(q string, dst *int) {
		if err := cx.DB.Read.QueryRow(q).Scan(dst); err != nil {
			logx.Debugf("relations counts %q: %v", q, err)
		}
	}
	get(`SELECT COUNT(*) FROM images WHERE phash IS NULL AND is_missing = 0`, &c.PhashMissing)
	get(`SELECT COUNT(*) FROM potential_relation_pairs WHERE skipped_at IS NULL`, &c.QueueOpen)
	get(`SELECT COUNT(*) FROM potential_relation_pairs WHERE skipped_at IS NOT NULL`, &c.QueueSkipped)
	get(`SELECT COUNT(*) FROM dup_groups`, &c.DupGroups)
	get(`SELECT COUNT(*) FROM alt_groups`, &c.AltGroups)
	get(`SELECT COUNT(*) FROM version_edges`, &c.VersionEdges)
	get(`SELECT COUNT(*) FROM derivative_edges`, &c.Derivatives)
	get(`SELECT COUNT(*) FROM not_related_pairs`, &c.NotRelatedPairs)
	return c
}

// loadBrowseCardsByKind collects up to `limit` rows of one card kind in
// reverse-id order. Used by the per-tab groups page.
func loadBrowseCardsByKind(cx *galleryCtx, kind string, limit int) ([]browseCard, error) {
	switch kind {
	case "dup":
		dupRows, err := cx.DB.Read.Query(`SELECT id, original_image_id FROM dup_groups ORDER BY id DESC LIMIT ?`, limit)
		if err != nil {
			return nil, err
		}
		defer dupRows.Close()
		var cards []browseCard
		for dupRows.Next() {
			var id, original int64
			if err := dupRows.Scan(&id, &original); err != nil {
				return nil, err
			}
			members, mErr := scanGroupMembers(cx, "dup_group_members", id)
			if mErr != nil {
				return nil, mErr
			}
			cards = append(cards, browseCard{Kind: "dup", GroupID: id, Members: members, Original: original})
		}
		return cards, dupRows.Err()
	case "alt":
		altRows, err := cx.DB.Read.Query(`SELECT id FROM alt_groups ORDER BY id DESC LIMIT ?`, limit)
		if err != nil {
			return nil, err
		}
		defer altRows.Close()
		var cards []browseCard
		for altRows.Next() {
			var id int64
			if err := altRows.Scan(&id); err != nil {
				return nil, err
			}
			members, mErr := scanGroupMembers(cx, "alt_group_members", id)
			if mErr != nil {
				return nil, mErr
			}
			cards = append(cards, browseCard{Kind: "alt", GroupID: id, Members: members})
		}
		return cards, altRows.Err()
	case "version":
		verRows, err := cx.DB.Read.Query(`SELECT parent_image_id, child_image_id FROM version_edges ORDER BY child_image_id DESC LIMIT ?`, limit)
		if err != nil {
			return nil, err
		}
		defer verRows.Close()
		var cards []browseCard
		for verRows.Next() {
			var parent, child int64
			if err := verRows.Scan(&parent, &child); err != nil {
				return nil, err
			}
			cards = append(cards, browseCard{Kind: "version", Members: []int64{parent, child}})
		}
		return cards, verRows.Err()
	case "derivative":
		derRows, err := cx.DB.Read.Query(`SELECT source_image_id, derivative_image_id FROM derivative_edges ORDER BY derivative_image_id DESC LIMIT ?`, limit)
		if err != nil {
			return nil, err
		}
		defer derRows.Close()
		var cards []browseCard
		for derRows.Next() {
			var src, der int64
			if err := derRows.Scan(&src, &der); err != nil {
				return nil, err
			}
			cards = append(cards, browseCard{Kind: "derivative", Members: []int64{src, der}})
		}
		return cards, derRows.Err()
	}
	return nil, nil
}

// scanGroupMembers returns every image_id belonging to the named
// group in id order. Reused across dup_group_members and
// alt_group_members.
func scanGroupMembers(cx *galleryCtx, table string, groupID int64) ([]int64, error) {
	rows, err := cx.DB.Read.Query(`SELECT image_id FROM `+table+` WHERE group_id = ? ORDER BY image_id`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// browseRow is one row in the /relations/browse grid: parent and child
// image ids plus the source group id for the group-shaped relations
// (dup, alt). Version and derivative edges have no group; GroupID is 0.
type browseRow struct {
	GroupID  int64
	ParentID int64
	ChildID  int64
}

// validBrowseTypes is the closed vocabulary the /relations/browse page
// accepts as the ?type= query parameter.
var validBrowseTypes = map[string]bool{
	"duplicate":   true,
	"alternate":   true,
	"version":     true,
	"derivative":  true,
	"not_related": true,
}

// browseRelationsPage renders /relations/browse with a tab per
// relation type. The grid lists every declared relation in the same
// shape: parent_id, parent thumb, child thumb, child_id, action.
func (s *Server) browseRelationsPage(w http.ResponseWriter, r *http.Request) {
	cx := s.Active()
	if cx == nil || cx.DB == nil {
		http.Error(w, "no gallery", http.StatusServiceUnavailable)
		return
	}
	typ := r.URL.Query().Get("type")
	if typ == "" {
		typ = "duplicate"
	}
	if !validBrowseTypes[typ] {
		http.NotFound(w, r)
		return
	}
	rows, err := loadBrowseRows(cx, typ)
	if err != nil {
		logx.Warnf("browse rows %s: %v", typ, err)
		http.Error(w, "load rows", http.StatusInternalServerError)
		return
	}
	counts := loadRelationsCounts(s, cx)
	s.renderTemplate(w, "relations_browse.html", browseRelationsData{
		baseData:      s.base(r, "relations", "Browse relations - Monbooru"),
		ActiveGallery: s.activeName,
		Type:          typ,
		Rows:          rows,
		Counts:        counts,
	})
}

type browseRelationsData struct {
	baseData
	ActiveGallery string
	Type          string
	Rows          []browseRow
	Counts        relationsCounts
}

// loadBrowseRows expands each relation kind into a flat (parent, child)
// row list. Group-shaped relations (dup, alt) emit one row per
// non-canonical-pair so each member sits next to a stable peer
// (original for dup, smallest-id member for alt).
func loadBrowseRows(cx *galleryCtx, typ string) ([]browseRow, error) {
	const limit = 200
	switch typ {
	case "duplicate":
		return loadDupBrowseRows(cx, limit)
	case "alternate":
		return loadAltBrowseRows(cx, limit)
	case "version":
		return loadEdgeBrowseRows(cx,
			`SELECT 0, parent_image_id, child_image_id FROM version_edges ORDER BY child_image_id DESC LIMIT ?`,
			limit)
	case "derivative":
		return loadEdgeBrowseRows(cx,
			`SELECT 0, source_image_id, derivative_image_id FROM derivative_edges ORDER BY derivative_image_id DESC LIMIT ?`,
			limit)
	case "not_related":
		return loadEdgeBrowseRows(cx,
			`SELECT 0, a_image_id, b_image_id FROM not_related_pairs ORDER BY rowid DESC LIMIT ?`,
			limit)
	}
	return nil, nil
}

func loadDupBrowseRows(cx *galleryCtx, limit int) ([]browseRow, error) {
	rows, err := cx.DB.Read.Query(`
		SELECT g.id, g.original_image_id, m.image_id
		FROM dup_group_members m
		JOIN dup_groups g ON g.id = m.group_id
		WHERE m.image_id != g.original_image_id
		ORDER BY g.id DESC, m.image_id
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []browseRow
	for rows.Next() {
		var row browseRow
		if scanErr := rows.Scan(&row.GroupID, &row.ParentID, &row.ChildID); scanErr != nil {
			return nil, scanErr
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func loadAltBrowseRows(cx *galleryCtx, limit int) ([]browseRow, error) {
	rows, err := cx.DB.Read.Query(`
		WITH heads AS (
			SELECT group_id, MIN(image_id) AS head_id
			FROM alt_group_members
			GROUP BY group_id
		)
		SELECT m.group_id, h.head_id, m.image_id
		FROM alt_group_members m
		JOIN heads h ON h.group_id = m.group_id
		WHERE m.image_id != h.head_id
		ORDER BY m.group_id DESC, m.image_id
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []browseRow
	for rows.Next() {
		var row browseRow
		if scanErr := rows.Scan(&row.GroupID, &row.ParentID, &row.ChildID); scanErr != nil {
			return nil, scanErr
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func loadEdgeBrowseRows(cx *galleryCtx, query string, limit int) ([]browseRow, error) {
	rows, err := cx.DB.Read.Query(query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []browseRow
	for rows.Next() {
		var row browseRow
		if scanErr := rows.Scan(&row.GroupID, &row.ParentID, &row.ChildID); scanErr != nil {
			return nil, scanErr
		}
		out = append(out, row)
	}
	return out, rows.Err()
}
