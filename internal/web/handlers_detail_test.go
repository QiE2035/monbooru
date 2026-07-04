package web

import (
	"testing"

	"github.com/leqwin/monbooru/internal/models"
)

func TestBuildAnnotationViews_ClampsToImageBounds(t *testing.T) {
	w, h := 40, 30
	img := &models.Image{Width: &w, Height: &h}
	views := buildAnnotationViews(img, []models.Annotation{
		{X: 10, Y: 20, W: 100, H: 40, Body: "overflow"},
		{X: -5, Y: -5, W: 10, H: 10, Body: "negative"},
	})
	if len(views) != 2 {
		t.Fatalf("views = %d, want 2", len(views))
	}
	if got, want := string(views[0].Style), "left:25.0000%;top:66.6667%;width:75.0000%;height:33.3333%"; got != want {
		t.Errorf("oversized box style = %q, want %q (clamped to the far edge)", got, want)
	}
	if got, want := string(views[1].Style), "left:0.0000%;top:0.0000%;width:25.0000%;height:33.3333%"; got != want {
		t.Errorf("negative box style = %q, want %q (floored at the origin)", got, want)
	}
}
