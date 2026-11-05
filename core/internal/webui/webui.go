// Package webui serves the single-page web app embedded into the binary
// (see package github.com/orglane/orglane/web).
//
//   - Files of the build are served as they are. Vite's hashed files under
//     /assets/ never change and are cached for a year (immutable); everything
//     else, index.html above all, is revalidated on every use (no-cache) by
//     its ETag.
//   - Any other path is a route of the app and gets index.html (SPA
//     fallback), except paths that belong to the core (/api/, /oidc/,
//     /.well-known/) or look like a missing file, which get 404.
//   - Every response carries a Content-Security-Policy that allows only this
//     origin for resources, with a form-only exception for GitHub App manifests.
//   - Text files are gzip-compressed once at startup.
package webui

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"
)

// ContentSecurityPolicy allows only same-origin resources. GitHub App manifest
// forms may POST to the GitHub organizations path; no other cross-origin form
// destination is allowed. Inline scripts
// and styles are refused, so the app loads its theme script as a file and
// avoids libraries that inject <style> elements.
const ContentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self'; " +
	"font-src 'self'; connect-src 'self'; manifest-src 'self'; worker-src 'self'; media-src 'self'; " +
	"object-src 'none'; base-uri 'none'; form-action 'self' https://github.com/organizations/; frame-ancestors 'none'"

// Cache-Control values.
const (
	CacheImmutable  = "public, max-age=31536000, immutable"
	CacheRevalidate = "no-cache"
)

// reserved path prefixes belong to the core, never to the app.
var reserved = []string{"/api/", "/oidc/", "/.well-known/"}

type file struct {
	name      string // path inside the build, without the leading slash
	ctype     string
	etag      string
	gz        []byte // nil when not worth compressing
	gzETag    string
	immutable bool
}

// Handler serves the web UI.
type Handler struct {
	fsys  fs.FS
	files map[string]*file // by URL path
	index *file
}

// New loads the build in fsys. A build without index.html is accepted: the
// handler then explains that the UI was not built.
func New(fsys fs.FS) (*Handler, error) {
	h := &Handler{fsys: fsys, files: map[string]*file{}}
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || strings.HasPrefix(path.Base(p), ".") {
			return nil
		}
		body, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		f := &file{name: p, ctype: contentType(p), etag: etag(body), immutable: strings.HasPrefix(p, "assets/")}
		if compressible(f.ctype) && len(body) > 1024 {
			gz, err := gzipped(body)
			if err != nil {
				return err
			}
			if len(gz) < len(body)*9/10 {
				f.gz, f.gzETag = gz, strings.TrimSuffix(f.etag, `"`)+`-gz"`
			}
		}
		h.files["/"+p] = f
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("load the web UI: %w", err)
	}
	h.index = h.files["/index.html"]
	return h, nil
}

// Built reports whether the UI was built into the binary.
func (h *Handler) Built() bool { return h.index != nil }

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	hdr := w.Header()
	hdr.Set("Content-Security-Policy", ContentSecurityPolicy)
	hdr.Set("X-Content-Type-Options", "nosniff")
	hdr.Set("X-Frame-Options", "DENY")
	hdr.Set("Referrer-Policy", "strict-origin-when-cross-origin")
	hdr.Set("Cross-Origin-Opener-Policy", "same-origin")
	hdr.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		hdr.Set("Allow", "GET, HEAD")
		http.Error(w, "Method not allowed.", http.StatusMethodNotAllowed)
		return
	}
	p := path.Clean("/" + r.URL.Path)
	if f, ok := h.files[p]; ok {
		h.serve(w, r, f)
		return
	}
	if !h.isRoute(p) {
		hdr.Set("Cache-Control", CacheRevalidate)
		http.Error(w, "Not found.", http.StatusNotFound)
		return
	}
	if h.index == nil {
		hdr.Set("Cache-Control", "no-store")
		http.Error(w, "The web UI is not part of this build. Run `make build` (or `pnpm --filter @orglane/web build` before `go build`).",
			http.StatusServiceUnavailable)
		return
	}
	h.serve(w, r, h.index)
}

// isRoute reports whether p can be a route of the app.
func (h *Handler) isRoute(p string) bool {
	for _, prefix := range reserved {
		if strings.HasPrefix(p+"/", prefix) {
			return false
		}
	}
	return !strings.HasPrefix(p+"/", "/assets/") && path.Ext(p) == ""
}

func (h *Handler) serve(w http.ResponseWriter, r *http.Request, f *file) {
	hdr := w.Header()
	hdr.Set("Content-Type", f.ctype)
	if f.immutable {
		hdr.Set("Cache-Control", CacheImmutable)
	} else {
		hdr.Set("Cache-Control", CacheRevalidate)
	}
	var body io.ReadSeeker
	if f.gz != nil {
		hdr.Add("Vary", "Accept-Encoding")
	}
	if f.gz != nil && acceptsGzip(r) {
		hdr.Set("Content-Encoding", "gzip")
		hdr.Set("ETag", f.gzETag)
		body = bytes.NewReader(f.gz)
	} else {
		hdr.Set("ETag", f.etag)
		raw, err := h.fsys.Open(f.name)
		if err != nil {
			http.Error(w, "Not found.", http.StatusNotFound)
			return
		}
		defer func() { _ = raw.Close() }()
		rs, ok := raw.(io.ReadSeeker)
		if !ok {
			http.Error(w, "Internal error.", http.StatusInternalServerError)
			return
		}
		body = rs
	}
	// The ETag drives conditional requests; embedded files have no
	// modification time.
	http.ServeContent(w, r, "", time.Time{}, body)
}

// acceptsGzip reports whether the client takes gzip (RFC 9110, section
// 12.5.3): listed, and not with q=0.
func acceptsGzip(r *http.Request) bool {
	for part := range strings.SplitSeq(r.Header.Get("Accept-Encoding"), ",") {
		coding, params, _ := strings.Cut(part, ";")
		if !strings.EqualFold(strings.TrimSpace(coding), "gzip") {
			continue
		}
		for param := range strings.SplitSeq(params, ";") {
			if k, v, ok := strings.Cut(strings.TrimSpace(param), "="); ok && strings.EqualFold(k, "q") {
				q, err := strconv.ParseFloat(v, 64)
				return err == nil && q > 0
			}
		}
		return true
	}
	return false
}

func etag(body []byte) string {
	sum := sha256.Sum256(body)
	return `"` + base64.RawURLEncoding.EncodeToString(sum[:12]) + `"`
}

func gzipped(body []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	if _, err := zw.Write(body); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

var types = map[string]string{
	".html":        "text/html; charset=utf-8",
	".js":          "text/javascript; charset=utf-8",
	".mjs":         "text/javascript; charset=utf-8",
	".css":         "text/css; charset=utf-8",
	".json":        "application/json",
	".svg":         "image/svg+xml",
	".png":         "image/png",
	".ico":         "image/x-icon",
	".webp":        "image/webp",
	".woff2":       "font/woff2",
	".woff":        "font/woff",
	".txt":         "text/plain; charset=utf-8",
	".webmanifest": "application/manifest+json",
	".map":         "application/json",
}

func contentType(p string) string {
	ext := strings.ToLower(path.Ext(p))
	if t, ok := types[ext]; ok {
		return t
	}
	if t := mime.TypeByExtension(ext); t != "" {
		return t
	}
	return "application/octet-stream"
}

func compressible(ctype string) bool {
	return strings.HasPrefix(ctype, "text/") || strings.HasPrefix(ctype, "image/svg") ||
		strings.HasPrefix(ctype, "application/json") || strings.HasPrefix(ctype, "application/manifest")
}
