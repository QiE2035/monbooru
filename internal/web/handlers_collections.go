package web

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/leqwin/monbooru/internal/db"
	"github.com/leqwin/monbooru/internal/gallery"
	"github.com/leqwin/monbooru/internal/models"
)

const collectionsPerPage = 60

// Preview tiles fetched per collection. Generous so the strip fills a wide
// row; the template clips the overflow to a single line.
const collectionPreviewSamples = 16

type collectionsPageData struct {
	baseData
	Collections []gallery.CollectionSummary
	Total       int
	Page        int
	TotalPages  int
	Prefix      string
	Sort        string
}

func (s *Server) collectionsHandler(w http.ResponseWriter, r *http.Request) {
	// Rename / dissolve mutate the listing; opt out of caching so a reload
	// after a job never serves a stale render.
	w.Header().Set("Cache-Control", "no-store")
	q := r.URL.Query()
	prefix := strings.TrimSpace(q.Get("q"))
	sortStr := q.Get("sort")
	if sortStr != "name" {
		sortStr = "size"
	}
	page := 1
	if p, err := strconv.Atoi(q.Get("page")); err == nil && p > 0 {
		page = p
	}
	excludeIDs := resolveCeiling(r, s.Active()).ExcludedTagIDs()

	total, err := gallery.CountCollections(s.db(), prefix, excludeIDs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	totalPages := (total + collectionsPerPage - 1) / collectionsPerPage
	if total > 0 && page > totalPages {
		page = totalPages
	}

	list, err := gallery.ListCollections(s.db(), prefix, sortStr, collectionsPerPage, (page-1)*collectionsPerPage, excludeIDs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	names := make([]string, len(list))
	for i := range list {
		names[i] = list[i].Name
	}
	samples, err := gallery.CollectionSamples(s.db(), names, collectionPreviewSamples, excludeIDs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for i := range list {
		list[i].Samples = samples[strings.ToLower(list[i].Name)]
	}

	s.renderTemplate(w, "collections.html", collectionsPageData{
		baseData:    s.base(r, "collections", "Collections - "+s.booruName()),
		Collections: list,
		Total:       total,
		Page:        page,
		TotalPages:  totalPages,
		Prefix:      prefix,
		Sort:        sortStr,
	})
}

// renameCollectionPost relabels a collection across every member as a
// background tag job. Renaming onto an existing label merges the two.
func (s *Server) renameCollectionPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	oldName := strings.TrimSpace(r.FormValue("prev"))
	newName := strings.TrimSpace(r.FormValue("name"))
	if oldName == "" || newName == "" {
		w.WriteHeader(http.StatusBadRequest)
		writeInlineFlash(w, "err", "Both the current and the new collection name are required.")
		return
	}
	if len(newName) > maxExternalSourceLen {
		w.WriteHeader(http.StatusBadRequest)
		writeInlineFlash(w, "err", "Collection label too long.")
		return
	}
	if oldName == newName {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	ids, err := gallery.CollectionMemberIDs(s.db(), oldName)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		writeInlineFlash(w, "err", "Could not read the collection.")
		return
	}
	if len(ids) == 0 {
		w.WriteHeader(http.StatusBadRequest)
		writeInlineFlash(w, "err", "That collection no longer exists.")
		return
	}
	if !s.startJob(w, models.JobTypeTag) {
		return
	}
	go s.runRenameCollection(ids, oldName, newName)
	w.WriteHeader(http.StatusAccepted)
}

// dissolveCollectionPost drops a collection label from every member as a
// background tag job. Images and files are left untouched; it reuses the
// batch-collection remove path over the full membership.
func (s *Server) dissolveCollectionPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	name := strings.TrimSpace(r.FormValue("collection"))
	if name == "" {
		w.WriteHeader(http.StatusBadRequest)
		writeInlineFlash(w, "err", "Collection label required.")
		return
	}
	ids, err := gallery.CollectionMemberIDs(s.db(), name)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		writeInlineFlash(w, "err", "Could not read the collection.")
		return
	}
	if len(ids) == 0 {
		w.WriteHeader(http.StatusBadRequest)
		writeInlineFlash(w, "err", "That collection no longer exists.")
		return
	}
	if !s.startJob(w, models.JobTypeTag) {
		return
	}
	go s.runBatchCollection(ids, name, "remove")
	w.WriteHeader(http.StatusAccepted)
}

// runRenameCollection relabels old to new across ids in chunks. An image
// already holding new keeps its existing membership (the old one is
// dropped so the relabel can't collide on the (image_id, name) key); the
// home mirror follows for rows homed on the old label.
func (s *Server) runRenameCollection(ids []int64, oldName, newName string) {
	ctx := s.jobs.Context()
	const chunkSize = 500
	total := len(ids)
	// A case-only rename ("New" -> "new") targets the same NOCASE label, so
	// there is nothing to merge; the collision delete below would otherwise
	// drop the very rows the relabel is meant to recase.
	merging := !strings.EqualFold(oldName, newName)

	processed, cancelled, err := chunkedJob(ctx, s.jobs, ids, chunkSize, "renaming collection", func(chunk []int64) error {
		placeholders, chunkArgs := db.InPlaceholders(chunk)
		tx, err := s.db().Write.Begin()
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if merging {
			if _, err := tx.Exec(
				`DELETE FROM image_collections
				 WHERE name = ? AND image_id IN (`+placeholders+`)
				   AND image_id IN (SELECT image_id FROM image_collections WHERE name = ?)`,
				append(append([]any{oldName}, chunkArgs...), newName)...,
			); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(
			`UPDATE image_collections SET name = ? WHERE name = ? AND image_id IN (`+placeholders+`)`,
			append([]any{newName, oldName}, chunkArgs...)...,
		); err != nil {
			return err
		}
		if _, err := tx.Exec(
			`UPDATE images SET series = ? WHERE series = ? COLLATE NOCASE AND id IN (`+placeholders+`)`,
			append([]any{newName, oldName}, chunkArgs...)...,
		); err != nil {
			return err
		}
		// Sync the home position to whichever membership survived (the
		// pre-existing target's position wins on a merge).
		if _, err := tx.Exec(
			`UPDATE images SET series_order =
			   (SELECT position FROM image_collections WHERE image_id = images.id AND name = ?)
			 WHERE series = ? COLLATE NOCASE AND id IN (`+placeholders+`)`,
			append([]any{newName, newName}, chunkArgs...)...,
		); err != nil {
			return err
		}
		return tx.Commit()
	})
	if err != nil {
		s.jobs.Fail(err.Error())
		return
	}
	s.Active().InvalidateCaches()
	if cancelled {
		s.jobs.Complete(fmt.Sprintf("rename cancelled (%d/%d processed)", processed, total))
		return
	}
	s.jobs.Complete(fmt.Sprintf("Renamed collection across %d image(s).", processed))
}
