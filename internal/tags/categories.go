package tags

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/leqwin/monbooru/internal/db"
	"github.com/leqwin/monbooru/internal/models"
)

// Tag-category vocabulary: listing, create/rename/recolor, and the
// move-or-delete teardown.

func (s *Service) ListCategories() ([]models.TagCategory, error) {
	rows, err := s.db.Read.Query(
		`SELECT id, name, color, is_builtin FROM tag_categories ORDER BY id`,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

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
	if !categoryNameRe.MatchString(name) {
		return nil, ErrInvalidCategoryName
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
		if isUniqueConstraintErr(err) {
			return nil, ErrCategoryExists
		}
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
	if !categoryNameRe.MatchString(newName) {
		return ErrInvalidCategoryName
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
		return ErrBuiltinCategoryName
	}
	_, err := s.db.Write.Exec(
		`UPDATE tag_categories SET name = ? WHERE id = ?`, newName, id,
	)
	if err != nil && isUniqueConstraintErr(err) {
		return ErrCategoryExists
	}
	return err
}

// isUniqueConstraintErr reports whether err is the SQLite UNIQUE
// constraint violation (raw error code 2067). Detecting it via the
// stringified message lets the handlers map a clean "name already
// exists" to the user without exposing the column or the error code.
func isUniqueConstraintErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
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
	var closure []int64
	err := s.inWriteTx(func(tx *sql.Tx) error {
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
			tagIDs, scanErr := db.ScanIDs(rows)
			_ = rows.Close()
			if scanErr != nil {
				return scanErr
			}
			// Route through the same closure sweep DeleteTag uses so an implied
			// child in a surviving category isn't orphaned when its only parent
			// here is deleted.
			closure, err = deleteTagsTx(tx, tagIDs)
			if err != nil {
				return err
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
		return nil
	})
	if err != nil {
		return err
	}
	if len(closure) > 0 {
		return s.RecalcIDs(closure)
	}
	return nil
}
