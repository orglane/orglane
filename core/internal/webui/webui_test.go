package webui_test

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/orglane/orglane/core/internal/webui"
)

var indexHTML = `<!doctype html><html><head><script src="/theme-init.js"></script></head><body><div id="root"></div>` +
	`<script type="module" src="/assets/index-abc123.js"></script>` + strings.Repeat("<!-- padding -->", 100) + `</body></html>`

func build() fstest.MapFS {
	return fstest.MapFS{
		"index.html":                 {Data: []byte(indexHTML)},
		"theme-init.js":              {Data: []byte("(() => {})();")},
		"favicon.svg":                {Data: []byte("<svg/>")},
		"assets/index-abc123.js":     {Data: []byte(strings.Repeat("console.log('orglane');\n", 200))},
		"assets/index-def456.css":    {Data: []byte("body{}")},
		"assets/inter-latin-1.woff2": {Data: []byte{0x77, 0x4f, 0x46, 0x32}},
		".gitkeep":                   {Data: nil},
	}
}

func newHandler(t *testing.T, fsys fstest.MapFS) *webui.Handler {
	t.Helper()
	h, err := webui.New(fsys)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func get(t *testing.T, h http.Handler, path string, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
	for i := 0; i+1 < len(headers); i += 2 {
		r.Header.Set(headers[i], headers[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestSPAFallbackServesIndexForRoutes(t *testing.T) {
	h := newHandler(t, build())
	for _, p := range []string{"/", "/sign-in", "/settings/security", "/people/", "/welcome/passkey?redirect=%2Finbox", "/index.html"} {
		w := get(t, h, p)
		if w.Code != http.StatusOK || w.Body.String() != indexHTML {
			t.Errorf("GET %s = %d %q, want index.html", p, w.Code, w.Body.String())
			continue
		}
		if got := w.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
			t.Errorf("GET %s Content-Type = %q", p, got)
		}
		if got := w.Header().Get("Cache-Control"); got != webui.CacheRevalidate {
			t.Errorf("GET %s Cache-Control = %q, want %q", p, got, webui.CacheRevalidate)
		}
		if w.Header().Get("ETag") == "" {
			t.Errorf("GET %s has no ETag", p)
		}
	}
}

func TestHashedAssetsAreImmutable(t *testing.T) {
	h := newHandler(t, build())
	cases := map[string]string{
		"/assets/index-abc123.js":     "text/javascript; charset=utf-8",
		"/assets/index-def456.css":    "text/css; charset=utf-8",
		"/assets/inter-latin-1.woff2": "font/woff2",
	}
	for p, ctype := range cases {
		w := get(t, h, p)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", p, w.Code)
		}
		if got := w.Header().Get("Cache-Control"); got != webui.CacheImmutable {
			t.Errorf("GET %s Cache-Control = %q, want %q", p, got, webui.CacheImmutable)
		}
		if got := w.Header().Get("Content-Type"); got != ctype {
			t.Errorf("GET %s Content-Type = %q, want %q", p, got, ctype)
		}
	}
	// Unhashed public files are revalidated.
	if got := get(t, h, "/theme-init.js").Header().Get("Cache-Control"); got != webui.CacheRevalidate {
		t.Errorf("theme-init.js Cache-Control = %q", got)
	}
}

func TestMissingFilesAndCorePathsAre404(t *testing.T) {
	h := newHandler(t, build())
	for _, p := range []string{"/assets/index-gone.js", "/assets/", "/robots.txt", "/api/v1/nope", "/oidc/nope", "/.well-known/nope", "/.gitkeep", "/api"} {
		if w := get(t, h, p); w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), "<html") {
			t.Errorf("GET %s = %d %q, want a plain 404", p, w.Code, w.Body.String())
		}
	}
}

func TestSecurityHeaders(t *testing.T) {
	h := newHandler(t, build())
	for _, p := range []string{"/", "/assets/index-abc123.js", "/nope.txt"} {
		w := get(t, h, p)
		csp := w.Header().Get("Content-Security-Policy")
		for _, want := range []string{"default-src 'self'", "script-src 'self'", "style-src 'self'", "frame-ancestors 'none'", "object-src 'none'", "form-action 'self' https://github.com/organizations/"} {
			if !strings.Contains(csp, want) {
				t.Errorf("GET %s CSP %q lacks %q", p, csp, want)
			}
		}
		// Only the manifest form may leave the origin. Resource loads and
		// all other form destinations retain the original restrictions.
		resourcePolicy := strings.Replace(csp, "form-action 'self' https://github.com/organizations/;", "", 1)
		if strings.Count(csp, "form-action") != 1 {
			t.Errorf("GET %s has ambiguous form destinations: %q", p, csp)
		}
		if strings.Contains(resourcePolicy, "unsafe-") || strings.Contains(resourcePolicy, "http") || strings.Contains(resourcePolicy, "data:") {
			t.Errorf("GET %s CSP allows more than self: %q", p, csp)
		}
		if w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("X-Frame-Options") != "DENY" {
			t.Errorf("GET %s headers = %v", p, w.Header())
		}
	}
}

func TestConditionalRequests(t *testing.T) {
	h := newHandler(t, build())
	first := get(t, h, "/inbox")
	if w := get(t, h, "/inbox", "If-None-Match", first.Header().Get("ETag")); w.Code != http.StatusNotModified || w.Body.Len() != 0 {
		t.Fatalf("revalidation = %d %q, want 304", w.Code, w.Body.String())
	}
}

func TestGzip(t *testing.T) {
	h := newHandler(t, build())
	plain := get(t, h, "/assets/index-abc123.js")
	w := get(t, h, "/assets/index-abc123.js", "Accept-Encoding", "br, gzip")
	if w.Header().Get("Content-Encoding") != "gzip" || w.Header().Get("Vary") != "Accept-Encoding" {
		t.Fatalf("headers = %v", w.Header())
	}
	if w.Header().Get("ETag") == plain.Header().Get("ETag") {
		t.Error("the gzip variant shares the ETag of the plain one")
	}
	zr, err := gzip.NewReader(bytes.NewReader(w.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(zr)
	if err != nil || string(body) != plain.Body.String() {
		t.Fatalf("decompressed body differs: %v", err)
	}
	if w := get(t, h, "/assets/index-abc123.js", "Accept-Encoding", "gzip;q=0"); w.Header().Get("Content-Encoding") != "" {
		t.Error("gzip;q=0 still got gzip")
	}
	// Tiny and binary files are never compressed.
	if w := get(t, h, "/assets/inter-latin-1.woff2", "Accept-Encoding", "gzip"); w.Header().Get("Content-Encoding") != "" {
		t.Error("compressed a font")
	}
}

func TestMethods(t *testing.T) {
	h := newHandler(t, build())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/sign-in", nil))
	if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("POST = %d %v", w.Code, w.Header())
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodHead, "/", nil))
	if w.Code != http.StatusOK || w.Body.Len() != 0 {
		t.Fatalf("HEAD = %d %q", w.Code, w.Body.String())
	}
}

func TestNotBuilt(t *testing.T) {
	h := newHandler(t, fstest.MapFS{".gitkeep": {}})
	if h.Built() {
		t.Fatal("an empty build reports Built")
	}
	w := get(t, h, "/")
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "make build") {
		t.Fatalf("GET / without a build = %d %q", w.Code, w.Body.String())
	}
}
