package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"math"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/HugoSmits86/nativewebp"
	"golang.org/x/image/draw"
	"golang.org/x/image/webp"
	_ "modernc.org/sqlite"
)

const (
	defaultEnvPath  = "config/.env"
	defaultDBPath   = "../data/main.sqlite"
	sessionCookie   = "webfit_session"
	sessionLifetime = 12 * time.Hour
	rateWindow      = 24 * time.Hour
	maxFailures     = 5
	maxUploadBytes  = 50 << 20
)

type resizePreset struct {
	ID    string
	Label string
	Width int
	Use   string
}

var resizePresets = []resizePreset{
	{ID: "hero", Label: "Hero", Width: 1920, Use: "Full-width banners, landing pages, large feature images"},
	{ID: "wide", Label: "Wide content", Width: 1440, Use: "Article headers, rich content blocks, large galleries"},
	{ID: "card", Label: "Card", Width: 1200, Use: "Product cards, section images, reusable page assets"},
	{ID: "article", Label: "Article", Width: 960, Use: "Blog images, documentation, centered content"},
	{ID: "social", Label: "Social post", Width: 1080, Use: "Social sharing assets that keep the original aspect ratio"},
	{ID: "thumb", Label: "Thumbnail", Width: 480, Use: "Lists, previews, compact UI images"},
	{ID: "icon", Label: "Icon", Width: 256, Use: "Logos, avatars, small interface assets"},
}

type appConfig struct {
	AdminUsername string
	AdminPassword string
	SessionSecret string
	DBPath        string
}

type app struct {
	cfg      appConfig
	db       *sql.DB
	sessions *sessionStore
}

type sessionStore struct {
	mu       sync.Mutex
	sessions map[string]time.Time
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "webfit:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) > 0 && args[0] == "init" {
		return initEnv(args[1:])
	}
	return serve(args)
}

func initEnv(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	envPath := fs.String("env", defaultEnvPath, "environment file to create")
	if err := fs.Parse(args); err != nil {
		return err
	}

	secret, err := randomToken(48)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(*envPath), 0755); err != nil {
		return err
	}
	envDir := filepath.Dir(*envPath)
	dbPath := filepath.Clean(filepath.Join(envDir, defaultDBPath))
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		return err
	}
	content := fmt.Sprintf("ADMIN_USERNAME=admin\nADMIN_PASSWORD=change-me-now\nSESSION_SECRET=%s\nDB_PATH=%s\n", secret, defaultDBPath)
	if err := writeNewFile(*envPath, []byte(content), 0600); err != nil {
		return err
	}
	fmt.Printf("created %s\n", *envPath)
	fmt.Printf("database path: %s\n", dbPath)
	return nil
}

func serve(args []string) error {
	fs := flag.NewFlagSet("webfit", flag.ExitOnError)
	addr := fs.String("addr", "0.0.0.0:8787", "address for the web app")
	envPath := fs.String("env", "", "required environment file")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage:")
		fmt.Fprintln(os.Stderr, "  webfit init")
		fmt.Fprintln(os.Stderr, "  webfit -env ./config/.env [-addr 0.0.0.0:8787]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *envPath == "" {
		fs.Usage()
		return errors.New("missing required -env path")
	}

	cfg, err := loadEnv(*envPath)
	if err != nil {
		return err
	}
	db, err := openDB(cfg.DBPath)
	if err != nil {
		return err
	}
	defer db.Close()

	app := &app{
		cfg:      cfg,
		db:       db,
		sessions: newSessionStore(),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", app.requireAuth(app.handleIndex))
	mux.HandleFunc("/resize", app.requireAuth(app.handleResize))
	mux.HandleFunc("/login", app.handleLogin)
	mux.HandleFunc("/logout", app.handleLogout)

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	fmt.Printf("webfit running at http://%s\n", ln.Addr().String())
	return http.Serve(ln, mux)
}

func loadEnv(path string) (appConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return appConfig{}, err
	}
	values := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return appConfig{}, fmt.Errorf("invalid env line: %s", line)
		}
		values[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(value), `"'`)
	}

	cfg := appConfig{
		AdminUsername: values["ADMIN_USERNAME"],
		AdminPassword: values["ADMIN_PASSWORD"],
		SessionSecret: values["SESSION_SECRET"],
		DBPath:        values["DB_PATH"],
	}
	if cfg.AdminUsername == "" || cfg.AdminPassword == "" || cfg.SessionSecret == "" || cfg.DBPath == "" {
		return appConfig{}, errors.New("ADMIN_USERNAME, ADMIN_PASSWORD, SESSION_SECRET, and DB_PATH are required")
	}
	if !filepath.IsAbs(cfg.DBPath) {
		cfg.DBPath = filepath.Clean(filepath.Join(filepath.Dir(path), cfg.DBPath))
	}
	return cfg, nil
}

func openDB(path string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(`
CREATE TABLE IF NOT EXISTS login_failures (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  ip TEXT NOT NULL,
  attempted_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_login_failures_ip_time
ON login_failures (ip, attempted_at);
`); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func (a *app) handleLogin(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if a.isAuthenticated(r) {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		writeHTML(w, renderLogin(""))
	case http.MethodPost:
		a.submitLogin(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *app) submitLogin(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	now := time.Now()
	blocked, err := a.isBlocked(ip, now)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if blocked {
		http.Error(w, "too many failed login attempts", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	userOK := subtleEqual(r.FormValue("username"), a.cfg.AdminUsername)
	passOK := subtleEqual(r.FormValue("password"), a.cfg.AdminPassword)
	if userOK && passOK {
		sid, err := randomToken(32)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		a.sessions.set(sid, now.Add(sessionLifetime))
		http.SetCookie(w, a.sessionCookie(sid, now.Add(sessionLifetime)))
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	blocked, err = a.recordFailure(ip, now)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if blocked {
		http.Error(w, "too many failed login attempts", http.StatusForbidden)
		return
	}
	writeHTML(w, renderLogin("Invalid username or password."))
}

func (a *app) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		if sid, ok := a.verifyCookie(cookie.Value); ok {
			a.sessions.delete(sid)
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (a *app) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	writeHTML(w, appHTML)
}

func (a *app) handleResize(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if err := r.ParseMultipartForm(maxUploadBytes); err != nil {
		http.Error(w, "upload must be 50 MiB or smaller", http.StatusBadRequest)
		return
	}
	width, preset, err := resolveResizeWidth(r.FormValue("preset"), r.FormValue("width"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	quality, err := parseIntField(r, "quality", 82)
	if err != nil || quality < 1 || quality > 100 {
		http.Error(w, "quality must be between 1 and 100", http.StatusBadRequest)
		return
	}
	outputFormat, err := resolveOutputFormat(r.FormValue("format"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	file, header, err := r.FormFile("image")
	if err != nil {
		http.Error(w, "image upload is required", http.StatusBadRequest)
		return
	}
	defer file.Close()

	result, err := resizeUpload(file, header.Filename, width, quality, outputFormat)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", result.contentType)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, result.filename))
	w.Header().Set("X-Webfit-Input-Size", strconv.FormatInt(result.before, 10))
	w.Header().Set("X-Webfit-Output-Size", strconv.FormatInt(int64(len(result.data)), 10))
	w.Header().Set("X-Webfit-Input-Dimensions", fmt.Sprintf("%dx%d", result.width, result.height))
	w.Header().Set("X-Webfit-Output-Dimensions", fmt.Sprintf("%dx%d", result.outWidth, result.outHeight))
	w.Header().Set("X-Webfit-Preset", preset)
	_, _ = w.Write(result.data)
}

func (a *app) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.isAuthenticated(r) {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next(w, r)
	}
}

func (a *app) isAuthenticated(r *http.Request) bool {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	sid, ok := a.verifyCookie(cookie.Value)
	if !ok {
		return false
	}
	return a.sessions.valid(sid, time.Now())
}

func (a *app) isBlocked(ip string, now time.Time) (bool, error) {
	if err := a.purgeFailures(now); err != nil {
		return false, err
	}
	count, err := a.failureCount(ip, now)
	return count >= maxFailures, err
}

func (a *app) recordFailure(ip string, now time.Time) (bool, error) {
	if err := a.purgeFailures(now); err != nil {
		return false, err
	}
	if _, err := a.db.Exec(`INSERT INTO login_failures (ip, attempted_at) VALUES (?, ?)`, ip, now.Unix()); err != nil {
		return false, err
	}
	count, err := a.failureCount(ip, now)
	return count >= maxFailures, err
}

func (a *app) purgeFailures(now time.Time) error {
	_, err := a.db.Exec(`DELETE FROM login_failures WHERE attempted_at < ?`, now.Add(-rateWindow).Unix())
	return err
}

func (a *app) failureCount(ip string, now time.Time) (int, error) {
	var count int
	err := a.db.QueryRow(`SELECT COUNT(*) FROM login_failures WHERE ip = ? AND attempted_at >= ?`, ip, now.Add(-rateWindow).Unix()).Scan(&count)
	return count, err
}

func (a *app) sessionCookie(sid string, expires time.Time) *http.Cookie {
	return &http.Cookie{
		Name:     sessionCookie,
		Value:    sid + "." + a.sign(sid),
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	}
}

func (a *app) sign(value string) string {
	mac := hmac.New(sha256.New, []byte(a.cfg.SessionSecret))
	_, _ = mac.Write([]byte(value))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (a *app) verifyCookie(value string) (string, bool) {
	sid, sig, ok := strings.Cut(value, ".")
	if !ok || sid == "" || sig == "" {
		return "", false
	}
	expected := a.sign(sid)
	if !hmac.Equal([]byte(sig), []byte(expected)) {
		return "", false
	}
	return sid, true
}

func newSessionStore() *sessionStore {
	return &sessionStore{sessions: map[string]time.Time{}}
}

func (s *sessionStore) set(id string, expires time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[id] = expires
}

func (s *sessionStore) valid(id string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	expires, ok := s.sessions[id]
	if !ok {
		return false
	}
	if now.After(expires) {
		delete(s.sessions, id)
		return false
	}
	return true
}

func (s *sessionStore) delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
}

type uploadResult struct {
	data        []byte
	filename    string
	contentType string
	before      int64
	width       int
	height      int
	outWidth    int
	outHeight   int
}

func resizeUpload(file multipart.File, filename string, maxWidth int, quality int, outputFormat string) (uploadResult, error) {
	data, err := io.ReadAll(file)
	if err != nil {
		return uploadResult{}, err
	}
	out, meta, contentType, err := resizeBytes(filename, data, maxWidth, quality, outputFormat)
	if err != nil {
		return uploadResult{}, err
	}
	return uploadResult{
		data:        out,
		filename:    downloadName(filename, contentType),
		contentType: contentType,
		before:      int64(len(data)),
		width:       meta.width,
		height:      meta.height,
		outWidth:    meta.outWidth,
		outHeight:   meta.outHeight,
	}, nil
}

type imageMeta struct {
	width     int
	height    int
	outWidth  int
	outHeight int
}

func resizeBytes(name string, data []byte, maxWidth int, quality int, outputFormat string) ([]byte, imageMeta, string, error) {
	ext := strings.ToLower(filepath.Ext(name))
	var img image.Image
	var err error
	switch ext {
	case ".jpg", ".jpeg":
		img, err = jpeg.Decode(bytes.NewReader(data))
	case ".png":
		img, err = png.Decode(bytes.NewReader(data))
	case ".webp":
		img, err = webp.Decode(bytes.NewReader(data))
	default:
		return nil, imageMeta{}, "", errors.New("supported uploads are JPEG, PNG, and WebP")
	}
	if err != nil {
		return nil, imageMeta{}, "", fmt.Errorf("decode %s: %w", strings.TrimPrefix(ext, "."), err)
	}

	img, meta := resizeToWidth(img, maxWidth)
	var out bytes.Buffer
	switch outputFormat {
	case "jpeg":
		err = jpeg.Encode(&out, img, &jpeg.Options{Quality: quality})
		return out.Bytes(), meta, "image/jpeg", err
	case "png":
		err = (&png.Encoder{CompressionLevel: png.BestCompression}).Encode(&out, img)
		return out.Bytes(), meta, "image/png", err
	case "webp":
		err = nativewebp.Encode(&out, img, &nativewebp.Options{CompressionLevel: nativewebp.DefaultCompression})
		return out.Bytes(), meta, "image/webp", err
	default:
		return nil, imageMeta{}, "", fmt.Errorf("unsupported output format %q", outputFormat)
	}
}

func resolveOutputFormat(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "jpeg", "jpg":
		return "jpeg", nil
	case "png":
		return "png", nil
	case "webp":
		return "webp", nil
	default:
		return "", errors.New("output format must be JPEG, PNG, or WebP")
	}
}

func resizeToWidth(img image.Image, maxWidth int) (image.Image, imageMeta) {
	bounds := img.Bounds()
	width := bounds.Dx()
	height := bounds.Dy()
	meta := imageMeta{width: width, height: height, outWidth: width, outHeight: height}
	if width <= maxWidth {
		return img, meta
	}

	outHeight := int(math.Round(float64(height) * (float64(maxWidth) / float64(width))))
	if outHeight < 1 {
		outHeight = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, maxWidth, outHeight))
	draw.CatmullRom.Scale(dst, dst.Bounds(), img, bounds, draw.Over, nil)
	meta.outWidth = maxWidth
	meta.outHeight = outHeight
	return dst, meta
}

func downloadName(name string, contentType string) string {
	base := filepath.Base(name)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	stem = sanitizeFilenamePart(stem)
	switch contentType {
	case "image/jpeg":
		ext = ".jpg"
	case "image/png":
		ext = ".png"
	case "image/webp":
		ext = ".webp"
	default:
		ext = sanitizeFilenamePart(strings.ToLower(ext))
	}
	if stem == "" {
		stem = "image"
	}
	if ext == "" {
		ext = ".jpg"
	}
	return stem + "-webfit" + ext
}

func sanitizeFilenamePart(value string) string {
	var out strings.Builder
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' || r == ' ' {
			out.WriteRune(r)
		}
	}
	return strings.TrimSpace(out.String())
}

func resolveResizeWidth(preset string, widthValue string) (int, string, error) {
	preset = strings.TrimSpace(preset)
	if preset == "" {
		preset = "hero"
	}
	if preset == "custom" {
		width, err := parseIntValue(widthValue, 0)
		if err != nil || width < 64 || width > 4000 {
			return 0, "", errors.New("custom width must be between 64 and 4000 pixels")
		}
		return width, "custom", nil
	}
	for _, candidate := range resizePresets {
		if preset == candidate.ID {
			return candidate.Width, candidate.ID, nil
		}
	}
	return 0, "", fmt.Errorf("unknown preset %q", preset)
}

func parseIntField(r *http.Request, name string, fallback int) (int, error) {
	return parseIntValue(r.FormValue(name), fallback)
}

func parseIntValue(value string, fallback int) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback, nil
	}
	return strconv.Atoi(value)
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

func subtleEqual(a string, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return hmac.Equal([]byte(a), []byte(b))
}

func randomToken(size int) (string, error) {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func writeNewFile(path string, data []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = file.Write(data)
	return err
}

func writeHTML(w http.ResponseWriter, html string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, html)
}

const loginHTMLStart = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>webfit login</title>
<script>
const savedTheme=localStorage.getItem("webfit-theme");
const systemDark=matchMedia("(prefers-color-scheme: dark)").matches;
document.documentElement.dataset.theme=savedTheme||((systemDark)?"dark":"light");
</script>
<style>
:root{color-scheme:light;--page-bg:#f3f5f4;--surface:#ffffff;--surface-muted:#f8faf9;--border:#dce3df;--border-strong:#c8d3cd;--text-primary:#17211d;--text-secondary:#5f6f67;--text-muted:#88958e;--brand:#16755f;--brand-hover:#105f4d;--brand-soft:#e8f4ef;--danger:#c24141;--focus:rgba(22,117,95,.22);--radius-sm:8px;--radius-md:11px;--radius-lg:16px;--radius-xl:20px}
:root[data-theme=dark]{color-scheme:dark;--page-bg:#101513;--surface:#17201c;--surface-muted:#1d2924;--border:#2d3b35;--border-strong:#43524b;--text-primary:#eef5f1;--text-secondary:#b8c6bf;--text-muted:#81918a;--brand:#42b993;--brand-hover:#5fd0aa;--brand-soft:#18382f;--danger:#f17d7d;--focus:rgba(66,185,147,.28)}
*{box-sizing:border-box}
body{margin:0;min-height:100vh;font-family:Inter,ui-sans-serif,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;background:var(--page-bg);color:var(--text-primary)}
.app-header{height:72px;background:color-mix(in srgb,var(--surface) 86%,transparent);backdrop-filter:blur(12px);border-bottom:1px solid var(--border);display:flex;align-items:center}
.header-inner{width:100%;max-width:1040px;margin:0 auto;padding:0 28px;display:flex;align-items:center;justify-content:space-between;gap:16px}
.brand-row{display:flex;gap:12px;align-items:center}.login-actions{display:flex;align-items:center}
.theme-button{width:auto;height:36px;margin:0;border:1px solid var(--border);border-radius:10px;background:var(--surface);color:var(--text-secondary);padding:0 12px;font:inherit;font-size:13px;font-weight:650;line-height:1;cursor:pointer;display:inline-flex;align-items:center;justify-content:center;appearance:none;white-space:nowrap;transition:background 140ms ease,border-color 140ms ease,color 140ms ease,box-shadow 140ms ease}.theme-button:hover{background:var(--surface-muted);border-color:var(--border-strong);color:var(--text-primary)}.theme-button:focus-visible{outline:none;box-shadow:0 0 0 4px var(--focus)}
.login-page{min-height:calc(100vh - 72px);display:grid;grid-template-columns:minmax(0,1fr) 420px;gap:40px;align-items:center;max-width:1040px;margin:0 auto;padding:48px 28px 80px}
.brand-panel{padding-bottom:32px}.brand-panel .brand-row{display:none}
.mark{width:42px;height:42px;border-radius:12px;background:var(--brand);position:relative;box-shadow:0 8px 18px rgba(22,117,95,.18)}
.mark:before,.mark:after{content:"";position:absolute;border:2px solid white;width:13px;height:13px}.mark:before{left:9px;top:9px;border-right:0;border-bottom:0}.mark:after{right:9px;bottom:9px;border-left:0;border-top:0}
.logo-name{font-size:22px;font-weight:750;letter-spacing:-.03em}.logo-subtitle{font-size:12px;color:var(--text-muted);margin-top:2px}
.brand-panel h1{font-size:36px;line-height:1.08;letter-spacing:-.035em;margin:0 0 12px}.brand-panel p{font-size:16px;line-height:1.55;color:var(--text-secondary);max-width:460px;margin:0}
.feature-list{display:grid;gap:10px;margin-top:24px;color:var(--text-secondary);font-size:14px}.feature-list span{display:flex;gap:10px;align-items:center}.feature-list span:before{content:"";width:8px;height:8px;border-radius:999px;background:var(--brand)}
.login-card{width:min(420px,calc(100vw - 32px));padding:32px;border-radius:18px;background:var(--surface);border:1px solid var(--border);box-shadow:0 18px 50px rgba(25,45,36,.08),0 2px 8px rgba(25,45,36,.04)}
.login-card h2{font-size:24px;letter-spacing:-.03em;margin:0}.login-card p{margin:8px 0 22px;color:var(--text-secondary);line-height:1.45}
label{display:grid;gap:8px;font-size:13px;font-weight:650;color:var(--text-secondary);margin-top:14px}
input{height:46px;padding:0 14px;border:1px solid var(--border-strong);border-radius:10px;background:var(--surface);color:var(--text-primary);font:inherit;font-size:15px;transition:border-color 140ms ease,box-shadow 140ms ease}
input:focus{border-color:var(--brand);box-shadow:0 0 0 4px var(--focus);outline:none}
button{height:48px;width:100%;margin-top:20px;border:1px solid var(--brand);border-radius:10px;background:var(--brand);color:white;font:inherit;font-weight:700;cursor:pointer;transition:background 140ms ease,transform 140ms ease,box-shadow 140ms ease}
button:hover{background:var(--brand-hover);transform:translateY(-1px)}button:focus-visible{outline:none;box-shadow:0 0 0 4px var(--focus)}button:active{transform:translateY(0)}
.error{display:block;color:var(--danger);font-size:13px;min-height:18px;margin-bottom:2px}.loading{color:var(--text-muted);font-size:13px;margin-top:12px;min-height:18px}
@media(max-width:840px){.header-inner{padding:0 16px}.logo-subtitle{display:none}.login-page{grid-template-columns:1fr;padding:32px 16px 56px}.brand-panel{padding-bottom:0}.brand-panel h1{font-size:30px}.login-card{width:100%}}
</style>
</head>
<body>
<header class="app-header">
<div class="header-inner">
<div class="brand-row"><div class="mark" aria-hidden="true"></div><div><div class="logo-name">webfit</div><div class="logo-subtitle">Web-ready image resizing</div></div></div>
<nav class="login-actions" aria-label="Login actions"><button id="themeToggle" class="theme-button" type="button" aria-label="Toggle color theme">Theme</button></nav>
</div>
</header>
<main class="login-page">
<section class="brand-panel" aria-label="webfit">
<div class="brand-row"><div class="mark" aria-hidden="true"></div><div><div class="logo-name">webfit</div><div class="logo-subtitle">Web-ready image resizing</div></div></div>
<h1>Images sized for the web, without the guesswork.</h1>
<p>Upload an image, choose a web-oriented target, tune quality, and export a clean resized asset.</p>
<div class="feature-list"><span>Practical presets for common web assets</span><span>Fast upload, preview, and download workflow</span><span>Private admin access with rate-limited login</span></div>
</section>
<section class="login-card">
<h2>Welcome back</h2>
<p>Sign in to resize and prepare images for the web.</p>
<form method="post" action="/login">
<span class="error" id="login-error">`

const loginHTMLEnd = `</span>
<label>Username<input name="username" autocomplete="username" aria-describedby="login-error" required autofocus></label>
<label>Password<input id="password" name="password" type="password" autocomplete="current-password" aria-describedby="login-error" required></label>
<button>Log in</button>
<div class="loading" id="loading" aria-live="polite">Initializing...</div>
</form>
</section>
</main>
<script>
const themeToggle=document.getElementById("themeToggle");
function setThemeLabel(){themeToggle.textContent=document.documentElement.dataset.theme==="dark"?"Light mode":"Dark mode";}
themeToggle.addEventListener("click",()=>{const theme=document.documentElement.dataset.theme==="dark"?"light":"dark";document.documentElement.dataset.theme=theme;localStorage.setItem("webfit-theme",theme);setThemeLabel();});
matchMedia("(prefers-color-scheme: dark)").addEventListener("change",event=>{if(!localStorage.getItem("webfit-theme")){document.documentElement.dataset.theme=event.matches?"dark":"light";setThemeLabel();}});
setThemeLabel();
window.addEventListener("load",()=>{document.getElementById("loading").textContent=""});
</script>
</body>
</html>`

func renderLogin(errorText string) string {
	return loginHTMLStart + htmlEscape(errorText) + loginHTMLEnd
}

func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;")
	return r.Replace(s)
}

const appHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>webfit</title>
<script>
const savedTheme=localStorage.getItem("webfit-theme");
const systemDark=matchMedia("(prefers-color-scheme: dark)").matches;
document.documentElement.dataset.theme=savedTheme||((systemDark)?"dark":"light");
</script>
<style>
:root{color-scheme:light;--page-bg:#f3f5f4;--surface:#ffffff;--surface-muted:#f8faf9;--border:#dce3df;--border-strong:#c8d3cd;--text-primary:#17211d;--text-secondary:#5f6f67;--text-muted:#88958e;--brand:#16755f;--brand-hover:#105f4d;--brand-soft:#e8f4ef;--danger:#c24141;--focus:rgba(22,117,95,.22);--success-bg:#edf8f3;--success-border:#bce4d2;--success-text:#235d49;--radius-sm:8px;--radius-md:11px;--radius-lg:16px;--radius-xl:20px;--shadow:0 10px 30px rgba(30,51,42,.05),0 1px 2px rgba(30,51,42,.03)}
:root[data-theme=dark]{color-scheme:dark;--page-bg:#101513;--surface:#17201c;--surface-muted:#1d2924;--border:#2d3b35;--border-strong:#43524b;--text-primary:#eef5f1;--text-secondary:#b8c6bf;--text-muted:#81918a;--brand:#42b993;--brand-hover:#5fd0aa;--brand-soft:#18382f;--danger:#f17d7d;--focus:rgba(66,185,147,.28);--success-bg:#15352b;--success-border:#2a6c57;--success-text:#c9f4e2;--shadow:0 10px 30px rgba(0,0,0,.22),0 1px 2px rgba(0,0,0,.18)}
*{box-sizing:border-box}
body{margin:0;min-height:100vh;font-family:Inter,ui-sans-serif,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;background:var(--page-bg);color:var(--text-primary)}
button,input,select{font:inherit}button{cursor:pointer;transition:background 140ms ease,border-color 140ms ease,box-shadow 140ms ease,transform 140ms ease}button:focus-visible,input:focus-visible,select:focus-visible,.upload-zone:focus-visible,.preset:focus-within{outline:none;box-shadow:0 0 0 4px var(--focus)}button:hover{transform:translateY(-1px)}button:active{transform:translateY(0)}button:disabled{opacity:.55;cursor:not-allowed;transform:none}
.app-header{height:72px;background:color-mix(in srgb,var(--surface) 86%,transparent);backdrop-filter:blur(12px);border-bottom:1px solid var(--border);display:flex;align-items:center}
.header-inner{width:100%;max-width:1240px;margin:0 auto;padding:0 28px;display:flex;align-items:center;justify-content:space-between;gap:16px}
.brand{display:flex;gap:12px;align-items:center}.mark{width:38px;height:38px;border-radius:11px;background:var(--brand);position:relative;box-shadow:0 8px 18px rgba(22,117,95,.16)}
.mark:before,.mark:after{content:"";position:absolute;border:2px solid white;width:12px;height:12px}.mark:before{left:8px;top:8px;border-right:0;border-bottom:0}.mark:after{right:8px;bottom:8px;border-left:0;border-top:0}
.logo-name{font-size:22px;font-weight:750;letter-spacing:-.03em}.logo-subtitle{font-size:12px;color:var(--text-muted);margin-top:1px}
.header-actions{display:flex;align-items:center;gap:10px}.user-pill{height:34px;display:inline-flex;align-items:center;border:1px solid var(--border);border-radius:999px;padding:0 12px;color:var(--text-secondary);font-size:13px;background:var(--surface)}
.ghost-button{height:36px;margin:0;border:1px solid transparent;border-radius:10px;background:transparent;color:var(--text-secondary);padding:0 10px;text-decoration:none;font-size:13px;font-weight:650;line-height:1;display:inline-flex;align-items:center;justify-content:center;gap:7px;appearance:none;white-space:nowrap}.ghost-button:hover{background:var(--surface-muted);color:var(--text-primary)}
.container{max-width:1240px;margin:0 auto;padding:32px 28px 48px}.heading-row{display:flex;justify-content:space-between;gap:20px;align-items:start;margin-bottom:24px}
h1{font-size:28px;line-height:1.15;letter-spacing:-.035em;margin:0;font-weight:750}.page-description{margin:8px 0 0;color:var(--text-secondary);max-width:640px;line-height:1.5}.state-label{font-size:13px;color:var(--text-muted);padding-top:8px;white-space:nowrap}
.workspace{display:grid;grid-template-columns:minmax(340px,420px) minmax(0,1fr);gap:24px;align-items:start}.controls-card,.preview-card{background:var(--surface);border:1px solid var(--border);border-radius:var(--radius-lg);box-shadow:var(--shadow)}
.controls-card{padding:22px;display:grid;gap:24px}.section{display:grid;gap:12px}.section-title{font-size:13px;font-weight:700;color:var(--text-primary)}.section-help{font-size:13px;color:var(--text-secondary);margin:3px 0 0;line-height:1.4}
.visually-hidden{position:absolute;width:1px;height:1px;padding:0;margin:-1px;overflow:hidden;clip:rect(0,0,0,0);white-space:nowrap;border:0}
.upload-zone{min-height:150px;display:grid;place-items:center;padding:24px;text-align:center;border:1.5px dashed var(--border-strong);border-radius:14px;background:var(--surface-muted);cursor:pointer;transition:border-color 140ms ease,background-color 140ms ease,transform 140ms ease}
.upload-zone:hover,.upload-zone.dragging{border-color:var(--brand);background:var(--brand-soft);transform:translateY(-1px)}.upload-empty{display:grid;justify-items:center;gap:8px;color:var(--text-secondary)}.upload-icon{width:42px;height:34px;border:2px solid var(--border-strong);border-radius:8px;position:relative}.upload-icon:before{content:"";position:absolute;left:8px;right:8px;bottom:8px;height:10px;background:linear-gradient(135deg,transparent 45%,var(--border-strong) 46% 54%,transparent 55%)}.upload-empty strong{color:var(--text-primary);font-size:15px}.upload-empty span{font-size:13px}
.file-chip{display:none;width:100%;grid-template-columns:74px minmax(0,1fr);gap:12px;align-items:center;text-align:left}.file-thumb{width:74px;height:58px;object-fit:cover;border-radius:10px;border:1px solid var(--border);background:var(--surface-muted)}.file-details{min-width:0}.file-name{display:block;font-weight:700;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}.file-meta{display:block;font-size:13px;color:var(--text-secondary);margin-top:4px}.file-actions{grid-column:1/-1;display:flex;justify-content:flex-end;gap:8px;border-top:1px solid var(--border);padding-top:10px}.small-button,.icon-button{height:34px;border:1px solid var(--border-strong);border-radius:9px;background:var(--surface);color:var(--text-primary);font-weight:650}.small-button{padding:0 12px}.icon-button{width:34px;padding:0;font-size:18px;line-height:1}
.preset-grid{display:grid;grid-template-columns:1fr 1fr;gap:10px}.preset{position:relative;min-height:78px;border:1px solid var(--border);border-radius:var(--radius-md);background:var(--surface);padding:11px 11px 10px 12px;cursor:pointer;display:grid;grid-template-columns:28px minmax(0,1fr);gap:9px;transition:border-color 140ms ease,background-color 140ms ease,box-shadow 140ms ease}
.preset input{position:absolute;opacity:0;pointer-events:none}.preset:hover{border-color:var(--border-strong)}.preset:has(input:checked){border-color:var(--brand);background:var(--brand-soft);box-shadow:inset 0 0 0 1px var(--brand)}.preset:has(input:checked):after{content:"ok";position:absolute;right:9px;top:8px;font-size:10px;font-weight:800;color:var(--brand)}
.preset-shape{width:28px;height:22px;margin-top:2px;border:2px solid var(--brand);border-radius:5px;opacity:.85}.preset-shape.tall{height:28px}.preset-shape.square{height:24px;width:24px}.preset-shape.small{width:22px;height:18px}.preset strong{display:block;font-size:13px;color:var(--text-primary);padding-right:20px}.preset span{display:block;font-size:12px;color:var(--text-secondary);margin-top:3px}
.custom-field{display:none;grid-template-columns:minmax(0,1fr) auto;align-items:end;gap:8px}.custom-field.active{display:grid}.input-label{display:grid;gap:8px;font-size:13px;font-weight:650;color:var(--text-secondary)}.input-wrap{display:flex;align-items:center;gap:8px}.input-wrap input,.quality-number{height:42px;border:1px solid var(--border-strong);border-radius:10px;background:var(--surface);color:var(--text-primary);padding:0 12px}.input-wrap input{width:100%}.unit{color:var(--text-muted);font-size:13px;font-weight:650}
.quality-head{display:flex;align-items:center;justify-content:space-between;gap:12px}.quality-number{width:72px;text-align:center}input[type=range]{width:100%;accent-color:var(--brand)}.quality-scale{display:flex;justify-content:space-between;color:var(--text-muted);font-size:12px}.quality-section.inactive{opacity:.58}.checkbox-row{display:flex;align-items:center;gap:9px;color:var(--text-secondary);font-size:13px}.checkbox-row input{width:17px;height:17px;accent-color:var(--brand)}
.format-select{width:100%;height:44px;border:1px solid var(--border-strong);border-radius:10px;background:var(--surface);color:var(--text-primary);padding:0 12px}
.export-box{border-top:1px solid var(--border);padding-top:18px;display:grid;gap:12px}.summary{font-size:13px;color:var(--text-secondary);line-height:1.45}.primary-button{width:100%;height:50px;border:1px solid var(--brand);border-radius:11px;background:var(--brand);color:white;font-weight:750;font-size:15px;display:flex;align-items:center;justify-content:center;gap:9px}.primary-button:hover{background:var(--brand-hover);border-color:var(--brand-hover)}.spinner{display:none;width:16px;height:16px;border:2px solid rgba(255,255,255,.45);border-top-color:white;border-radius:999px;animation:spin 800ms linear infinite}.primary-button.busy .spinner{display:block}@keyframes spin{to{transform:rotate(360deg)}}
.alert{display:none;border-radius:11px;border:1px solid var(--border);padding:11px 12px;font-size:13px;line-height:1.45}.alert.show{display:block}.alert.success{background:var(--success-bg);border-color:var(--success-border);color:var(--success-text)}.alert.error{background:color-mix(in srgb,var(--danger) 12%,var(--surface));border-color:color-mix(in srgb,var(--danger) 35%,var(--border));color:var(--danger)}.alert-actions{display:flex;gap:8px;margin-top:10px}.alert-actions button{height:34px;border-radius:9px;background:var(--surface);border:1px solid var(--border-strong);color:var(--text-primary);font-weight:650;padding:0 10px}
.preview-card{min-height:560px;display:flex;flex-direction:column;overflow:hidden}.preview-toolbar{height:50px;border-bottom:1px solid var(--border);display:flex;align-items:center;justify-content:space-between;padding:0 16px}.preview-title{font-size:14px;font-weight:700}.preview-tools{display:flex;align-items:center;gap:8px;color:var(--text-muted);font-size:13px}.tool-chip{height:30px;border:1px solid var(--border);border-radius:8px;background:var(--surface);padding:0 9px;display:inline-flex;align-items:center}
.preview-canvas{flex:1;min-height:430px;display:grid;place-items:center;padding:32px;background:var(--surface-muted)}.empty-preview{text-align:center;color:var(--text-secondary);display:grid;justify-items:center;gap:8px}.empty-preview .upload-icon{width:58px;height:46px;background:var(--surface)}.empty-preview strong{font-size:16px;color:var(--text-primary)}.preview-image{display:none;max-width:100%;max-height:430px;object-fit:contain;border-radius:4px;box-shadow:0 14px 32px rgba(24,44,35,.16);background:var(--surface)}
.meta-footer{border-top:1px solid var(--border);background:var(--surface-muted);padding:16px;display:grid;grid-template-columns:1fr 1fr;gap:14px}.meta-box{display:grid;gap:6px}.meta-title{font-size:12px;text-transform:uppercase;letter-spacing:.04em;color:var(--text-muted);font-weight:700}.meta-main{font-size:15px;font-weight:700}.meta-sub{font-size:13px;color:var(--text-secondary)}
@media(max-width:980px){.workspace{grid-template-columns:minmax(320px,380px) minmax(0,1fr)}.container{padding:28px 20px 40px}}
@media(max-width:760px){.app-header{height:64px}.header-inner{padding:0 16px}.logo-subtitle,.user-pill{display:none}.container{padding:20px 16px 32px}.heading-row{display:block;margin-bottom:18px}.state-label{padding-top:10px}.workspace{grid-template-columns:1fr}.controls-card{padding:18px}.preview-card{min-height:420px}.preview-canvas{min-height:320px;padding:20px}.preview-image{max-height:320px}.preset-grid{grid-template-columns:1fr 1fr}.meta-footer{grid-template-columns:1fr}}
@media(max-width:430px){.preset-grid{grid-template-columns:1fr}.file-chip{grid-template-columns:58px minmax(0,1fr);gap:10px}.file-thumb{width:58px;height:48px}}
</style>
</head>
<body>
<header class="app-header">
<div class="header-inner">
<div class="brand"><div class="mark" aria-hidden="true"></div><div><div class="logo-name">webfit</div><div class="logo-subtitle">Web-ready image resizing</div></div></div>
<nav class="header-actions" aria-label="Application actions"><span class="user-pill">Admin</span><button id="themeToggle" class="ghost-button" type="button" aria-label="Toggle color theme">Theme</button><a class="ghost-button" href="/logout" aria-label="Log out">Log out</a></nav>
</div>
</header>
<main class="container">
<div class="heading-row">
<div><h1>Prepare image</h1><p class="page-description">Resize and optimize an image for websites, cards, social posts, or custom layouts.</p></div>
<div id="pageState" class="state-label">No image selected</div>
</div>
<div class="workspace">
<form id="form" class="controls-card" aria-describedby="status">
<section class="section">
<div><div class="section-title">Upload image</div><p class="section-help">Drop an image here or click to browse.</p></div>
<input id="image" class="visually-hidden" name="image" type="file" accept="image/png,image/jpeg,image/webp" required>
<div id="dropzone" class="upload-zone" role="button" tabindex="0" aria-label="Choose image to upload">
<span id="uploadEmpty" class="upload-empty"><span class="upload-icon" aria-hidden="true"></span><strong>Drop an image here</strong><span>or click to browse</span><span>PNG, JPG, or WebP</span></span>
<span id="fileChip" class="file-chip"><img id="fileThumb" class="file-thumb" alt=""><span class="file-details"><span id="fileName" class="file-name"></span><span id="fileMeta" class="file-meta"></span></span><span class="file-actions"><button id="replaceFile" class="small-button" type="button">Replace</button><button id="removeFile" class="icon-button" type="button" aria-label="Remove selected image">&times;</button></span></span>
</div>
</section>
<section class="section">
<div><div class="section-title">Output size</div><p class="section-help">Choose where this image will be used.</p></div>
<div class="preset-grid" role="radiogroup" aria-label="Output size">
<label class="preset"><input type="radio" name="preset" value="hero" checked><span class="preset-shape"></span><span><strong>Hero</strong><span>1920 px</span></span></label>
<label class="preset"><input type="radio" name="preset" value="wide"><span class="preset-shape"></span><span><strong>Wide content</strong><span>1440 px</span></span></label>
<label class="preset"><input type="radio" name="preset" value="card"><span class="preset-shape"></span><span><strong>Card</strong><span>1200 px</span></span></label>
<label class="preset"><input type="radio" name="preset" value="social"><span class="preset-shape square"></span><span><strong>Social</strong><span>1080 px</span></span></label>
<label class="preset"><input type="radio" name="preset" value="article"><span class="preset-shape"></span><span><strong>Article</strong><span>960 px</span></span></label>
<label class="preset"><input type="radio" name="preset" value="thumb"><span class="preset-shape small"></span><span><strong>Thumbnail</strong><span>480 px</span></span></label>
<label class="preset"><input type="radio" name="preset" value="icon"><span class="preset-shape square small"></span><span><strong>Icon</strong><span>256 px</span></span></label>
<label class="preset"><input type="radio" name="preset" value="custom"><span class="preset-shape tall"></span><span><strong>Custom</strong><span>Manual width</span></span></label>
</div>
<div id="customField" class="custom-field"><label class="input-label">Custom width<span class="input-wrap"><input id="width" name="width" type="number" min="64" max="4000" step="1" value="1360"><span class="unit">px</span></span></label><label class="checkbox-row"><input id="ratio" type="checkbox" checked>Preserve aspect ratio</label></div>
</section>
<section class="section">
<div><div class="section-title">Output format</div><p class="section-help">Choose the file type to download.</p></div>
<select id="format" class="format-select" name="format" aria-label="Output image format">
<option value="webp" selected>WebP — lossless and web-ready</option>
<option value="jpeg">JPEG — photos and broad compatibility</option>
<option value="png">PNG — transparency and lossless graphics</option>
</select>
</section>
<section id="qualitySection" class="section quality-section">
<div class="quality-head"><div><div class="section-title">JPEG quality</div><p id="qualityHelp" class="section-help"><span id="qualityText">82</span> is a good balance for most website images.</p></div><input id="qualityNumber" class="quality-number" type="number" min="40" max="100" value="82" aria-label="JPEG quality value"></div>
<input id="quality" name="quality" type="range" min="40" max="100" value="82" aria-label="Image quality">
<div class="quality-scale"><span>Smaller file</span><span>Better quality</span></div>
</section>
<section class="export-box">
<div id="summary" class="summary">Select an image to continue.</div>
<div id="status" class="alert" aria-live="polite"></div>
<button id="button" class="primary-button" disabled><span class="spinner" aria-hidden="true"></span><span id="buttonText">Select an image to continue</span></button>
</section>
</form>
<section class="preview-card" aria-label="Preview">
<div class="preview-toolbar"><div class="preview-title">Preview</div><div class="preview-tools"><span class="tool-chip">Fit</span><span id="zoomLabel">100%</span></div></div>
<div class="preview-canvas">
<div id="emptyPreview" class="empty-preview"><span class="upload-icon" aria-hidden="true"></span><strong>Your preview will appear here</strong><span>Upload an image to inspect the resized result before downloading.</span></div>
<img id="previewImage" class="preview-image" alt="Selected image preview">
</div>
<div class="meta-footer">
<div class="meta-box"><span class="meta-title">Original</span><span id="originalMeta" class="meta-main">No image</span><span id="originalSub" class="meta-sub">Select a PNG, JPG, or WebP</span></div>
<div class="meta-box"><span class="meta-title">Output</span><span id="outputMeta" class="meta-main">Not calculated</span><span id="outputSub" class="meta-sub">Calculated after resize</span></div>
</div>
</section>
</div>
</main>
<script>
const form=document.getElementById("form"),imageInput=document.getElementById("image"),dropzone=document.getElementById("dropzone"),uploadEmpty=document.getElementById("uploadEmpty"),fileChip=document.getElementById("fileChip"),fileThumb=document.getElementById("fileThumb"),fileName=document.getElementById("fileName"),fileMeta=document.getElementById("fileMeta"),replaceFile=document.getElementById("replaceFile"),removeFile=document.getElementById("removeFile"),customField=document.getElementById("customField"),widthInput=document.getElementById("width"),format=document.getElementById("format"),qualitySection=document.getElementById("qualitySection"),qualityHelp=document.getElementById("qualityHelp"),quality=document.getElementById("quality"),qualityNumber=document.getElementById("qualityNumber"),summary=document.getElementById("summary"),statusEl=document.getElementById("status"),button=document.getElementById("button"),buttonText=document.getElementById("buttonText"),pageState=document.getElementById("pageState"),emptyPreview=document.getElementById("emptyPreview"),previewImage=document.getElementById("previewImage"),originalMeta=document.getElementById("originalMeta"),originalSub=document.getElementById("originalSub"),outputMeta=document.getElementById("outputMeta"),outputSub=document.getElementById("outputSub"),ratio=document.getElementById("ratio"),themeToggle=document.getElementById("themeToggle");
const presets={hero:{label:"Hero",width:1920},wide:{label:"Wide content",width:1440},card:{label:"Card",width:1200},social:{label:"Social",width:1080},article:{label:"Article",width:960},thumb:{label:"Thumbnail",width:480},icon:{label:"Icon",width:256}};
let currentFile=null,currentObjectURL="",downloadURL="",downloadName="";
function setThemeLabel(){themeToggle.textContent=document.documentElement.dataset.theme==="dark"?"Light mode":"Dark mode";}
themeToggle.addEventListener("click",()=>{const theme=document.documentElement.dataset.theme==="dark"?"light":"dark";document.documentElement.dataset.theme=theme;localStorage.setItem("webfit-theme",theme);setThemeLabel();});
matchMedia("(prefers-color-scheme: dark)").addEventListener("change",event=>{if(!localStorage.getItem("webfit-theme")){document.documentElement.dataset.theme=event.matches?"dark":"light";setThemeLabel();}});
setThemeLabel();
function bytes(n){if(!n)return "0 B";if(n<1024)return n+" B";const u=["KiB","MiB","GiB"];let v=n,i=-1;do{v/=1024;i++}while(v>=1024&&i<u.length-1);return v.toFixed(1)+" "+u[i];}
function escapeHtml(value){return String(value).replace(/[&<>"']/g,c=>({"&":"&amp;","<":"&lt;",">":"&gt;","\"":"&quot;","'":"&#39;"}[c]));}
function selectedPreset(){return document.querySelector('input[name="preset"]:checked').value;}
function targetWidth(){const preset=selectedPreset();return preset==="custom" ? Number(widthInput.value||0) : presets[preset].width;}
function targetLabel(){const preset=selectedPreset();return preset==="custom" ? "Custom" : presets[preset].label;}
function outputDims(){if(!currentFile||!currentFile.width)return null;const width=targetWidth();if(!width)return null;const outWidth=Math.min(currentFile.width,width);const outHeight=Math.max(1,Math.round(currentFile.height*(outWidth/currentFile.width)));return {width:outWidth,height:outHeight};}
function setStatus(type,text,html){statusEl.className="alert"+(type ? " "+type+" show" : "");statusEl.textContent=text||"";if(html)statusEl.innerHTML=html;}
function syncQuality(from){if(from==="range")qualityNumber.value=quality.value;else quality.value=qualityNumber.value;if(format.value==="jpeg")qualityHelp.textContent=quality.value+" is a good balance for most website images.";updateSummary();}
function syncFormat(){const active=format.value==="jpeg";quality.disabled=!active;qualityNumber.disabled=!active;qualitySection.classList.toggle("inactive",!active);qualityHelp.textContent=active?quality.value+" is a good balance for most website images.":"PNG and WebP exports use lossless encoding.";updateSummary();}
function syncCustom(){customField.classList.toggle("active",selectedPreset()==="custom");updateSummary();}
function recommend(width){if(width>=2400)return "hero";if(width>=1800)return "wide";if(width>=1300)return "card";if(width>=1050)return "article";if(width>=620)return "thumb";return "icon";}
function setPreset(id){const input=document.querySelector('input[name="preset"][value="'+id+'"]');if(input){input.checked=true;syncCustom();}}
function updateSummary(){const dims=outputDims();if(!currentFile){summary.textContent="Select an image to continue.";button.disabled=true;buttonText.textContent="Select an image to continue";pageState.textContent="No image selected";outputMeta.textContent="Not calculated";outputSub.textContent="Calculated after resize";return;}button.disabled=false;buttonText.textContent="Resize and download";pageState.textContent=currentFile.name;summary.textContent=(dims?dims.width+" px wide":"Choose a valid width")+" - "+outputTypeLabel()+(format.value==="jpeg"?" - Quality "+quality.value:" - Lossless")+" - "+targetLabel();originalMeta.textContent=currentFile.width+" x "+currentFile.height;originalSub.textContent=bytes(currentFile.size)+" - "+currentFile.typeLabel;outputMeta.textContent=dims?dims.width+" x "+dims.height:"Invalid width";outputSub.textContent=outputTypeLabel()+" export";}
function fileTypeLabel(file){if(file.type==="image/png")return "PNG";if(file.type==="image/webp")return "WebP";return "JPEG";}
function outputTypeLabel(){return format.value==="webp"?"WebP":format.value==="png"?"PNG":"JPEG";}
function setFile(file){if(!file)return;if(!/^image\/(png|jpeg|webp)$/.test(file.type)){setStatus("error","This file type is not supported. Choose a PNG, JPG, or WebP image.");return;}if(currentObjectURL)URL.revokeObjectURL(currentObjectURL);currentObjectURL=URL.createObjectURL(file);const img=new Image();img.onload=()=>{currentFile={file:file,name:file.name,size:file.size,typeLabel:fileTypeLabel(file),width:img.naturalWidth,height:img.naturalHeight,url:currentObjectURL};fileThumb.src=currentObjectURL;previewImage.src=currentObjectURL;previewImage.style.display="block";emptyPreview.style.display="none";fileName.textContent=file.name;fileMeta.textContent=img.naturalWidth+" x "+img.naturalHeight+" - "+bytes(file.size);uploadEmpty.style.display="none";fileChip.style.display="grid";setPreset(recommend(img.naturalWidth));setStatus("","");updateSummary();};img.onerror=()=>{setStatus("error","The selected image could not be processed. Choose a valid PNG, JPG, or WebP image.");};img.src=currentObjectURL;}
function clearFile(){imageInput.value="";currentFile=null;if(currentObjectURL)URL.revokeObjectURL(currentObjectURL);currentObjectURL="";uploadEmpty.style.display="grid";fileChip.style.display="none";previewImage.style.display="none";previewImage.removeAttribute("src");emptyPreview.style.display="grid";setStatus("","");updateSummary();}
dropzone.addEventListener("click",()=>imageInput.click());
dropzone.addEventListener("keydown",e=>{if(e.key==="Enter"||e.key===" "){e.preventDefault();imageInput.click();}});
dropzone.addEventListener("dragover",e=>{e.preventDefault();dropzone.classList.add("dragging");});
dropzone.addEventListener("dragleave",()=>dropzone.classList.remove("dragging"));
dropzone.addEventListener("drop",e=>{e.preventDefault();dropzone.classList.remove("dragging");const file=e.dataTransfer.files&&e.dataTransfer.files[0];if(file){imageInput.files=e.dataTransfer.files;setFile(file);}});
imageInput.addEventListener("change",()=>setFile(imageInput.files&&imageInput.files[0]));
removeFile.addEventListener("click",e=>{e.preventDefault();e.stopPropagation();clearFile();});
replaceFile.addEventListener("click",e=>{e.preventDefault();e.stopPropagation();imageInput.click();});
document.querySelectorAll('input[name="preset"]').forEach(input=>input.addEventListener("change",syncCustom));
quality.addEventListener("input",()=>syncQuality("range"));qualityNumber.addEventListener("input",()=>syncQuality("number"));widthInput.addEventListener("input",updateSummary);format.addEventListener("change",syncFormat);
ratio.addEventListener("change",()=>{if(!ratio.checked){ratio.checked=true;setStatus("error","Webfit preserves aspect ratio to avoid distorted exports.");}});
form.addEventListener("submit",async e=>{e.preventDefault();if(!currentFile)return;const width=targetWidth();if(width<64||width>4000){setStatus("error","Enter a width between 64 and 4000 pixels.");return;}button.disabled=true;button.classList.add("busy");buttonText.textContent="Preparing image...";setStatus("","");try{const data=new FormData(form);const res=await fetch("/resize",{method:"POST",body:data});if(!res.ok)throw new Error("The selected image could not be processed. Check the file and output settings.");const blob=await res.blob();const disposition=res.headers.get("Content-Disposition")||"";const match=/filename="([^"]+)"/.exec(disposition);downloadName=match?match[1]:"webfit-image";if(downloadURL)URL.revokeObjectURL(downloadURL);downloadURL=URL.createObjectURL(blob);const a=document.createElement("a");a.href=downloadURL;a.download=downloadName;document.body.appendChild(a);a.click();a.remove();outputSub.textContent=bytes(Number(res.headers.get("X-Webfit-Output-Size")||blob.size))+" - "+outputTypeLabel();setStatus("success","",'<strong>Image ready</strong><br>Downloaded as '+escapeHtml(downloadName)+'<div class="alert-actions"><button type="button" id="again">Download again</button><button type="button" id="another">Resize another image</button></div>');document.getElementById("again").onclick=()=>{const a=document.createElement("a");a.href=downloadURL;a.download=downloadName;document.body.appendChild(a);a.click();a.remove();};document.getElementById("another").onclick=clearFile;}catch(err){setStatus("error",err.message.trim());}finally{button.disabled=false;button.classList.remove("busy");buttonText.textContent=currentFile?"Resize and download":"Select an image to continue";}});
syncCustom();syncFormat();
</script>
</body>
</html>`
