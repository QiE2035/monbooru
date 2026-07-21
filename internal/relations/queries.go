package relations

import (
	"database/sql"

	"github.com/monbooru/monbooru/internal/db"
)

// ImageRelations is the per-image relations summary the detail panel
// and the /images/{id}/relations grid render from. Nil-valued fields
// mean "no relation of that kind"; the templates hide their section
// when the field is empty.
type ImageRelations struct {
	DupGroup         *DupGroupSummary
	AltGroupID       *int64
	AltGroupMembers  []int64
	VersionParent    *int64
	VersionChild     *int64
	DerivativeSource *int64
	Derivatives      []int64
}

// DupGroupSummary names a duplicate group plus the canonical original
// and every member id.
type DupGroupSummary struct {
	ID       int64
	Original int64
	Members  []int64
}

// HasAny reports whether the image carries at least one declared
// relation. Used by the templates to suppress the "no relations"
// stub on the detail-page panel.
func (r *ImageRelations) HasAny() bool {
	if r == nil {
		return false
	}
	if r.DupGroup != nil {
		return true
	}
	if len(r.AltGroupMembers) > 0 {
		return true
	}
	if r.VersionParent != nil || r.VersionChild != nil {
		return true
	}
	if r.DerivativeSource != nil || len(r.Derivatives) > 0 {
		return true
	}
	return false
}

// LoadImageRelations gathers every relation the image participates in.
// Each query rides a covering index; the whole load is sub-millisecond
// on a 1M-row library.
func LoadImageRelations(database *db.DB, imageID int64) (*ImageRelations, error) {
	out := &ImageRelations{}

	// Duplicate group.
	var dupGroupID sql.NullInt64
	if err := database.Read.QueryRow(
		`SELECT group_id FROM dup_group_members WHERE image_id = ?`, imageID,
	).Scan(&dupGroupID); err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	if dupGroupID.Valid {
		var dg DupGroupSummary
		dg.ID = dupGroupID.Int64
		if err := database.Read.QueryRow(
			`SELECT original_image_id FROM dup_groups WHERE id = ?`, dg.ID,
		).Scan(&dg.Original); err != nil {
			return nil, err
		}
		rows, err := database.Read.Query(
			`SELECT image_id FROM dup_group_members WHERE group_id = ? ORDER BY image_id`, dg.ID,
		)
		if err != nil {
			return nil, err
		}
		members, scanErr := db.ScanIDs(rows)
		_ = rows.Close()
		if scanErr != nil {
			return nil, scanErr
		}
		dg.Members = members
		out.DupGroup = &dg
	}

	// Alternate group.
	var altGroupID sql.NullInt64
	if err := database.Read.QueryRow(
		`SELECT group_id FROM alt_group_members WHERE image_id = ?`, imageID,
	).Scan(&altGroupID); err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	if altGroupID.Valid {
		out.AltGroupID = &altGroupID.Int64
		rows, err := database.Read.Query(
			`SELECT image_id FROM alt_group_members WHERE group_id = ? ORDER BY image_id`,
			altGroupID.Int64,
		)
		if err != nil {
			return nil, err
		}
		members, scanErr := db.ScanIDs(rows)
		_ = rows.Close()
		if scanErr != nil {
			return nil, scanErr
		}
		out.AltGroupMembers = members
	}

	// Version edges.
	var parentID sql.NullInt64
	if err := database.Read.QueryRow(
		`SELECT parent_image_id FROM version_edges WHERE child_image_id = ?`, imageID,
	).Scan(&parentID); err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	if parentID.Valid {
		out.VersionParent = &parentID.Int64
	}
	var childID sql.NullInt64
	if err := database.Read.QueryRow(
		`SELECT child_image_id FROM version_edges WHERE parent_image_id = ?`, imageID,
	).Scan(&childID); err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	if childID.Valid {
		out.VersionChild = &childID.Int64
	}

	// Derivative edges.
	var sourceID sql.NullInt64
	if err := database.Read.QueryRow(
		`SELECT source_image_id FROM derivative_edges WHERE derivative_image_id = ?`, imageID,
	).Scan(&sourceID); err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	if sourceID.Valid {
		out.DerivativeSource = &sourceID.Int64
	}
	rows, err := database.Read.Query(
		`SELECT derivative_image_id FROM derivative_edges WHERE source_image_id = ? ORDER BY derivative_image_id`, imageID,
	)
	if err != nil {
		return nil, err
	}
	derivatives, scanErr := db.ScanIDs(rows)
	_ = rows.Close()
	if scanErr != nil {
		return nil, scanErr
	}
	out.Derivatives = derivatives
	return out, nil
}
