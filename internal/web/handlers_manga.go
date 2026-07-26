package web

import (
	"context"
	"database/sql"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/models"
)

// readerData drives reader.html. PageCount is the authoritative count;
// Page is the 1-based current page (server-clamped). BackQS / BackKVQS
// are pre-rendered query-string fragments for the reader's links: the
// first carries `?back_*=...&from=pages` (used on the Back-to-detail
// link), the second carries `&back_*=...&from=pages` (used on
// prev/next links that already have a `?page=` prefix). Both are
// template.URL so html/template treats the `&` separators as already
// encoded and emits them verbatim into the URL attribute.
type readerData struct {
	baseData
	Image       models.Image
	Filename    string
	Page        int
	PageCount   int
	NextPage    int          // 0 when on the last page; drives the prefetch link
	BackQS      template.URL // "?back_q=...&..." or ""; safe to append to a path with no query
	BackKVQS    template.URL // "&back_q=...&..." or ""; safe to append after `?page=N`
	BackToPages bool         // true when the reader was opened from /images/{id}/pages; flips the back-link target to that page
}

// pagesGridData drives pages.html.
type pagesGridData struct {
	baseData
	Image        models.Image
	Filename     string
	PageCount    int
	LastReadPage int               // resume bookmark, 0 when unstarted or finished
	ImageTags    []models.ImageTag // populates the per-image sidebar tag list, mirroring detail.html
	Aliases      []models.Tag      // alias rows pointing at any non-implied tag on this image
	BackQuery    string            // raw back_q used by the sidebar-browse render
	BackQS       template.URL
	BackKVQS     template.URL
}

// resumePage returns the reader bookmark clamped to the row's current
// page count, or 0 when there is nothing to resume. A re-ingested
// archive can shrink, so a stored value is never trusted; landing on the
// first or last page means unstarted or finished either way.
func resumePage(img *models.Image) int {
	if img.LastReadPage == nil || img.PageCount == nil {
		return 0
	}
	page := *img.LastReadPage
	if page <= 1 || page >= *img.PageCount {
		return 0
	}
	return page
}

// readerHandler serves /images/{id}/read?page=N. Validates the row is
// a manga, clamps page to [1, page_count], and renders the reader
// template.
// loadMangaImage parses {id}, loads the image, and 404s unless it is a
// readable cbz. Returns ok=false (the 404 is already written) otherwise.
func (s *Server) loadMangaImage(w http.ResponseWriter, r *http.Request) (*models.Image, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFoundHandler(w, r)
		return nil, false
	}
	img, err := loadImage(r.Context(), s.db(), id)
	if err != nil || img.FileType != models.FileTypeCBZ || img.PageCount == nil || *img.PageCount < 1 {
		s.notFoundHandler(w, r)
		return nil, false
	}
	return img, true
}

func (s *Server) readerHandler(w http.ResponseWriter, r *http.Request) {
	img, ok := s.loadMangaImage(w, r)
	if !ok {
		return
	}
	pageCount := *img.PageCount
	// No `page` at all means "open the book", so land on the bookmark
	// rather than page 1 - rendering page 1 is what clears it, and a
	// browser bookmark of the bare URL would wipe the resume position
	// every visit. An explicit `?page=1` still clears, as documented.
	page := 1
	if resume := resumePage(img); resume > 0 {
		page = resume
	}
	rawPage := ""
	if v := r.URL.Query().Get("page"); v != "" {
		rawPage = v
		if n, err := strconv.Atoi(v); err == nil {
			page = n
		}
	}
	if page < 1 {
		page = 1
	}
	if page > pageCount {
		page = pageCount
	}
	// URL coherence with the gallery's pagination clamp: when the raw
	// `?page=N` disagrees with the clamped page (out of range, leading
	// zero, etc.), 303 to the clamped value so a bookmark of the bogus
	// URL doesn't keep replaying it.
	if rawPage != "" && rawPage != strconv.Itoa(page) {
		q := r.URL.Query()
		q.Set("page", strconv.Itoa(page))
		http.Redirect(w, r, r.URL.Path+"?"+q.Encode(), http.StatusSeeOther)
		return
	}
	s.recordReaderPosition(img, page, pageCount)
	next := 0
	if page < pageCount {
		next = page + 1
	}
	back := parseBackContext(r)
	backToPages := r.URL.Query().Get("from") == "pages"
	backQS, backKVQS := back.ReaderQS(backToPages)

	data := readerData{
		baseData:    s.base(r, "gallery", filepath.Base(img.CanonicalPath)+" - Reader - "+s.booruName()),
		Image:       *img,
		Filename:    filepath.Base(img.CanonicalPath),
		Page:        page,
		PageCount:   pageCount,
		NextPage:    next,
		BackQS:      backQS,
		BackKVQS:    backKVQS,
		BackToPages: backToPages,
	}
	s.renderTemplate(w, "reader.html", data)
}

// recordReaderPosition moves the resume bookmark to the page just
// rendered. Page 1 is the default entry point and the last page means
// the book is finished, so both clear it. Writing from a GET is fine
// here: the reader prefetches page bytes, not the render, so this only
// runs on real navigation, and the clamp redirect fires first so only
// canonical URLs reach it.
func (s *Server) recordReaderPosition(img *models.Image, page, pageCount int) {
	next := 0
	if page > 1 && page < pageCount {
		next = page
	}
	stored := 0
	if img.LastReadPage != nil {
		stored = *img.LastReadPage
	}
	if next == stored {
		return
	}
	var value any
	if next > 0 {
		value = next
	}
	if _, err := s.db().Write.Exec(
		`UPDATE images SET last_read_page = ? WHERE id = ?`, value, img.ID,
	); err != nil {
		logx.Warnf("record reader position for image %d: %v", img.ID, err)
	}
}

// pagesGridHandler serves /images/{id}/pages. Renders a thumbnail grid
// of every page; clicking a cell opens the reader at that page.
func (s *Server) pagesGridHandler(w http.ResponseWriter, r *http.Request) {
	img, ok := s.loadMangaImage(w, r)
	if !ok {
		return
	}
	back := parseBackContext(r)
	// Pages grid never opens the reader as a from=pages context for
	// itself; the back link from the grid lands on the detail page,
	// not back on the grid.
	backQS, backKVQS := back.ReaderQS(false)
	_, imageTags, _ := s.tagSvc().GetImageTags(img.ID)
	data := pagesGridData{
		baseData:     s.base(r, "gallery", filepath.Base(img.CanonicalPath)+" - Pages - "+s.booruName()),
		Image:        *img,
		Filename:     filepath.Base(img.CanonicalPath),
		PageCount:    *img.PageCount,
		LastReadPage: resumePage(img),
		ImageTags:    imageTags,
		Aliases:      s.aliasesForImageTags(imageTags),
		BackQuery:    back.Q,
		BackQS:       backQS,
		BackKVQS:     backKVQS,
	}
	s.renderTemplate(w, "pages.html", data)
}

// extractMangaPage saves the n-th page of a cbz as its own image row in
// the active gallery, linked back to the archive by a derivative edge.
// Extracting the same page twice lands on the row already holding those
// bytes, so the reader's button needs no "already extracted" state.
func (s *Server) extractMangaPage(w http.ResponseWriter, r *http.Request) {
	img, ok := s.loadMangaImage(w, r)
	if !ok {
		return
	}
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || n < 1 || n > *img.PageCount {
		s.notFoundHandler(w, r)
		return
	}
	fail := func(stage string, err error) {
		logx.Warnf("extract page %d of image %d: %s: %v", n, img.ID, stage, err)
		http.Error(w, "Could not extract this page.", http.StatusInternalServerError)
	}

	pagePath, err := gallery.EnsureMangaPage(s.thumbnailsPath(), img.CanonicalPath, img.ID, n)
	if err != nil {
		fail("extract", err)
		return
	}
	destDir, err := gallery.ResolveSubdir(s.galleryPath(), s.cfg.Gallery.DefaultUploadFolder)
	if err != nil {
		fail("resolve folder", err)
		return
	}
	if err := os.MkdirAll(destDir, 0755); err != nil {
		fail("create folder", err)
		return
	}
	stem := strings.TrimSuffix(filepath.Base(img.CanonicalPath), filepath.Ext(img.CanonicalPath))
	// Copy rather than move: the cache file belongs to the manga reclaim
	// goroutine, which is free to unlink it at any point.
	dstPath := gallery.UniqueDestPath(destDir, fmt.Sprintf("%s_p%04d%s", stem, n, filepath.Ext(pagePath)))
	if err := copyFileContents(pagePath, dstPath); err != nil {
		fail("copy page", err)
		return
	}
	ft, err := gallery.DetectFileType(dstPath)
	if err != nil {
		_ = os.Remove(dstPath)
		fail("detect type", err)
		return
	}
	// No MaxFileSizeMB check: the bytes are already in the library, inside
	// the archive.
	page, isDup, err := gallery.Ingest(s.db(), s.galleryPath(), s.thumbnailsPath(), dstPath, ft, models.OriginExtract)
	if err != nil {
		_ = os.Remove(dstPath)
		fail("ingest", err)
		return
	}
	if isDup {
		// Same unwind as the upload drop zone: the bytes already live in
		// the gallery, so the fresh copy and the alias ingest recorded for
		// it are dead weight.
		if _, delErr := s.db().Write.Exec(
			`DELETE FROM image_paths WHERE image_id = ? AND path = ? AND is_canonical = 0`,
			page.ID, dstPath,
		); delErr != nil {
			logx.Warnf("extract: drop duplicate alias for %q: %v", dstPath, delErr)
		}
		if rmErr := os.Remove(dstPath); rmErr != nil && !os.IsNotExist(rmErr) {
			logx.Warnf("extract: remove duplicate copy %q: %v", dstPath, rmErr)
		}
	}
	if cx := s.Active(); cx != nil {
		if cx.RelationsSvc != nil {
			// A conflicting relation or an existing source on the page row
			// is a standing operator decision, so the link is skipped and
			// the extract still stands.
			if err := cx.RelationsSvc.AddDerivativeEdge(img.ID, page.ID); err != nil {
				logx.Debugf("extract: link %d -> %d skipped: %v", img.ID, page.ID, err)
			}
		}
		cx.InvalidateCaches()
	}
	http.Redirect(w, r, fmt.Sprintf("/images/%d", page.ID), http.StatusSeeOther)
}

// loadMangaMeta reads the manga_metadata row for an image, or nil when
// absent. Errors other than ErrNoRows are logged at debug since the
// detail page degrades cleanly when the row is missing.
func loadMangaMeta(ctx context.Context, database *db.DB, imageID int64) *models.MangaMetadata {
	var m models.MangaMetadata
	var title, series, number, volume, summary, notes sql.NullString
	var writer, penciller, inker, colorist, letterer sql.NullString
	var coverArtist, editor, publisher, imprint, genre sql.NullString
	var web, languageISO, format, manga, ageRating sql.NullString
	var rawXML sql.NullString
	var count, year, month, day, xmlPageCount sql.NullInt64
	var communityRating sql.NullFloat64
	err := database.Read.QueryRowContext(ctx, `
		SELECT image_id, title, series, number, volume, count, summary, notes,
		       year, month, day, writer, penciller, inker, colorist, letterer, cover_artist, editor, publisher,
		       imprint, genre, web, language_iso, format, manga, age_rating, community_rating, xml_page_count, raw_xml
		FROM manga_metadata WHERE image_id = ?`, imageID,
	).Scan(&m.ImageID, &title, &series, &number, &volume, &count, &summary, &notes,
		&year, &month, &day, &writer, &penciller, &inker, &colorist, &letterer, &coverArtist, &editor, &publisher,
		&imprint, &genre, &web, &languageISO, &format, &manga, &ageRating, &communityRating, &xmlPageCount, &rawXML)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		logx.Debugf("loadMangaMeta: %v", err)
		return nil
	}
	m.Title = nullToString(title)
	m.Series = nullToString(series)
	m.Number = nullToString(number)
	m.Volume = nullToString(volume)
	m.Summary = nullToString(summary)
	m.Notes = nullToString(notes)
	m.Writer = nullToString(writer)
	m.Penciller = nullToString(penciller)
	m.Inker = nullToString(inker)
	m.Colorist = nullToString(colorist)
	m.Letterer = nullToString(letterer)
	m.CoverArtist = nullToString(coverArtist)
	m.Editor = nullToString(editor)
	m.Publisher = nullToString(publisher)
	m.Imprint = nullToString(imprint)
	m.Genre = nullToString(genre)
	m.Web = nullToString(web)
	m.LanguageISO = nullToString(languageISO)
	m.Format = nullToString(format)
	m.Manga = nullToString(manga)
	m.AgeRating = nullToString(ageRating)
	m.RawXML = nullToString(rawXML)
	m.Count = nullToIntPtr(count)
	m.Year = nullToIntPtr(year)
	m.Month = nullToIntPtr(month)
	m.Day = nullToIntPtr(day)
	m.XMLPageCount = nullToIntPtr(xmlPageCount)
	m.CommunityRating = nullToFloatPtr(communityRating)
	return &m
}

func nullToString(n sql.NullString) string {
	if n.Valid {
		return n.String
	}
	return ""
}

func nullToIntPtr(n sql.NullInt64) *int {
	if !n.Valid {
		return nil
	}
	v := int(n.Int64)
	return &v
}

func nullToFloatPtr(n sql.NullFloat64) *float64 {
	if !n.Valid {
		return nil
	}
	v := n.Float64
	return &v
}
