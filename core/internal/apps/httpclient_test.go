package apps

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTTPClientRefusesOversizedBodies(t *testing.T) {
	sizes := map[string]int{"/exact": MaxResponseBytes, "/over": MaxResponseBytes + 1}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("a", sizes[r.URL.Path]))
	}))
	defer srv.Close()
	client := NewHTTPClient()
	read := func(path string) (int, error) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = res.Body.Close() }()
		b, err := io.ReadAll(res.Body)
		return len(b), err
	}
	if n, err := read("/exact"); err != nil || n != MaxResponseBytes {
		t.Fatalf("a body at the limit = %d bytes, %v", n, err)
	}
	if _, err := read("/over"); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("a body over the limit = %v, want ErrResponseTooLarge", err)
	}
}
