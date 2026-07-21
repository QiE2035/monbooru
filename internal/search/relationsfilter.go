package search

import (
	"strconv"
	"strings"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/relations"
)

// relationPresence is the per-source existence answer used to
// short-circuit relation: predicates against an empty source. A nil
// db (test path) leaves every field false and the builder falls back
// to the regular EXISTS / NOT-IN shape.
type relationPresence struct {
	dup        bool
	alt        bool
	version    bool
	derivative bool
	series     bool
}

// buildPhashFilter handles the three phash: forms:
//
//   - bare `phash:` matches every image that has a phash (was once
//     ingested with a decodable thumbnail). The negation `-phash:`
//     is the IS NULL inverse.
//   - `phash:<16hex>` is an exact-equality lookup on idx_images_phash.
//   - `phash:<16hex>~<d>` matches every image within Hamming distance
//     d. When a BK-tree is wired for the gallery it answers the lookup
//     directly; otherwise the SQL popcount fallback scans phashes.
//
// Malformed input collapses to `1=0` so a typo never bleeds into the
// rest of the query as an empty-match.
func (b *whereBuilder) buildPhashFilter(e FilterExpr) string {
	val := strings.TrimSpace(e.Val)
	if val == "" {
		return "i.phash IS NOT NULL"
	}
	hexPart := val
	distance := -1
	if idx := strings.IndexByte(val, '~'); idx >= 0 {
		hexPart = val[:idx]
		d, err := strconv.Atoi(strings.TrimSpace(val[idx+1:]))
		if err != nil || d < 0 || d > 64 {
			return "1=0"
		}
		distance = d
	}
	hexPart = strings.TrimSpace(hexPart)
	if len(hexPart) != 16 {
		return "1=0"
	}
	u, err := strconv.ParseUint(hexPart, 16, 64)
	if err != nil {
		return "1=0"
	}
	phash := int64(u)
	if distance < 0 {
		b.args = append(b.args, phash)
		return "i.phash = ?"
	}
	if b.db != nil {
		if tree := relations.DefaultRegistry.Lookup(b.db); tree != nil {
			if err := tree.EnsureBuilt(b.db); err == nil {
				ids := tree.SearchWithinDistance(phash, distance)
				if len(ids) == 0 {
					return "1=0"
				}
				placeholders, idArgs := db.InPlaceholders(ids)
				b.args = append(b.args, idArgs...)
				return "i.id IN (" + placeholders + ")"
			}
		}
	}
	// Fallback: SQL-side hammingdist scalar. Slower than the BK-tree on
	// a 1M-row library but correct.
	b.args = append(b.args, phash, distance)
	return "(i.phash IS NOT NULL AND hammingdist(i.phash, ?) <= ?)"
}

// buildRelationFilter maps each closed `relation:` vocabulary value to
// an EXISTS / NOT-EXISTS over the matching relations table; every
// clause rides a covering index, so cost is dominated by the outer
// sort. `any` is the union, `none` is the NOT-any inverse.
//
// resolveRelationPresence is consulted first so a gallery with no
// declared relations skips the per-row EXISTS / NOT IN entirely: an
// empty source means the predicate's answer is constant across every
// visible row, and we emit `1=0` (positive sense) or `1=1` (NOT) up
// front. The cold relation:none scan on a 1M-row visible set used to
// land in seconds; here it folds into the outer sort with no
// per-row work.
func (b *whereBuilder) buildRelationFilter(e FilterExpr) string {
	val := strings.ToLower(strings.TrimSpace(e.Val))
	b.resolveRelationPresence()
	switch val {
	case "duplicate":
		if !b.relPresence.dup {
			return "1=0"
		}
		return "EXISTS (SELECT 1 FROM dup_group_members m WHERE m.image_id = i.id)"
	case "original":
		if !b.relPresence.dup {
			return "1=0"
		}
		return "EXISTS (SELECT 1 FROM dup_groups g WHERE g.original_image_id = i.id)"
	case "alternate":
		if !b.relPresence.alt {
			return "1=0"
		}
		return "EXISTS (SELECT 1 FROM alt_group_members m WHERE m.image_id = i.id)"
	case "version":
		if !b.relPresence.version {
			return "1=0"
		}
		return "EXISTS (SELECT 1 FROM version_edges v WHERE v.child_image_id = i.id OR v.parent_image_id = i.id)"
	case "derivative":
		if !b.relPresence.derivative {
			return "1=0"
		}
		return "EXISTS (SELECT 1 FROM derivative_edges d WHERE d.derivative_image_id = i.id)"
	case "source":
		if !b.relPresence.derivative {
			return "1=0"
		}
		return "EXISTS (SELECT 1 FROM derivative_edges d WHERE d.source_image_id = i.id)"
	case "collection":
		if !b.relPresence.series {
			return "1=0"
		}
		return "i.series IS NOT NULL AND i.series != ''"
	case "any":
		if !b.anyRelationPresent() {
			return "1=0"
		}
		return b.relationAnyClauseForPresence()
	case "none":
		if !b.anyRelationPresent() {
			return "1=1"
		}
		return b.relationNoneClauseForPresence()
	}
	return "1=0"
}

// resolveRelationPresence runs a single COUNT(EXISTS) probe per
// relations source (5 cheap lookups, each <0.1 ms on a warm DB) and
// caches the booleans on the builder. Skipped on a nil db (test path)
// so the regular EXISTS / NOT IN shapes still surface there.
func (b *whereBuilder) resolveRelationPresence() {
	if b.relPresenceResolved {
		return
	}
	b.relPresenceResolved = true
	if b.db == nil {
		return
	}
	probe := func(query string) bool {
		var has int
		if err := b.db.Read.QueryRow(query).Scan(&has); err != nil {
			// Treat probe failure as "may have rows" so the slow but
			// correct path runs - errors here only surface in degraded
			// modes, not on the empty-table fast path this resolver
			// targets.
			return true
		}
		return has > 0
	}
	b.relPresence = relationPresence{
		dup:        probe(`SELECT EXISTS (SELECT 1 FROM dup_group_members)`),
		alt:        probe(`SELECT EXISTS (SELECT 1 FROM alt_group_members)`),
		version:    probe(`SELECT EXISTS (SELECT 1 FROM version_edges)`),
		derivative: probe(`SELECT EXISTS (SELECT 1 FROM derivative_edges)`),
		series:     probe(`SELECT EXISTS (SELECT 1 FROM images WHERE series IS NOT NULL AND series != '' LIMIT 1)`),
	}
}

func (b *whereBuilder) anyRelationPresent() bool {
	p := &b.relPresence
	return p.dup || p.alt || p.version || p.derivative || p.series
}

// relationAnyClauseForPresence returns the union with each subquery
// dropped when its source is known to be empty. Strict-empty branches
// can't contribute matches, so trimming them yields a cheaper plan
// (often a single EXISTS or a series-IS-NOT-NULL leg).
func (b *whereBuilder) relationAnyClauseForPresence() string {
	p := &b.relPresence
	parts := make([]string, 0, 5)
	if p.dup {
		parts = append(parts,
			"EXISTS (SELECT 1 FROM dup_group_members m WHERE m.image_id = i.id)",
		)
	}
	if p.alt {
		parts = append(parts,
			"EXISTS (SELECT 1 FROM alt_group_members m WHERE m.image_id = i.id)",
		)
	}
	if p.version {
		parts = append(parts,
			"EXISTS (SELECT 1 FROM version_edges v WHERE v.child_image_id = i.id OR v.parent_image_id = i.id)",
		)
	}
	if p.derivative {
		parts = append(parts,
			"EXISTS (SELECT 1 FROM derivative_edges d WHERE d.derivative_image_id = i.id OR d.source_image_id = i.id)",
		)
	}
	if p.series {
		parts = append(parts, "(i.series IS NOT NULL AND i.series != '')")
	}
	if len(parts) == 0 {
		return "1=0"
	}
	return strings.Join(parts, " OR ")
}

// relationNoneClauseForPresence rewrites the NOT IN (UNION ...) so
// only the populated relation sources contribute. An empty union
// would walk every visible row checking against nothing; dropping
// the empty branches keeps the materialised set tight to whatever
// the operator actually has. Series carriers stay on the AND-leg
// per the original shape.
func (b *whereBuilder) relationNoneClauseForPresence() string {
	p := &b.relPresence
	var unions []string
	if p.dup {
		unions = append(unions, "SELECT image_id FROM dup_group_members")
	}
	if p.alt {
		unions = append(unions, "SELECT image_id FROM alt_group_members")
	}
	if p.version {
		unions = append(unions,
			"SELECT child_image_id FROM version_edges",
			"SELECT parent_image_id FROM version_edges",
		)
	}
	if p.derivative {
		unions = append(unions,
			"SELECT derivative_image_id FROM derivative_edges",
			"SELECT source_image_id FROM derivative_edges",
		)
	}
	if !p.series && len(unions) == 0 {
		return "1=1"
	}
	// Fold the series-empty check into the NOT IN so SQLite uses the
	// partial idx_images_series to materialise the carriers cheaply
	// (the index covers WHERE series != ''). The bare LIKE on series
	// would otherwise force a per-row column read on every visible
	// image even when zero carriers exist.
	if p.series {
		unions = append(unions, "SELECT id FROM images WHERE series IS NOT NULL AND series != ''")
	}
	if len(unions) == 0 {
		return "1=1"
	}
	return "i.id NOT IN (\n\t\t" + strings.Join(unions, "\n\t\tUNION ") + "\n\t)"
}
