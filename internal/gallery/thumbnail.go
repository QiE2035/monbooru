package gallery

import (
	"bytes"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"

	"github.com/leqwin/monbooru/internal/logx"
)

const thumbMaxDim = 300
const thumbQuality = 85

// maxImagePixels caps the number of pixels in any image the decode
// path is willing to allocate the destination bitmap for. A 256-MPx
// PNG with random IHDR (e.g. 50000x50000) would otherwise demand
// ~1 GiB just for the RGBA buffer, OOM-killing the process at ingest
// or thumbnail regen. The cap is generous enough that no realistic
// camera or scanner output trips it; pathological synthetic headers
// are the only blocked input.
const maxImagePixels = 256 * 1000 * 1000

// DecodeImageWithCap is image.Decode gated on a megapixel ceiling.
// Runs image.DecodeConfig first to read just the header, refuses any
// image whose width*height exceeds maxImagePixels, then replays the
// header bytes alongside the rest of the stream so the full Decode
// works on non-seekable readers (zip page streams). Mirrors the
// stdlib signature minus the format-name return.
func DecodeImageWithCap(r io.Reader) (image.Image, error) {
	var buf bytes.Buffer
	tee := io.TeeReader(r, &buf)
	cfg, _, err := image.DecodeConfig(tee)
	if err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	if int64(cfg.Width)*int64(cfg.Height) > int64(maxImagePixels) {
		return nil, fmt.Errorf("image %dx%d exceeds %d-megapixel cap", cfg.Width, cfg.Height, maxImagePixels/1_000_000)
	}
	img, _, err := image.Decode(io.MultiReader(&buf, r))
	return img, err
}

func ThumbnailPath(dir string, imageID int64) string {
	return filepath.Join(dir, fmt.Sprintf("%d.jpg", imageID))
}

func HoverPath(dir string, imageID int64) string {
	return filepath.Join(dir, fmt.Sprintf("%d_hover.webp", imageID))
}

// Generate writes the static thumbnail (and animated hover for videos
// and GIFs when ffmpeg is available) for the given file under dstDir.
func Generate(srcPath, dstDir string, imageID int64, fileType string) error {
	if err := os.MkdirAll(dstDir, 0755); err != nil {
		return fmt.Errorf("creating thumbnail dir: %w", err)
	}

	dstPath := ThumbnailPath(dstDir, imageID)

	if IsVideoType(fileType) {
		if err := generateVideoThumb(srcPath, dstPath); err != nil {
			return err
		}
		hoverDst := HoverPath(dstDir, imageID)
		if err := generateVideoHover(srcPath, hoverDst); err != nil {
			logx.Warnf("hover preview for %q: %v", srcPath, err)
		}
		return nil
	}
	if fileType == "cbz" {
		return generateMangaThumbnails(srcPath, dstDir, imageID)
	}
	if err := generateImageThumb(srcPath, dstPath, fileType); err != nil {
		return err
	}
	if fileType == "gif" {
		hoverDst := HoverPath(dstDir, imageID)
		if err := generateGIFHover(srcPath, hoverDst); err != nil {
			logx.Warnf("hover preview for %q: %v", srcPath, err)
		}
	}
	return nil
}

// generateMangaThumbnails opens the archive once and writes the cover
// thumbnail (`<dstDir>/<id>.jpg`) plus a per-page thumbnail
// (`MangaImageDir/page_NNNN_thumb.jpg`) for every entry. Pre-generating
// the per-page set turns the first /pages render into a static-file
// serve. Page-thumb failures are logged at warn but do not fail the
// ingest: a missing entry falls back to the lazy EnsureMangaPageThumb
// path on next access.
func generateMangaThumbnails(srcPath, dstDir string, imageID int64) error {
	archive, err := OpenManga(srcPath)
	if err != nil {
		return fmt.Errorf("open manga thumb: %w", err)
	}
	defer func() { _ = archive.Close() }()

	cover, err := archive.CoverImage()
	if err != nil {
		return fmt.Errorf("decode manga cover: %w", err)
	}
	if err := writeJPEGAtomic(scaleImage(cover, thumbMaxDim), ThumbnailPath(dstDir, imageID), thumbQuality); err != nil {
		return err
	}

	imageDir := MangaImageDir(dstDir, imageID)
	if err := os.MkdirAll(imageDir, 0o755); err != nil {
		return fmt.Errorf("create manga thumb dir: %w", err)
	}
	for i := range archive.Pages {
		pageNum := i + 1
		thumbPath := MangaPageThumbPath(imageDir, pageNum)
		if err := generateOneMangaPageThumb(archive, i, thumbPath); err != nil {
			logx.Warnf("manga page thumb %d for %q: %v", pageNum, srcPath, err)
		}
	}
	return nil
}

// generateOneMangaPageThumb decodes one page directly from the archive
// (no raw-bytes cache write) and writes the thumbnail. Keeps the
// per-page footprint to one file on disk - the raw bytes stay lazy.
func generateOneMangaPageThumb(archive *Manga, idx int, dstPath string) error {
	rc, err := archive.PageReader(idx)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	src, err := DecodeImageWithCap(rc)
	if err != nil {
		return fmt.Errorf("decode page %d: %w", idx+1, err)
	}
	return writeJPEGAtomic(scaleImage(src, thumbMaxDim), dstPath, thumbQuality)
}

func generateImageThumb(srcPath, dstPath, fileType string) error {
	f, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("opening source: %w", err)
	}
	defer func() { _ = f.Close() }()

	var src image.Image

	if fileType == "gif" {
		g, err := gif.Decode(f)
		if err != nil {
			return fmt.Errorf("decoding gif: %w", err)
		}
		src = g
	} else {
		img, err := DecodeImageWithCap(f)
		if err != nil {
			return fmt.Errorf("decoding image: %w", err)
		}
		src = img
	}

	thumb := scaleImage(src, thumbMaxDim)
	return writeJPEGAtomic(thumb, dstPath, thumbQuality)
}

// scaleImage scales src so its longest side is at most maxDim.
func scaleImage(src image.Image, maxDim int) image.Image {
	bounds := src.Bounds()
	w, h := bounds.Dx(), bounds.Dy()

	if w <= maxDim && h <= maxDim {
		return src
	}

	var nw, nh int
	if w >= h {
		nw = maxDim
		nh = h * maxDim / w
	} else {
		nh = maxDim
		nw = w * maxDim / h
	}
	if nh == 0 {
		nh = 1
	}
	if nw == 0 {
		nw = 1
	}

	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	draw.BiLinear.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Over, nil)
	return dst
}

// writeJPEGAtomic encodes img as JPEG at path via a temp file + rename.
func writeJPEGAtomic(img image.Image, path string, quality int) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".thumb.*")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpName := tmp.Name()

	if err := jpeg.Encode(tmp, img, &jpeg.Options{Quality: quality}); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("encoding jpeg: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("renaming temp file: %w", err)
	}
	return nil
}
