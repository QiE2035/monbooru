//go:build tagger

package tagger

import (
	"image"
	"image/color"
	"math"
	"testing"
)

// makeTestImage builds a small RGB pattern so the tensor-build paths see
// non-uniform pixels (channel order errors would slip through if every
// pixel were grey).
func makeTestImage(w, h int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{
				R: uint8((x * 7) & 0xff),
				G: uint8((y*11 + 30) & 0xff),
				B: uint8((x*y + 17) & 0xff),
				A: 0xff,
			})
		}
	}
	return img
}

// TestBuildTensor_WD14_BitIdentical pins the WD14 path to a snapshot
// taken from the pre-refactor code. NHWC + BGR + 0..255 floats. Any
// regression in the NHWC arm flips one of these floats and trips here
// before it reaches an actual model run.
func TestBuildTensor_WD14_BitIdentical(t *testing.T) {
	const size = 16
	src := makeTestImage(20, 12)
	processed := padAndResize(src, size, wd14Profile)
	got := make([]float32, 3*size*size)
	shape, err := buildTensor(processed, got, size, wd14Profile)
	if err != nil {
		t.Fatalf("buildTensor: %v", err)
	}
	if shape[0] != 1 || shape[1] != int64(size) || shape[2] != int64(size) || shape[3] != 3 {
		t.Errorf("shape = %v, want [1,%d,%d,3]", shape, size, size)
	}
	// Hand-computed reference: walk the resized RGBA and emit float32
	// in BGR order, no normalisation. Matches the pre-refactor body.
	pix := processed.Pix
	stride := processed.Stride
	want := make([]float32, size*size*3)
	for y := 0; y < size; y++ {
		row := pix[y*stride:]
		for x := 0; x < size; x++ {
			s := x * 4
			d := (y*size + x) * 3
			want[d+0] = float32(row[s+2])
			want[d+1] = float32(row[s+1])
			want[d+2] = float32(row[s+0])
		}
	}
	if len(got) != len(want) {
		t.Fatalf("length mismatch: got %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("idx %d: got %v, want %v", i, got[i], want[i])
		}
	}
}

// TestBuildTensor_Camie_ImageNet pins the camie-v2 NCHW + RGB +
// ImageNet path to a hand-computed reference. Same shape as joytag with
// different mean/std constants.
func TestBuildTensor_Camie_ImageNet(t *testing.T) {
	const size = 16
	src := makeTestImage(20, 12)
	prof := Profile{Layout: "nchw", Channels: "rgb", Normalize: "imagenet", Pad: "white_square"}
	processed := padAndResize(src, size, prof)
	got := make([]float32, 3*size*size)
	shape, err := buildTensor(processed, got, size, prof)
	if err != nil {
		t.Fatalf("buildTensor: %v", err)
	}
	if shape[0] != 1 || shape[1] != 3 || shape[2] != int64(size) || shape[3] != int64(size) {
		t.Errorf("shape = %v, want [1,3,%d,%d]", shape, size, size)
	}
	pix := processed.Pix
	stride := processed.Stride
	plane := size * size
	want := make([]float32, 3*plane)
	for y := 0; y < size; y++ {
		row := pix[y*stride:]
		for x := 0; x < size; x++ {
			s := x * 4
			off := y*size + x
			want[0*plane+off] = (float32(row[s+0])/255 - imageNetMean[0]) / imageNetStd[0]
			want[1*plane+off] = (float32(row[s+1])/255 - imageNetMean[1]) / imageNetStd[1]
			want[2*plane+off] = (float32(row[s+2])/255 - imageNetMean[2]) / imageNetStd[2]
		}
	}
	for i := range want {
		if math.Float32bits(got[i]) != math.Float32bits(want[i]) {
			t.Fatalf("idx %d: got %v, want %v", i, got[i], want[i])
		}
	}
}

// TestBuildTensor_JoyTag_BitIdentical pins the joytag path to the same
// snapshot the pre-refactor code produced (NCHW + RGB + CLIP normalise).
func TestBuildTensor_JoyTag_BitIdentical(t *testing.T) {
	const size = 16
	src := makeTestImage(20, 12)
	processed := padAndResize(src, size, joytagProfile)
	got := make([]float32, 3*size*size)
	shape, err := buildTensor(processed, got, size, joytagProfile)
	if err != nil {
		t.Fatalf("buildTensor: %v", err)
	}
	if shape[0] != 1 || shape[1] != 3 || shape[2] != int64(size) || shape[3] != int64(size) {
		t.Errorf("shape = %v, want [1,3,%d,%d]", shape, size, size)
	}
	pix := processed.Pix
	stride := processed.Stride
	plane := size * size
	want := make([]float32, 3*plane)
	for y := 0; y < size; y++ {
		row := pix[y*stride:]
		for x := 0; x < size; x++ {
			s := x * 4
			off := y*size + x
			want[0*plane+off] = (float32(row[s+0])/255 - clipMean[0]) / clipStd[0]
			want[1*plane+off] = (float32(row[s+1])/255 - clipMean[1]) / clipStd[1]
			want[2*plane+off] = (float32(row[s+2])/255 - clipMean[2]) / clipStd[2]
		}
	}
	if len(got) != len(want) {
		t.Fatalf("length mismatch: got %d, want %d", len(got), len(want))
	}
	for i := range want {
		// Floats produced by the same expression on the same machine
		// must compare exactly equal; any drift here means the new
		// code reorders an arithmetic op.
		if math.Float32bits(got[i]) != math.Float32bits(want[i]) {
			t.Fatalf("idx %d: got %v (bits %x), want %v (bits %x)",
				i, got[i], math.Float32bits(got[i]),
				want[i], math.Float32bits(want[i]))
		}
	}
}

// TestPadAndResize_WhiteSquare_AlphaForced confirms the legacy white-pad
// path still forces the alpha-zero pixels to opaque white. A regression
// here would let WD14 see transparent corners as `(0,0,0)` instead of
// `(255,255,255)` and silently shift the tensor distribution.
func TestPadAndResize_WhiteSquare_AlphaForced(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 4, 4))
	// Fully transparent 4x4 input.
	dst := padAndResize(src, 8, wd14Profile)
	for i := 0; i+3 < len(dst.Pix); i += 4 {
		if dst.Pix[i] != 0xff || dst.Pix[i+1] != 0xff || dst.Pix[i+2] != 0xff || dst.Pix[i+3] != 0xff {
			t.Fatalf("pix %d not forced to opaque white: %v", i, dst.Pix[i:i+4])
		}
	}
}

// TestPadAndResize_MeanColorAspect pins Camie's pad recipe: a non-square
// source resizes preserving aspect into a size×size canvas, with the
// canvas background filled by the profile's FillColor (default
// (124,116,104)).
func TestPadAndResize_MeanColorAspect(t *testing.T) {
	// Solid red 100x50 source. The 100-edge becomes the 64-pixel wide
	// scaled image, with a 32-pixel high vertical strip; the canvas is
	// 64x64 with the unfilled rows showing the camie default fill.
	src := image.NewRGBA(image.Rect(0, 0, 100, 50))
	for i := 0; i+3 < len(src.Pix); i += 4 {
		src.Pix[i+0] = 0xff
		src.Pix[i+1] = 0x00
		src.Pix[i+2] = 0x00
		src.Pix[i+3] = 0xff
	}
	prof := Profile{Pad: "mean_color_aspect"}
	dst := padAndResize(src, 64, prof)
	if dst.Bounds().Dx() != 64 || dst.Bounds().Dy() != 64 {
		t.Fatalf("size = %v, want 64x64", dst.Bounds())
	}
	// Top row is fill; centre row is the resized red strip. Sample.
	stride := dst.Stride
	off := func(x, y int) int { return y*stride + x*4 }
	// Top-left corner: fill colour.
	if dst.Pix[off(0, 0)+0] != camieDefaultFill[0] ||
		dst.Pix[off(0, 0)+1] != camieDefaultFill[1] ||
		dst.Pix[off(0, 0)+2] != camieDefaultFill[2] {
		t.Errorf("top-left = (%d,%d,%d), want camie default %v",
			dst.Pix[off(0, 0)], dst.Pix[off(0, 0)+1], dst.Pix[off(0, 0)+2], camieDefaultFill)
	}
	// Centre: red strip pixel.
	if dst.Pix[off(32, 32)+0] != 0xff ||
		dst.Pix[off(32, 32)+1] != 0x00 ||
		dst.Pix[off(32, 32)+2] != 0x00 {
		t.Errorf("centre = (%d,%d,%d), want red",
			dst.Pix[off(32, 32)], dst.Pix[off(32, 32)+1], dst.Pix[off(32, 32)+2])
	}
}

// TestPadAndResize_MeanColorAspect_FillColorOverride verifies the
// profile.FillColor override path replaces the camie default. This is
// the knob a sidecar would flip. A non-square source guarantees the
// scaled image leaves visible padding rows on the canvas.
func TestPadAndResize_MeanColorAspect_FillColorOverride(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 100, 50))
	for i := 0; i+3 < len(src.Pix); i += 4 {
		src.Pix[i+0] = 0xff
		src.Pix[i+3] = 0xff
	}
	prof := Profile{Pad: "mean_color_aspect", FillColor: [3]uint8{10, 20, 30}}
	dst := padAndResize(src, 64, prof)
	// 100x50 → scaled to 64x32 centred on a 64x64 canvas; rows 0..15 and
	// 48..63 are pure fill, the central 32 rows carry the source.
	off := dst.Stride*0 + 0
	if dst.Pix[off] != 10 || dst.Pix[off+1] != 20 || dst.Pix[off+2] != 30 {
		t.Errorf("override fill = (%d,%d,%d), want (10,20,30)",
			dst.Pix[off], dst.Pix[off+1], dst.Pix[off+2])
	}
}

// TestBuildTensor_PoolReuse_NoStaleData seeds the input buffer with
// sentinel bytes and confirms buildTensor rewrites every position - if
// any path were to skip writing a cell, recycled buffers from the
// inputTensorPools would leak last call's data into the next inference.
func TestBuildTensor_PoolReuse_NoStaleData(t *testing.T) {
	const size = 16
	src := makeTestImage(20, 12)
	processed := padAndResize(src, size, wd14Profile)

	buf := make([]float32, 3*size*size)
	for i := range buf {
		buf[i] = -777
	}
	if _, err := buildTensor(processed, buf, size, wd14Profile); err != nil {
		t.Fatalf("buildTensor: %v", err)
	}
	for i, v := range buf {
		if v == -777 {
			t.Fatalf("tensor[%d] left at sentinel - buildTensor missed this position", i)
		}
	}

	prof := Profile{Layout: "nchw", Channels: "rgb", Normalize: "imagenet", Pad: "white_square"}
	processed = padAndResize(src, size, prof)
	for i := range buf {
		buf[i] = -777
	}
	if _, err := buildTensor(processed, buf, size, prof); err != nil {
		t.Fatalf("buildTensor (nchw): %v", err)
	}
	for i, v := range buf {
		if v == -777 {
			t.Fatalf("nchw tensor[%d] left at sentinel", i)
		}
	}
}

// TestInferInputSize_ReadsFromONNXShape locks in the NHWC vs NCHW axis
// pick. Real ORT models send dimensions through here at session-build
// time; a flipped axis would silently feed a 3-pixel-wide tensor to a
// 448-pixel model.
func TestInferInputSize_ReadsFromONNXShape(t *testing.T) {
	cases := []struct {
		name   string
		layout string
		dims   []int64
		want   int
	}{
		{"NHWC standard 448", "nhwc", []int64{1, 448, 448, 3}, 448},
		{"NCHW standard 448", "nchw", []int64{1, 3, 448, 448}, 448},
		{"NCHW Camie 512", "nchw", []int64{1, 3, 512, 512}, 512},
		{"dynamic axis", "nchw", []int64{-1, 3, -1, -1}, 0},
		{"unexpected rank", "nhwc", []int64{448, 448, 3}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := inferInputSize(c.dims, c.layout); got != c.want {
				t.Errorf("inferInputSize(%v, %q) = %d, want %d", c.dims, c.layout, got, c.want)
			}
		})
	}
}
