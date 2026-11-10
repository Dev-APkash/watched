package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAssets(t *testing.T) {
	a := &app{cfg: config{dataDir: t.TempDir()}, tr: newTrakt("")}
	h := a.routes()
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}

	for path, wantType := range map[string]string{
		"/assets/cabin.webp":         "image/webp",
		"/assets/cabin-still.jpg":    "image/jpeg",
		"/assets/fonts.css":          "text/css",
		"/assets/font-figtree.woff2": "font/woff2",
	} {
		rec := get(path)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status %d, want 200", path, rec.Code)
			continue
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, wantType) {
			t.Errorf("%s: Content-Type %q, want %s", path, ct, wantType)
		}
	}

	// Every font the stylesheet names must be shipped.
	css := get("/assets/fonts.css").Body.String()
	for _, part := range strings.Split(css, "url(")[1:] {
		name := part[:strings.Index(part, ")")]
		if rec := get("/assets/" + name); rec.Code != http.StatusOK {
			t.Errorf("fonts.css references %s, which returns %d", name, rec.Code)
		}
	}

	for _, path := range []string{"/assets/missing.png", "/assets/.hidden", "/assets/..%2fmain.go"} {
		if rec := get(path); rec.Code == http.StatusOK {
			t.Errorf("%s: status 200, want an error", path)
		}
	}
}
