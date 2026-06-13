package gallery

import (
	"image"
	"image/color"
	"image/jpeg"
	"math/bits"
	"os"
	"path/filepath"
	"testing"
)

// All-zero RGBA: every DCT coefficient is exactly zero, the median
// of 63 zeros is zero, and every `0 >= 0` comparison flips the bit
// on. Bit 0 (the DC slot) stays at zero by construction. Documents
// the corner-case bit pattern and pins the algorithm against
// accidental tweaks that would change it.
func TestPhashAllZeroBlack(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 32, 32))
	h := computePhash(img)
	const want uint64 = 0xFFFFFFFFFFFFFFFE
	if h != want {
		t.Fatalf("all-zero phash: got %016x want %016x", h, want)
	}
}

// computePhash on the same image is deterministic.
func TestPhashDeterministic(t *testing.T) {
	img := newGradient(64, 64)
	a := computePhash(img)
	b := computePhash(img)
	if a != b {
		t.Fatal("computePhash is non-deterministic")
	}
}

// A non-trivial image and its horizontal mirror must hash to the
// same value (the canonicalisation in computePhash is the contract
// the rest of the relations system relies on).
func TestPhashMirrorCanonical(t *testing.T) {
	img := newGradient(64, 64)
	h := computePhash(img)
	mirror := mirrorImage(img)
	hm := computePhash(mirror)
	if h != hm {
		t.Fatalf("mirror canonicalisation broken: orig=%016x mirror=%016x", h, hm)
	}
}

// Two visibly distinct images hash to noticeably different values -
// the Hamming distance is what the find-pairs job uses to discriminate.
// The threshold is fuzzy by design; assert "at least 8 bits differ" so
// we are comfortably above the d=4 session-UI cap.
func TestPhashDifferentImagesDiverge(t *testing.T) {
	left := newGradient(64, 64)
	right := newCheckerboard(64, 64, 8)
	hl := computePhash(left)
	hr := computePhash(right)
	dist := bits.OnesCount64(hl ^ hr)
	if dist < 8 {
		t.Fatalf("gradient vs checkerboard: only %d bits differ (h=%016x %016x)", dist, hl, hr)
	}
}

// End-to-end thumb path: write a JPEG to a tmpdir, run the public
// ComputePhashFromThumb, confirm it matches the in-memory hash.
func TestComputePhashFromThumbRoundTrip(t *testing.T) {
	dir := t.TempDir()
	img := newGradient(96, 96)
	path := filepath.Join(dir, "test.jpg")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create temp jpeg: %v", err)
	}
	if err := jpeg.Encode(f, img, &jpeg.Options{Quality: 85}); err != nil {
		_ = f.Close()
		t.Fatalf("encode jpeg: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close jpeg: %v", err)
	}

	h, err := ComputePhashFromThumb(path)
	if err != nil {
		t.Fatalf("ComputePhashFromThumb: %v", err)
	}
	// Bit-for-bit equality with the in-memory pipeline against the
	// decoded JPEG. We re-open and re-decode to remove the encode
	// step from the comparison.
	f2, err := os.Open(path)
	if err != nil {
		t.Fatalf("reopen temp jpeg: %v", err)
	}
	dec, err := jpeg.Decode(f2)
	_ = f2.Close()
	if err != nil {
		t.Fatalf("decode temp jpeg: %v", err)
	}
	if uint64(h) != computePhash(dec) {
		t.Fatalf("roundtrip mismatch: file=%016x mem=%016x", uint64(h), computePhash(dec))
	}
}

// Missing thumbnail returns an error so the caller knows to leave
// images.phash NULL.
func TestComputePhashFromThumbMissing(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "nope.jpg")
	if _, err := ComputePhashFromThumb(missing); err == nil {
		t.Fatal("expected error on missing thumbnail, got nil")
	}
}

// newGradient returns a diagonal-luma RGBA gradient. Deterministic
// per (w, h) so two calls produce identical pixel data.
func newGradient(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := uint8((x + y) * 255 / (w + h - 2))
			img.SetRGBA(x, y, color.RGBA{R: v, G: v, B: v, A: 255})
		}
	}
	return img
}

// newCheckerboard returns an RGBA checkerboard with `cell`-pixel
// squares alternating between black and white.
func newCheckerboard(w, h, cell int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			on := ((x/cell)+(y/cell))%2 == 0
			c := color.RGBA{A: 255}
			if on {
				c.R, c.G, c.B = 255, 255, 255
			}
			img.SetRGBA(x, y, c)
		}
	}
	return img
}

func mirrorImage(src *image.RGBA) *image.RGBA {
	b := src.Bounds()
	dst := image.NewRGBA(b)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			dst.Set(b.Max.X-1-(x-b.Min.X), y, src.At(x, y))
		}
	}
	return dst
}
