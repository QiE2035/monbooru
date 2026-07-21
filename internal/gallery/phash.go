package gallery

import (
	"context"
	"fmt"
	"image"
	"image/jpeg"
	"math"
	"os"
	"sort"

	"github.com/monbooru/monbooru/internal/db"
)

// phashSize is the input-matrix side length (32x32 greyscale) and
// phashBlock is the top-left DCT block side (8x8) that the bit-pack
// reads. Both are the standard pHash settings.
const (
	phashSize  = 32
	phashBlock = 8
)

// ComputePhashFromThumb opens the static thumbnail JPEG at thumbPath
// and returns its canonicalised 64-bit perceptual hash as a signed
// int64 (SQLite's INTEGER affinity). Returns a non-nil error when the
// file is missing or undecodable; the caller leaves images.phash NULL
// in that case.
//
// The thumbnail is the uniform input across every visual file_type:
// jpeg / png / webp / gif use their static thumbnail directly, mp4 /
// webm use the 10%-of-duration frame the existing pipeline writes,
// and cbz uses the cover thumbnail. Hashing the thumbnail (not the
// original) keeps the hashed pixels identical to what the operator
// sees on the gallery grid.
func ComputePhashFromThumb(thumbPath string) (int64, error) {
	f, err := os.Open(thumbPath)
	if err != nil {
		return 0, fmt.Errorf("open thumb: %w", err)
	}
	defer func() { _ = f.Close() }()
	img, err := jpeg.Decode(f)
	if err != nil {
		return 0, fmt.Errorf("decode thumb: %w", err)
	}
	return int64(computePhash(img)), nil
}

// computePhash converts img into a 32x32 greyscale matrix via box-
// average downsample, runs a 2D DCT-II, takes the top-left 8x8 block,
// and emits one bit per non-DC coefficient (1 when the coefficient is
// at least the median of the 63 non-DC values, else 0). Bit 0 maps to
// the DC slot and is forced to 0 so the encoding is canonical. The
// returned value is min(h(img), h(mirror(img))) so a horizontally-
// flipped copy lands at the same value.
func computePhash(img image.Image) uint64 {
	mat := greyResize32(img)
	h := dctHashMatrix(mat)
	var mirror [phashSize][phashSize]float64
	for r := 0; r < phashSize; r++ {
		for c := 0; c < phashSize; c++ {
			mirror[r][c] = mat[r][phashSize-1-c]
		}
	}
	if hm := dctHashMatrix(mirror); hm < h {
		return hm
	}
	return h
}

// greyResize32 reads img into a 32x32 greyscale matrix using a box
// average over each destination cell's source area. Rec. 601 luma
// coefficients on 8-bit-per-channel samples; the source can be any
// stdlib-decodable image.
func greyResize32(img image.Image) [phashSize][phashSize]float64 {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	var out [phashSize][phashSize]float64
	if w == 0 || h == 0 {
		return out
	}
	for dy := 0; dy < phashSize; dy++ {
		y0 := dy * h / phashSize
		y1 := (dy + 1) * h / phashSize
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for dx := 0; dx < phashSize; dx++ {
			x0 := dx * w / phashSize
			x1 := (dx + 1) * w / phashSize
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var sum float64
			var n int
			for y := y0; y < y1; y++ {
				for x := x0; x < x1; x++ {
					r, g, bl, _ := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
					sum += 0.299*float64(r>>8) + 0.587*float64(g>>8) + 0.114*float64(bl>>8)
					n++
				}
			}
			out[dy][dx] = sum / float64(n)
		}
	}
	return out
}

// dctHashMatrix runs the 2D DCT-II on a 32x32 input matrix, keeps the
// top-left 8x8 block, and emits the 64-bit pHash. The DCT scaling
// factors are omitted: each coefficient is compared against the median
// of the other 63 in the same block, so any positive uniform scale on
// the row or column pass cancels out.
func dctHashMatrix(in [phashSize][phashSize]float64) uint64 {
	var rowDCT [phashSize][phashBlock]float64
	for j := 0; j < phashSize; j++ {
		for k := 0; k < phashBlock; k++ {
			var s float64
			for n := 0; n < phashSize; n++ {
				s += in[j][n] * dctCosTable[n][k]
			}
			rowDCT[j][k] = s
		}
	}
	var block [phashBlock][phashBlock]float64
	for k := 0; k < phashBlock; k++ {
		for m := 0; m < phashBlock; m++ {
			var s float64
			for n := 0; n < phashSize; n++ {
				s += rowDCT[n][k] * dctCosTable[n][m]
			}
			block[m][k] = s
		}
	}
	coeffs := make([]float64, 0, phashBlock*phashBlock-1)
	for r := 0; r < phashBlock; r++ {
		for c := 0; c < phashBlock; c++ {
			if r == 0 && c == 0 {
				continue
			}
			coeffs = append(coeffs, block[r][c])
		}
	}
	sort.Float64s(coeffs)
	// 63 elements: median is the 32nd value, index 31.
	median := coeffs[len(coeffs)/2]
	var h uint64
	for r := 0; r < phashBlock; r++ {
		for c := 0; c < phashBlock; c++ {
			if r == 0 && c == 0 {
				continue
			}
			if block[r][c] >= median {
				h |= uint64(1) << uint(r*phashBlock+c)
			}
		}
	}
	return h
}

// dctCosTable memoises cos((2n+1)*k*pi/(2*N)) for N = phashSize, since
// row and column passes both reuse the same 32*8 = 256 entries.
var dctCosTable [phashSize][phashBlock]float64

func init() {
	for n := 0; n < phashSize; n++ {
		for k := 0; k < phashBlock; k++ {
			dctCosTable[n][k] = math.Cos(float64(2*n+1) * float64(k) * math.Pi / float64(2*phashSize))
		}
	}
}

// PhashHooks is the extension point a higher layer (internal/relations)
// uses to keep its in-memory BK-tree consistent with what
// RecomputeAndStorePhash just wrote. Set by the relations package's
// init(); nil-safe when no hook is registered (tests, --tags variants).
// gallery → relations would be a cycle, so the relations side
// registers itself here rather than being called directly.
var PhashHooks struct {
	// OnStored fires after a successful UPDATE images SET phash = ?
	// row. Database is the handle the UPDATE ran on, so the registry
	// dispatch can find the right per-gallery tree.
	OnStored func(database *db.DB, imageID, phash int64)
}

// RecomputeAndStorePhash recomputes the phash from the image's static
// thumbnail and writes it back. The ingest path calls this after
// thumbnail generation; the re-extract maintenance loop calls it once
// per image alongside its other recompute steps. When the thumbnail
// is unreadable - missing because Generate failed, or undecodable
// because the disk image is corrupt - the row's phash stays at its
// previous value (or NULL on first compute). The relations system
// then ignores the row until the operator rebuilds thumbnails and
// re-runs the compute.
func RecomputeAndStorePhash(ctx context.Context, database *db.DB, imageID int64, thumbnailsPath string) error {
	thumb := ThumbnailPath(thumbnailsPath, imageID)
	h, err := ComputePhashFromThumb(thumb)
	if err != nil {
		return err
	}
	if _, err := database.Write.ExecContext(ctx, `UPDATE images SET phash = ? WHERE id = ?`, h, imageID); err != nil {
		return fmt.Errorf("update phash: %w", err)
	}
	if PhashHooks.OnStored != nil {
		PhashHooks.OnStored(database, imageID, h)
	}
	return nil
}
