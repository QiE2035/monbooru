package web

import (
	"fmt"
	"html"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

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
// builds in well under a millisecond on a 1M-image library. The
// version / derivative counters are "chains" / "trees" rather than
// "edges" so the number matches the card list /relations/browse
// renders: the same `AnyTainted` whole-group filter that drops a card
// also drops a chain / tree from the counter.
type relationsCounts struct {
	PhashMissing    int
	QueueOpen       int
	QueueSkipped    int
	DupGroups       int
	AltGroups       int
	VersionChains   int
	DerivativeTrees int
	NotRelatedPairs int
}

// browseCard is one row of the unified /relations/browse page. Group
// kinds (dup, alt) lift n members in a thumb strip; the version kind
// lifts a chain of N images and the derivative kind lifts a tree of
// N images, each rooted at the chain / tree's earliest ancestor; the
// not_related kind rides a two-image pair plus a comparison table.
// The template branches on .Kind.
type browseCard struct {
	Kind     string  // "duplicate" | "alternate" | "version" | "derivative" | "not_related"
	GroupID  int64   // group id for group kinds; 0 for chain / tree / pair rows
	Members  []int64 // group members in id order; for version chains root-to-leaf order; for derivative trees BFS order; for not_related [a, b]
	Original int64   // dup-group original; 0 for the other kinds
	// CreatedAt is the group / chain / tree / pair declaration date,
	// formatted as "2006-01-02 15:04:05" to match the detail page. For
	// chains and trees this is the newest edge's created_at so the
	// card's "declared" time tracks the last extension.
	CreatedAt string
	// MemberIngestedAt is keyed by Members[i] and carries the same
	// formatted ingest date the detail page shows for each image. The
	// template renders it next to the image id.
	MemberIngestedAt map[int64]string
	// Generations groups Members by depth for the version (chain) kind
	// so the template paints a left-to-right row of thumbs with one
	// image per generation. Empty for kinds without a chain structure.
	Generations [][]int64
	// TreeRows is the DFS-ordered (root first, then each child's
	// subtree before its next sibling) flattening of the derivative
	// tree, each row carrying its depth so the template can indent
	// children under their parent. Empty for kinds without a tree.
	TreeRows []treeRow
}

// treeRow is one entry of a DFS-flattened derivative tree. Depth=0
// marks the root; Trunks carries one segment per indent column from
// the root toward this row, encoding the CSS class the template uses
// to paint the branch lines:
//   - "line"  : ancestor at this depth still has more siblings below
//   - "empty" : ancestor at this depth was the last child (no trunk)
//   - "tee"   : this row is not the last child of its parent (the
//               parent's vertical continues past this row)
//   - "elbow" : this row is the last child of its parent (the vertical
//               stops at the row centre)
// The connector (tee / elbow) sits at the last index; earlier indices
// are the ancestor trunks. Root rows carry no trunks.
type treeRow struct {
	ID     int64
	Depth  int
	Trunks []string
}

// relationsPage serves /relations: header counters and the per-section
// CTAs. Per-group cards live on /relations/browse-groups.
func (s *Server) relationsPage(w http.ResponseWriter, r *http.Request) {
	cx := s.Active()
	if cx == nil || cx.DB == nil {
		http.Error(w, "no gallery", http.StatusServiceUnavailable)
		return
	}
	ceiling := resolveCeiling(r, cx)
	counts := loadRelationsCounts(s, cx, ceiling, nil)
	s.renderTemplate(w, "relations.html", relationsPageData{
		baseData:      s.base(r, "relations", "Relations - "+s.booruName()),
		Counts:        counts,
		ActiveGallery: s.activeName,
	})
}

type relationsPageData struct {
	baseData
	Counts        relationsCounts
	ActiveGallery string
}

// browseGroupsRedirect 301s the v1.8 /relations/browse-groups URL to
// the unified /relations/browse page. Keeps bookmarks alive without
// dragging the old route into the handler set.
func (s *Server) browseGroupsRedirect(w http.ResponseWriter, r *http.Request) {
	target := "/relations/browse?kind=duplicate"
	switch r.URL.Query().Get("kind") {
	case "alt":
		target = "/relations/browse?kind=alternate"
	case "version":
		target = "/relations/browse?kind=version"
	case "derivative":
		target = "/relations/browse?kind=derivative"
	case "dup", "":
		target = "/relations/browse?kind=duplicate"
	}
	http.Redirect(w, r, target, http.StatusMovedPermanently)
}

// relationsCountOverrides lets the caller skip the version_edges /
// derivative_edges walk when it has already counted those rows for a
// matching card build. A nil pointer field means "walk normally". Used
// by /relations/browse?kind=version|derivative to avoid reading the
// edge tables twice per render.
type relationsCountOverrides struct {
	VersionChains   *int
	DerivativeTrees *int
}

// loadRelationsCounts runs the seven small count queries the header
// renders. Errors during the rollup degrade to a zero count on that
// row - the page still renders the rest. Every counter is ceiling-
// aware so the hub's numbers match what the operator can see and act
// on under their current cookie: group counters drop a group when
// any member is hidden, edge / pair counters drop a row when either
// side is hidden, PhashMissing skips hidden rows. This keeps the
// /relations hub consistent with /relations/browse, whose cards apply
// the same filters. ov is optional precomputed counts; a nil pointer
// field means "walk normally".
func loadRelationsCounts(s *Server, cx *galleryCtx, ceiling *Ceiling, ov *relationsCountOverrides) relationsCounts {
	var c relationsCounts
	get := func(q string, dst *int, args ...any) {
		if err := cx.DB.Read.QueryRow(q, args...).Scan(dst); err != nil {
			logx.Debugf("relations counts %q: %v", q, err)
		}
	}
	if n, err := cx.PhashMissingUnder(ceiling); err == nil {
		c.PhashMissing = n
	}
	openQ := `SELECT COUNT(*) FROM potential_relation_pairs WHERE skipped_at IS NULL`
	skipQ := `SELECT COUNT(*) FROM potential_relation_pairs WHERE skipped_at IS NOT NULL`
	if where, args := ceiling.WhereTwo("p.a_image_id", "p.b_image_id"); where != "" {
		openQ = `SELECT COUNT(*) FROM potential_relation_pairs p WHERE p.skipped_at IS NULL AND ` + where
		skipQ = `SELECT COUNT(*) FROM potential_relation_pairs p WHERE p.skipped_at IS NOT NULL AND ` + where
		get(openQ, &c.QueueOpen, args...)
		get(skipQ, &c.QueueSkipped, args...)
	} else {
		get(openQ, &c.QueueOpen)
		get(skipQ, &c.QueueSkipped)
	}
	if where, args := ceiling.WhereGroupClean("dup_group_members", "dup_groups.id"); where != "" {
		get(`SELECT COUNT(*) FROM dup_groups WHERE `+where, &c.DupGroups, args...)
	} else {
		get(`SELECT COUNT(*) FROM dup_groups`, &c.DupGroups)
	}
	if where, args := ceiling.WhereGroupClean("alt_group_members", "alt_groups.id"); where != "" {
		get(`SELECT COUNT(*) FROM alt_groups WHERE `+where, &c.AltGroups, args...)
	} else {
		get(`SELECT COUNT(*) FROM alt_groups`, &c.AltGroups)
	}
	if ov != nil && ov.VersionChains != nil {
		c.VersionChains = *ov.VersionChains
	} else if n, err := countVersionChains(cx, ceiling); err == nil {
		c.VersionChains = n
	} else {
		logx.Debugf("relations counts version chains: %v", err)
	}
	if ov != nil && ov.DerivativeTrees != nil {
		c.DerivativeTrees = *ov.DerivativeTrees
	} else if n, err := countDerivativeTrees(cx, ceiling); err == nil {
		c.DerivativeTrees = n
	} else {
		logx.Debugf("relations counts derivative trees: %v", err)
	}
	if where, args := ceiling.WhereTwo("a_image_id", "b_image_id"); where != "" {
		get(`SELECT COUNT(*) FROM not_related_pairs WHERE `+where, &c.NotRelatedPairs, args...)
	} else {
		get(`SELECT COUNT(*) FROM not_related_pairs`, &c.NotRelatedPairs)
	}
	return c
}

// loadBrowseCardsByKind collects up to `limit` cards of one relation
// kind in newest-first order. Group kinds (duplicate, alternate) lift
// the full member list per row; edge kinds (version, derivative) and
// the symmetric pair kind (not_related) emit a two-member slice in
// canonical order. Cards whose members fall above the operator's
// rating ceiling are filtered out via ceiling - an inactive ceiling
// disables the filter. The returned kindTotal is the post-ceiling
// count of chains / trees for version / derivative kinds (regardless
// of limit) so the caller can drive the matching hub counter without
// re-walking the edges; 0 for kinds the count helpers can query
// directly from SQL.
func loadBrowseCardsByKind(cx *galleryCtx, kind, sort string, limit, offset int, ceiling *Ceiling) ([]browseCard, int, error) {
	// groupCardsWhere wraps Ceiling.WhereGroupClean for the two
	// group-card scans: the underlying SQL is "AND <not-exists>" when
	// the predicate is non-empty, plain `1=1` filler otherwise so the
	// LIMIT placeholder ordering stays stable.
	groupCardsWhere := func(membersTable, groupCol string) (string, []any) {
		w, a := ceiling.WhereGroupClean(membersTable, groupCol)
		if w == "" {
			return "", nil
		}
		return " AND " + w, a
	}
	var cards []browseCard
	var walkedTotal int
	switch kind {
	case "duplicate":
		where, args := groupCardsWhere("dup_group_members", "dup_groups.id")
		orderBy := dupSortClause(sort)
		dupRows, err := cx.DB.Read.Query(
			`SELECT dup_groups.id, dup_groups.original_image_id, dup_groups.created_at FROM dup_groups WHERE 1=1`+where+` `+orderBy+` LIMIT ? OFFSET ?`,
			append(args, limit, offset)...,
		)
		if err != nil {
			return nil, 0, err
		}
		defer dupRows.Close()
		for dupRows.Next() {
			var id, original int64
			var createdAt string
			if err := dupRows.Scan(&id, &original, &createdAt); err != nil {
				return nil, 0, err
			}
			members, mErr := scanGroupMembers(cx, "dup_group_members", id)
			if mErr != nil {
				return nil, 0, mErr
			}
			cards = append(cards, browseCard{Kind: "duplicate", GroupID: id, Members: members, Original: original, CreatedAt: humanISOTime(createdAt)})
		}
		if err := dupRows.Err(); err != nil {
			return nil, 0, err
		}
	case "alternate":
		where, args := groupCardsWhere("alt_group_members", "alt_groups.id")
		orderBy := altSortClause(sort)
		altRows, err := cx.DB.Read.Query(
			`SELECT alt_groups.id, alt_groups.created_at FROM alt_groups WHERE 1=1`+where+` `+orderBy+` LIMIT ? OFFSET ?`,
			append(args, limit, offset)...,
		)
		if err != nil {
			return nil, 0, err
		}
		defer altRows.Close()
		for altRows.Next() {
			var id int64
			var createdAt string
			if err := altRows.Scan(&id, &createdAt); err != nil {
				return nil, 0, err
			}
			members, mErr := scanGroupMembers(cx, "alt_group_members", id)
			if mErr != nil {
				return nil, 0, mErr
			}
			cards = append(cards, browseCard{Kind: "alternate", GroupID: id, Members: members, CreatedAt: humanISOTime(createdAt)})
		}
		if err := altRows.Err(); err != nil {
			return nil, 0, err
		}
	case "version":
		// loadVersionChainCards always returns the full sorted set
		// and its post-ceiling total. The annotate-then-sort order
		// matters when sort=newest_member - that pass reads each
		// member's ingested_at, populated by annotateBrowseCardIngestedAt.
		chains, total, cErr := loadVersionChainCards(cx, 0, ceiling)
		if cErr != nil {
			return nil, 0, cErr
		}
		if sort == "newest_member" {
			if aErr := annotateBrowseCardIngestedAt(cx, chains); aErr != nil {
				logx.Warnf("browse cards ingest-dates version: %v", aErr)
			}
		}
		sortVersionCards(chains, sort)
		cards = append(cards, sliceWindow(chains, offset, limit)...)
		walkedTotal = total
	case "derivative":
		trees, total, tErr := loadDerivativeTreeCards(cx, 0, ceiling)
		if tErr != nil {
			return nil, 0, tErr
		}
		sortDerivativeCards(trees, sort)
		cards = append(cards, sliceWindow(trees, offset, limit)...)
		walkedTotal = total
	case "not_related":
		where, args := ceiling.WhereTwo("a_image_id", "b_image_id")
		q := `SELECT a_image_id, b_image_id, created_at FROM not_related_pairs`
		if where != "" {
			q += ` WHERE ` + where
		}
		q += ` ORDER BY rowid DESC LIMIT ? OFFSET ?`
		nrRows, err := cx.DB.Read.Query(q, append(args, limit, offset)...)
		if err != nil {
			return nil, 0, err
		}
		defer nrRows.Close()
		for nrRows.Next() {
			var a, b int64
			var createdAt string
			if err := nrRows.Scan(&a, &b, &createdAt); err != nil {
				return nil, 0, err
			}
			cards = append(cards, browseCard{Kind: "not_related", Members: []int64{a, b}, CreatedAt: humanISOTime(createdAt)})
		}
		if err := nrRows.Err(); err != nil {
			return nil, 0, err
		}
	default:
		return nil, 0, nil
	}
	if err := annotateBrowseCardIngestedAt(cx, cards); err != nil {
		// Log but keep rendering: dates are nice-to-have, the rest of
		// the card carries the operator's primary signal.
		logx.Warnf("browse cards ingest-dates %s: %v", kind, err)
	}
	return cards, walkedTotal, nil
}

// dupSortClause maps the per-kind whitelist value to a static
// ORDER BY tail. The sort value is already resolved through
// resolveBrowseSort so it's safe to splice directly.
func dupSortClause(sort string) string {
	switch sort {
	case "size":
		return `ORDER BY (SELECT COUNT(*) FROM dup_group_members WHERE group_id = dup_groups.id) DESC, dup_groups.id DESC`
	case "original_added":
		return `ORDER BY (SELECT ingested_at FROM images WHERE id = dup_groups.original_image_id) DESC, dup_groups.id DESC`
	}
	return `ORDER BY dup_groups.id DESC`
}

func altSortClause(sort string) string {
	if sort == "size" {
		return `ORDER BY (SELECT COUNT(*) FROM alt_group_members WHERE group_id = alt_groups.id) DESC, alt_groups.id DESC`
	}
	return `ORDER BY alt_groups.id DESC`
}

func sortVersionCards(cards []browseCard, sortKey string) {
	switch sortKey {
	case "length":
		sort.SliceStable(cards, func(i, j int) bool {
			return len(cards[i].Members) > len(cards[j].Members)
		})
	case "newest_member":
		newest := func(c browseCard) string {
			var best string
			for _, m := range c.Members {
				if d, ok := c.MemberIngestedAt[m]; ok && d > best {
					best = d
				}
			}
			return best
		}
		sort.SliceStable(cards, func(i, j int) bool {
			return newest(cards[i]) > newest(cards[j])
		})
	}
	// "recent" is the loader's default order; nothing to do.
}

func sortDerivativeCards(cards []browseCard, sortKey string) {
	if sortKey == "size" {
		sort.SliceStable(cards, func(i, j int) bool {
			return len(cards[i].Members) > len(cards[j].Members)
		})
	}
}

func sliceWindow(cards []browseCard, offset, limit int) []browseCard {
	if offset >= len(cards) {
		return nil
	}
	end := offset + limit
	if limit <= 0 || end > len(cards) {
		end = len(cards)
	}
	return cards[offset:end]
}

// annotateBrowseCardIngestedAt populates the per-card MemberIngestedAt
// map with each member's images.ingested_at, formatted the same way
// the detail page formats its dates. One bulk SELECT keeps the load
// O(1) regardless of card / member count.
func annotateBrowseCardIngestedAt(cx *galleryCtx, cards []browseCard) error {
	if len(cards) == 0 {
		return nil
	}
	idSet := map[int64]struct{}{}
	for _, c := range cards {
		for _, m := range c.Members {
			idSet[m] = struct{}{}
		}
	}
	if len(idSet) == 0 {
		return nil
	}
	placeholders := make([]string, 0, len(idSet))
	args := make([]any, 0, len(idSet))
	for id := range idSet {
		placeholders = append(placeholders, "?")
		args = append(args, id)
	}
	rows, err := cx.DB.Read.Query(
		`SELECT id, ingested_at FROM images WHERE id IN (`+strings.Join(placeholders, ",")+`)`,
		args...,
	)
	if err != nil {
		return err
	}
	defer rows.Close()
	dates := map[int64]string{}
	for rows.Next() {
		var id int64
		var ingested string
		if scanErr := rows.Scan(&id, &ingested); scanErr != nil {
			return scanErr
		}
		dates[id] = humanISOTime(ingested)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range cards {
		out := make(map[int64]string, len(cards[i].Members))
		for _, m := range cards[i].Members {
			if d, ok := dates[m]; ok {
				out[m] = d
			}
		}
		cards[i].MemberIngestedAt = out
	}
	return nil
}

// humanISOTime turns an ISO-8601 timestamp stored as TEXT in SQLite
// ("2026-05-17T08:00:00Z") into the matter-of-fact "2026-05-17 08:00:00"
// the rest of the app uses. Returns the input unchanged on a parse
// failure so a freshly added column never blanks a card body.
func humanISOTime(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	return t.UTC().Format("2006-01-02 15:04:05")
}

// humanISODate is humanISOTime's date-only sibling: the table-cell
// formatter on the comparison view drops the time so the cell stays
// short. Falls back to the substring split on a parse failure so the
// stored timestamp still renders something sensible.
func humanISODate(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		if i := strings.IndexByte(s, 'T'); i > 0 {
			return s[:i]
		}
		return s
	}
	return t.UTC().Format("2006-01-02")
}

// countVersionChains mirrors loadVersionChainCards but skips card
// construction and the limit cap: the chain-count counter on the
// /relations hub and on /relations/browse?kind=version must equal the
// number of chain cards the browse list would render, so the same
// AnyTainted filter that drops a card here drops it from the count.
func countVersionChains(cx *galleryCtx, ceiling *Ceiling) (int, error) {
	rows, err := cx.DB.Read.Query(`SELECT child_image_id, parent_image_id FROM version_edges`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	parentOf := map[int64]int64{} // child -> parent
	childOf := map[int64]int64{}  // parent -> child (UNIQUE per schema)
	for rows.Next() {
		var c, p int64
		if scanErr := rows.Scan(&c, &p); scanErr != nil {
			return 0, scanErr
		}
		parentOf[c] = p
		childOf[p] = c
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if _, err := ceiling.TaintedImageIDs(); err != nil {
		return 0, err
	}
	rootSet := map[int64]bool{}
	for _, p := range parentOf {
		if _, hasParent := parentOf[p]; !hasParent {
			rootSet[p] = true
		}
	}
	n := 0
	for root := range rootSet {
		members := []int64{root}
		cur := root
		for {
			next, ok := childOf[cur]
			if !ok {
				break
			}
			members = append(members, next)
			cur = next
		}
		if !ceiling.AnyTainted(members) {
			n++
		}
	}
	return n, nil
}

// countDerivativeTrees mirrors loadDerivativeTreeCards: each tree is
// walked from its root (a source that is not itself a derivative)
// and counted when no member exceeds the ceiling. The counter must
// equal the number of derivative-tree cards the browse list renders.
func countDerivativeTrees(cx *galleryCtx, ceiling *Ceiling) (int, error) {
	rows, err := cx.DB.Read.Query(`SELECT derivative_image_id, source_image_id FROM derivative_edges`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	derivativesOf := map[int64][]int64{}
	sourceOf := map[int64]int64{}
	for rows.Next() {
		var d, src int64
		if scanErr := rows.Scan(&d, &src); scanErr != nil {
			return 0, scanErr
		}
		derivativesOf[src] = append(derivativesOf[src], d)
		sourceOf[d] = src
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if _, err := ceiling.TaintedImageIDs(); err != nil {
		return 0, err
	}
	rootSet := map[int64]bool{}
	for src := range derivativesOf {
		if _, isDeriv := sourceOf[src]; !isDeriv {
			rootSet[src] = true
		}
	}
	n := 0
	for root := range rootSet {
		members := []int64{}
		collectDerivativeMembers(root, derivativesOf, &members)
		if !ceiling.AnyTainted(members) {
			n++
		}
	}
	return n, nil
}

// collectDerivativeMembers appends node + every descendant under it
// to members in DFS order. Mirrors the traversal dfsDerivativeTree
// uses for the card builder.
func collectDerivativeMembers(node int64, derivativesOf map[int64][]int64, members *[]int64) {
	*members = append(*members, node)
	for _, child := range derivativesOf[node] {
		collectDerivativeMembers(child, derivativesOf, members)
	}
}

// loadVersionChainCards reads every version_edge into memory, walks
// each chain from its root (a parent with no incoming edge) down to
// its leaf, and emits one card per chain. The flat Members slice is
// root-to-leaf so per-member ingest dates key into it; Generations is
// the same image ids regrouped one-per-depth so the template can
// render each generation as a row separated by a down-arrow. Chains
// whose member set carries any tag above the ceiling are dropped
// whole so a ceiling-hidden image never surfaces a sibling. The
// returned total counts every surviving chain regardless of limit so
// the caller can drive the matching counter without re-walking the
// edges.
func loadVersionChainCards(cx *galleryCtx, limit int, ceiling *Ceiling) ([]browseCard, int, error) {
	rows, err := cx.DB.Read.Query(`SELECT child_image_id, parent_image_id, created_at FROM version_edges`)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	type edgeMeta struct {
		parent    int64
		createdAt string
	}
	edges := map[int64]edgeMeta{} // child -> {parent, createdAt}
	childOf := map[int64]int64{}  // parent -> child (UNIQUE per schema)
	for rows.Next() {
		var c, p int64
		var ts string
		if scanErr := rows.Scan(&c, &p, &ts); scanErr != nil {
			return nil, 0, scanErr
		}
		edges[c] = edgeMeta{parent: p, createdAt: ts}
		childOf[p] = c
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	if _, err := ceiling.TaintedImageIDs(); err != nil {
		return nil, 0, err
	}
	rootSet := map[int64]bool{}
	for _, em := range edges {
		if _, hasParent := edges[em.parent]; !hasParent {
			rootSet[em.parent] = true
		}
	}
	roots := make([]int64, 0, len(rootSet))
	for r := range rootSet {
		roots = append(roots, r)
	}
	sort.Slice(roots, func(i, j int) bool { return roots[i] > roots[j] })
	cards := make([]browseCard, 0, len(roots))
	for _, root := range roots {
		members := []int64{root}
		generations := [][]int64{{root}}
		latestTS := ""
		cur := root
		for {
			next, ok := childOf[cur]
			if !ok {
				break
			}
			members = append(members, next)
			generations = append(generations, []int64{next})
			if em, ok := edges[next]; ok && em.createdAt > latestTS {
				latestTS = em.createdAt
			}
			cur = next
		}
		if ceiling.AnyTainted(members) {
			continue
		}
		cards = append(cards, browseCard{
			Kind:        "version",
			Members:     members,
			Generations: generations,
			CreatedAt:   humanISOTime(latestTS),
		})
	}
	total := len(cards)
	// Order by the newest edge in each chain, descending, so freshly
	// declared chains land at the top.
	sort.SliceStable(cards, func(i, j int) bool {
		return cards[i].CreatedAt > cards[j].CreatedAt
	})
	if limit > 0 && len(cards) > limit {
		cards = cards[:limit]
	}
	return cards, total, nil
}

// loadDerivativeTreeCards is the derivative-edge analogue of
// loadVersionChainCards. Each tree roots at a source that isn't itself
// a derivative; a depth-first walk from the root produces TreeRows
// (root first, then each subtree before its next sibling) so the
// template can indent each row by its depth and the branching is
// visible at a glance. Members keeps the same DFS order so per-member
// metadata maps key against it. Returns the total tree count (every
// surviving tree, regardless of limit) so the caller can drive the
// matching counter without re-walking the edges.
func loadDerivativeTreeCards(cx *galleryCtx, limit int, ceiling *Ceiling) ([]browseCard, int, error) {
	rows, err := cx.DB.Read.Query(`SELECT derivative_image_id, source_image_id, created_at FROM derivative_edges`)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	derivativesOf := map[int64][]int64{} // source -> derivatives (sorted by id ASC)
	sourceOf := map[int64]int64{}        // derivative -> source
	derivCreated := map[int64]string{}   // derivative -> edge's created_at
	for rows.Next() {
		var d, src int64
		var ts string
		if scanErr := rows.Scan(&d, &src, &ts); scanErr != nil {
			return nil, 0, scanErr
		}
		derivativesOf[src] = append(derivativesOf[src], d)
		sourceOf[d] = src
		derivCreated[d] = ts
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	for src := range derivativesOf {
		sort.Slice(derivativesOf[src], func(i, j int) bool {
			return derivativesOf[src][i] < derivativesOf[src][j]
		})
	}
	if _, err := ceiling.TaintedImageIDs(); err != nil {
		return nil, 0, err
	}
	rootSet := map[int64]bool{}
	for src := range derivativesOf {
		if _, isDeriv := sourceOf[src]; !isDeriv {
			rootSet[src] = true
		}
	}
	roots := make([]int64, 0, len(rootSet))
	for r := range rootSet {
		roots = append(roots, r)
	}
	sort.Slice(roots, func(i, j int) bool { return roots[i] > roots[j] })
	cards := make([]browseCard, 0, len(roots))
	for _, root := range roots {
		var members []int64
		var treeRows []treeRow
		latestTS := ""
		dfsDerivativeTree(root, 0, nil, true, derivativesOf, derivCreated, &members, &treeRows, &latestTS)
		if ceiling.AnyTainted(members) {
			continue
		}
		cards = append(cards, browseCard{
			Kind:      "derivative",
			Members:   members,
			TreeRows:  treeRows,
			CreatedAt: humanISOTime(latestTS),
		})
	}
	total := len(cards)
	sort.SliceStable(cards, func(i, j int) bool {
		return cards[i].CreatedAt > cards[j].CreatedAt
	})
	if limit > 0 && len(cards) > limit {
		cards = cards[:limit]
	}
	return cards, total, nil
}

// dfsDerivativeTree appends each tree node and its subtree to members
// and rows in depth-first order. Sibling order matches the caller's
// pre-sorted derivativesOf slice (ascending id) so the visual layout
// is stable across renders. ancestorTrunks is the prefix common to
// every descendant of this node (one entry per ancestor depth);
// isLast says whether this node is the last child of its parent so
// the connector is drawn as an elbow when true and a tee when false.
func dfsDerivativeTree(node int64, depth int, ancestorTrunks []string, isLast bool, derivativesOf map[int64][]int64, derivCreated map[int64]string, members *[]int64, rows *[]treeRow, latestTS *string) {
	*members = append(*members, node)
	*rows = append(*rows, treeRow{ID: node, Depth: depth, Trunks: rowTrunks(ancestorTrunks, depth, isLast)})
	if ts := derivCreated[node]; ts > *latestTS {
		*latestTS = ts
	}
	childAncestors := extendAncestorTrunks(ancestorTrunks, depth, isLast)
	children := derivativesOf[node]
	for i, child := range children {
		childIsLast := i == len(children)-1
		dfsDerivativeTree(child, depth+1, childAncestors, childIsLast, derivativesOf, derivCreated, members, rows, latestTS)
	}
}

// rowTrunks builds the per-row trunks slice the template renders. The
// root (depth 0) carries no trunks; every other row gets one entry per
// ancestor depth plus the tee / elbow connector.
func rowTrunks(ancestorTrunks []string, depth int, isLast bool) []string {
	if depth == 0 {
		return nil
	}
	out := make([]string, 0, depth)
	out = append(out, ancestorTrunks...)
	if isLast {
		out = append(out, "elbow")
	} else {
		out = append(out, "tee")
	}
	return out
}

// extendAncestorTrunks computes the ancestor-trunk slice each child of
// the current node sees: the existing ancestors plus a new segment at
// the current depth. The new segment is "line" when this node has more
// siblings below (its column continues past its children) and "empty"
// when this node is the last child (the column ends).
func extendAncestorTrunks(ancestorTrunks []string, depth int, isLast bool) []string {
	if depth == 0 {
		return nil
	}
	out := make([]string, 0, depth)
	out = append(out, ancestorTrunks...)
	if isLast {
		out = append(out, "empty")
	} else {
		out = append(out, "line")
	}
	return out
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

// validBrowseKinds is the closed vocabulary the /relations/browse page
// accepts as the ?kind= query parameter.
var validBrowseKinds = map[string]bool{
	"duplicate":   true,
	"alternate":   true,
	"version":     true,
	"derivative":  true,
	"not_related": true,
}

// browseSortsByKind whitelists the ?sort= values each kind tab accepts.
// The first entry in every slice is the default; anything off the list
// silently collapses to that default so a typo never executes against
// an interpolated SQL fragment.
var browseSortsByKind = map[string][]string{
	"duplicate":   {"recent", "size", "original_added"},
	"alternate":   {"recent", "size"},
	"version":     {"recent", "length", "newest_member"},
	"derivative":  {"recent", "size"},
	"not_related": {"recent"},
}

func resolveBrowseSort(kind, requested string) string {
	allowed := browseSortsByKind[kind]
	if len(allowed) == 0 {
		return "recent"
	}
	for _, s := range allowed {
		if s == requested {
			return s
		}
	}
	return allowed[0]
}

// browseRelationsPageSize caps each /relations/browse page; matches
// /tags's 100-row cap shape but tuned smaller because each card lifts
// a thumb strip whose vertical footprint is far denser.
const browseRelationsPageSize = 60

// browseRelationsPage renders /relations/browse with one tab per
// relation kind. The card layout adapts per kind: group cards lift a
// thumb strip plus dissolve / merge controls; edge cards render two
// thumbs with a directional arrow plus reverse / unlink; not-related
// rows render two thumbs plus unlink.
func (s *Server) browseRelationsPage(w http.ResponseWriter, r *http.Request) {
	cx := s.Active()
	if cx == nil || cx.DB == nil {
		http.Error(w, "no gallery", http.StatusServiceUnavailable)
		return
	}
	kind := r.URL.Query().Get("kind")
	if kind == "" {
		kind = "duplicate"
	}
	if !validBrowseKinds[kind] {
		http.NotFound(w, r)
		return
	}
	page := 1
	if p, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && p > 0 {
		page = p
	}
	sort := resolveBrowseSort(kind, r.URL.Query().Get("sort"))
	ceiling := resolveCeiling(r, cx)
	// Counts drive both the kind-tab labels and the page divisor.
	// Compute them first so a past-end ?page= can clamp to the last
	// valid page before the loader does its slice. For
	// version/derivative kinds the kind-total override lands after
	// the loader finishes its in-Go walk (the same walk drives the
	// card list, so the count rides the same data).
	counts := loadRelationsCounts(s, cx, ceiling, nil)
	total := kindTotal(counts, kind)
	totalPages := 1
	if total > 0 {
		totalPages = (total + browseRelationsPageSize - 1) / browseRelationsPageSize
	}
	if page > totalPages {
		page = totalPages
	}
	offset := (page - 1) * browseRelationsPageSize
	cards, walkedTotal, err := loadBrowseCardsByKind(cx, kind, sort, browseRelationsPageSize, offset, ceiling)
	if err != nil {
		logx.Warnf("browse cards %s: %v", kind, err)
		http.Error(w, "load cards", http.StatusInternalServerError)
		return
	}
	if walkedTotal > 0 {
		switch kind {
		case "version":
			counts.VersionChains = walkedTotal
		case "derivative":
			counts.DerivativeTrees = walkedTotal
		}
	}
	s.renderTemplate(w, "relations_browse.html", browseRelationsData{
		baseData:      s.base(r, "relations", "Browse relations - "+s.booruName()),
		ActiveGallery: s.activeName,
		Kind:          kind,
		Cards:         cards,
		Counts:        counts,
		Page:          page,
		TotalPages:    totalPages,
		Sort:          sort,
		SortOptions:   browseSortsByKind[kind],
	})
}

// kindTotal returns the per-kind count from a relationsCounts bundle.
func kindTotal(c relationsCounts, kind string) int {
	switch kind {
	case "duplicate":
		return c.DupGroups
	case "alternate":
		return c.AltGroups
	case "version":
		return c.VersionChains
	case "derivative":
		return c.DerivativeTrees
	case "not_related":
		return c.NotRelatedPairs
	}
	return 0
}

type browseRelationsData struct {
	baseData
	ActiveGallery string
	Kind          string
	Cards         []browseCard
	Counts        relationsCounts
	Page          int
	TotalPages    int
	Sort          string
	SortOptions   []string
}
