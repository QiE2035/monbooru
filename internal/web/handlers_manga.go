package web

import (
	"context"
	"database/sql"
	"html/template"
	"net/http"
	"path/filepath"
	"strconv"

	"github.com/leqwin/monbooru/internal/db"
	"github.com/leqwin/monbooru/internal/logx"
	"github.com/leqwin/monbooru/internal/models"
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
	Image     models.Image
	Filename  string
	PageCount int
	ImageTags []models.ImageTag // populates the per-image sidebar tag list, mirroring detail.html
	Aliases   []models.Tag      // alias rows pointing at any non-implied tag on this image
	BackQuery string            // raw back_q used by the sidebar-browse render
	BackQS    template.URL
	BackKVQS  template.URL
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
	page := 1
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
		baseData:  s.base(r, "gallery", filepath.Base(img.CanonicalPath)+" - Pages - "+s.booruName()),
		Image:     *img,
		Filename:  filepath.Base(img.CanonicalPath),
		PageCount: *img.PageCount,
		ImageTags: imageTags,
		Aliases:   s.aliasesForImageTags(imageTags),
		BackQuery: back.Q,
		BackQS:    backQS,
		BackKVQS:  backKVQS,
	}
	s.renderTemplate(w, "pages.html", data)
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
	if count.Valid {
		v := int(count.Int64)
		m.Count = &v
	}
	if year.Valid {
		v := int(year.Int64)
		m.Year = &v
	}
	if month.Valid {
		v := int(month.Int64)
		m.Month = &v
	}
	if day.Valid {
		v := int(day.Int64)
		m.Day = &v
	}
	if xmlPageCount.Valid {
		v := int(xmlPageCount.Int64)
		m.XMLPageCount = &v
	}
	if communityRating.Valid {
		v := communityRating.Float64
		m.CommunityRating = &v
	}
	return &m
}

func nullToString(n sql.NullString) string {
	if n.Valid {
		return n.String
	}
	return ""
}
