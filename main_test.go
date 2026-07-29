package main

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"mime/multipart"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadEnvRequiresValuesAndResolvesDBRelativeToEnvFile(t *testing.T) {
	dir := t.TempDir()
	envDir := filepath.Join(dir, "config")
	if err := os.MkdirAll(envDir, 0755); err != nil {
		t.Fatal(err)
	}
	envPath := filepath.Join(envDir, ".env")
	if err := os.WriteFile(envPath, []byte("ADMIN_USERNAME=admin\nADMIN_PASSWORD=secret\nSESSION_SECRET=test-secret\nDB_PATH=../data/main.sqlite\n"), 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadEnv(envPath)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "data", "main.sqlite")
	if cfg.DBPath != want {
		t.Fatalf("expected DB path %q, got %q", want, cfg.DBPath)
	}
}

func TestLoadEnvRejectsMissingValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("ADMIN_USERNAME=admin\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadEnv(path); err == nil {
		t.Fatal("expected missing env values to fail")
	}
}

func TestRecordFailureBlocksAfterFiveAttemptsAndPurgesOldRows(t *testing.T) {
	app := testApp(t)
	now := time.Unix(200000, 0)
	if _, err := app.db.Exec(`INSERT INTO login_failures (ip, attempted_at) VALUES (?, ?)`, "127.0.0.1", now.Add(-25*time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 4; i++ {
		blocked, err := app.recordFailure("127.0.0.1", now)
		if err != nil {
			t.Fatal(err)
		}
		if blocked {
			t.Fatalf("attempt %d should not be blocked yet", i+1)
		}
	}
	blocked, err := app.recordFailure("127.0.0.1", now)
	if err != nil {
		t.Fatal(err)
	}
	if !blocked {
		t.Fatal("fifth recent failure should block")
	}

	count, err := app.failureCount("127.0.0.1", now)
	if err != nil {
		t.Fatal(err)
	}
	if count != 5 {
		t.Fatalf("expected old row purged and 5 recent rows counted, got %d", count)
	}
}

func TestSessionCookieIsSignedAndServerSide(t *testing.T) {
	app := testApp(t)
	sid := "session-id"
	app.sessions.set(sid, time.Now().Add(time.Hour))
	cookie := app.sessionCookie(sid, time.Now().Add(time.Hour))

	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(cookie)
	if !app.isAuthenticated(req) {
		t.Fatal("expected signed active session to authenticate")
	}

	req = httptest.NewRequest("GET", "/", nil)
	req.AddCookie(cookie)
	app.sessions.delete(sid)
	if app.isAuthenticated(req) {
		t.Fatal("deleted server-side session should not authenticate")
	}
}

func TestResizeUploadReturnsDownloadBytes(t *testing.T) {
	var input bytes.Buffer
	writeJPEGTo(t, &input, 1600, 800, 95)
	file := multipart.File(nopFile{bytes.NewReader(input.Bytes())})

	result, err := resizeUpload(file, "wide.jpg", 400, 80)
	if err != nil {
		t.Fatal(err)
	}
	if result.filename != "wide-webfit.jpg" {
		t.Fatalf("unexpected filename: %s", result.filename)
	}
	if result.contentType != "image/jpeg" {
		t.Fatalf("unexpected content type: %s", result.contentType)
	}
	if result.width != 1600 || result.height != 800 || result.outWidth != 400 || result.outHeight != 200 {
		t.Fatalf("unexpected dimensions: %#v", result)
	}
	if len(result.data) == 0 || bytes.Equal(result.data, input.Bytes()) {
		t.Fatal("expected resized output bytes")
	}
}

func TestResolveResizeWidthSupportsPresetsAndCustomWidth(t *testing.T) {
	width, preset, err := resolveResizeWidth("hero", "")
	if err != nil {
		t.Fatal(err)
	}
	if width != 1920 || preset != "hero" {
		t.Fatalf("unexpected hero preset: width=%d preset=%s", width, preset)
	}

	width, preset, err = resolveResizeWidth("custom", "735")
	if err != nil {
		t.Fatal(err)
	}
	if width != 735 || preset != "custom" {
		t.Fatalf("unexpected custom preset: width=%d preset=%s", width, preset)
	}

	if _, _, err := resolveResizeWidth("poster", "1200"); err == nil {
		t.Fatal("expected unknown preset to fail")
	}
	if _, _, err := resolveResizeWidth("custom", "0"); err == nil {
		t.Fatal("expected invalid custom width to fail")
	}
}

func TestResizeUploadRejectsUnsupportedImages(t *testing.T) {
	file := multipart.File(nopFile{bytes.NewReader([]byte("not an image"))})
	if _, err := resizeUpload(file, "notes.txt", 400, 80); err == nil {
		t.Fatal("expected unsupported upload to fail")
	}
}

func testApp(t *testing.T) *app {
	t.Helper()
	db, err := openDB(filepath.Join(t.TempDir(), "main.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = db.Close()
	})
	return &app{
		cfg: appConfig{
			AdminUsername: "admin",
			AdminPassword: "secret",
			SessionSecret: "test-secret",
			DBPath:        "unused",
		},
		db:       db,
		sessions: newSessionStore(),
	}
}

func writeJPEGTo(t *testing.T, out *bytes.Buffer, width, height, quality int) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 2), G: uint8(y * 2), B: uint8(x + y), A: 255})
		}
	}
	if err := jpeg.Encode(out, img, &jpeg.Options{Quality: quality}); err != nil {
		t.Fatal(err)
	}
}

func writePNGTo(t *testing.T, out *bytes.Buffer) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 20, 20))
	if err := png.Encode(out, img); err != nil {
		t.Fatal(err)
	}
}

type nopFile struct {
	*bytes.Reader
}

func (nopFile) Close() error {
	return nil
}
