package gallery

import (
	"database/sql"
	"image"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/leqwin/monbooru/internal/models"
)

// generateTinyMP4 writes a 32x18 1-frame mp4 to dst via the system
// ffmpeg. Used by the dimension-probe and ingest tests so the
// assertions ride a real container rather than hand-rolled bytes that
// would not survive ffprobe.
func generateTinyMP4(t *testing.T, dst string) {
	t.Helper()
	if !ffmpegAvailable() {
		t.Skip("ffmpeg not available")
	}
	cmd := exec.Command("ffmpeg",
		"-y", "-loglevel", "quiet",
		"-f", "lavfi", "-i", "color=c=black:s=32x18:d=0.1:r=10",
		"-c:v", "libx264", "-pix_fmt", "yuv420p",
		"--", dst,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg generate mp4: %v\n%s", err, out)
	}
}

func TestNormalizeImage(t *testing.T) {
	if !ffmpegAvailable() {
		t.Skip("ffmpeg not available")
	}
	dir := t.TempDir()

	// The re-encode lands a baseline JPEG the stdlib decode path can read,
	// at the same dimensions. This is the rescue applied to an upload whose
	// original chroma subsampling Go's image/jpeg refuses.
	src := createTestJPEG(t, dir, "in.jpg", 320, 240)
	if err := NormalizeImage(src); err != nil {
		t.Fatalf("NormalizeImage: %v", err)
	}
	f, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		t.Fatalf("normalized file does not decode with the stdlib: %v", err)
	}
	if cfg.Width != 320 || cfg.Height != 240 {
		t.Errorf("normalized dimensions = %dx%d, want 320x240", cfg.Width, cfg.Height)
	}

	// A non-image cannot be rescued: ffmpeg fails so the call errors and the
	// caller still rejects the upload.
	bad := filepath.Join(dir, "bad.jpg")
	if err := os.WriteFile(bad, []byte("not an image"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := NormalizeImage(bad); err == nil {
		t.Error("NormalizeImage on a non-image should return an error")
	}
}

func TestProbeVideoDimensions_RealMP4(t *testing.T) {
	if !ffmpegAvailable() {
		t.Skip("ffmpeg not available")
	}
	tmp := t.TempDir()
	mp4 := filepath.Join(tmp, "clip.mp4")
	generateTinyMP4(t, mp4)

	w, h, ok := ProbeVideoDimensions(mp4)
	if !ok {
		t.Fatal("ProbeVideoDimensions returned ok=false for a real mp4")
	}
	if w != 32 || h != 18 {
		t.Errorf("ProbeVideoDimensions = %dx%d, want 32x18", w, h)
	}
}

func TestProbeVideoDimensions_MissingFile(t *testing.T) {
	if !ffmpegAvailable() {
		t.Skip("ffmpeg not available")
	}
	if _, _, ok := ProbeVideoDimensions("/nonexistent/no-such-file.mp4"); ok {
		t.Error("ProbeVideoDimensions returned ok=true for a missing file")
	}
}

func TestProbeVideoDimensions_NonVideo(t *testing.T) {
	if !ffmpegAvailable() {
		t.Skip("ffmpeg not available")
	}
	tmp := t.TempDir()
	junk := filepath.Join(tmp, "not-a-video.txt")
	if err := os.WriteFile(junk, []byte("definitely not a video"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := ProbeVideoDimensions(junk); ok {
		t.Error("ProbeVideoDimensions returned ok=true for a text file")
	}
}

func TestIngest_VideoPopulatesWidthHeight(t *testing.T) {
	if !ffmpegAvailable() {
		t.Skip("ffmpeg not available")
	}
	database, env, galleryDir := setupSyncTest(t)
	mp4 := filepath.Join(galleryDir, "clip.mp4")
	generateTinyMP4(t, mp4)

	img, dup, err := Ingest(database, env.galleryPath, env.thumbnailsPath, mp4, models.FileTypeMP4, models.OriginIngest)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if dup {
		t.Fatalf("expected non-duplicate")
	}
	if img.Width == nil || img.Height == nil {
		t.Fatalf("expected populated width/height, got width=%v height=%v", img.Width, img.Height)
	}
	if *img.Width != 32 || *img.Height != 18 {
		t.Errorf("ingest returned width/height = %d/%d, want 32/18", *img.Width, *img.Height)
	}

	var dbW, dbH sql.NullInt64
	if err := database.Read.QueryRow(`SELECT width, height FROM images WHERE id = ?`, img.ID).Scan(&dbW, &dbH); err != nil {
		t.Fatal(err)
	}
	if !dbW.Valid || dbW.Int64 != 32 {
		t.Errorf("DB width = %v, want 32", dbW)
	}
	if !dbH.Valid || dbH.Int64 != 18 {
		t.Errorf("DB height = %v, want 18", dbH)
	}
}
