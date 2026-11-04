package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunReleaseVersion(t *testing.T) {
	tests := []struct {
		tag     string
		want    string
		wantErr bool
	}{
		{tag: "v0.2.1", want: "version=0.2.1\nprerelease=false\n"},
		{tag: "v0.3.0-rc.1", want: "version=0.3.0-rc.1\nprerelease=true\n"},
		{tag: "main", wantErr: true},
		{tag: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.tag, func(t *testing.T) {
			output := filepath.Join(t.TempDir(), "output")
			var out bytes.Buffer
			err := run(t.Context(), []string{"release-version", "-tag", tt.tag, "-github-output", output}, nil, &out)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("release-version %q succeeded, want an error:\n%s", tt.tag, out.String())
				}
				if _, statErr := os.Stat(output); !os.IsNotExist(statErr) {
					t.Errorf("outputs were written for an invalid tag")
				}
				return
			}
			if err != nil {
				t.Fatalf("release-version %s: %v\n%s", tt.tag, err, out.String())
			}
			got, err := os.ReadFile(output) //nolint:gosec // test file in a temp dir
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Errorf("outputs = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestRunReleaseAliases runs the command against a registry that already has
// a newer release than the one being promoted: the aliases follow the
// registry, not the release.
func TestRunReleaseAliases(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			if user, pass, ok := r.BasicAuth(); !ok || user != "bot" || pass != "secret" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "tok"})
		case "/v2/orglane/orglane/tags/list":
			if r.Header.Get("Authorization") != "Bearer tok" {
				w.Header().Set("WWW-Authenticate", `Bearer realm="`+srv.URL+`/token",service="test",scope="repository:orglane/orglane:pull"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"tags": []string{
				"0.1.0", "0.1", "latest", "0.2.0", "0.1.1", "0.3.0-rc.1",
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	t.Setenv("REGISTRY_USERNAME", "bot")
	t.Setenv("REGISTRY_PASSWORD", "secret")

	output := filepath.Join(t.TempDir(), "output")
	var out bytes.Buffer
	err := run(t.Context(), []string{"release-aliases", "-image", "ghcr.io/orglane/orglane",
		"-registry-url", srv.URL, "-github-output", output}, nil, &out)
	if err != nil {
		t.Fatalf("release-aliases: %v\n%s", err, out.String())
	}
	got, err := os.ReadFile(output) //nolint:gosec // test file in a temp dir
	if err != nil {
		t.Fatal(err)
	}
	if want := "aliases<<EOF\nlatest 0.2.0\n0.2 0.2.0\n0.1 0.1.1\nEOF\n"; string(got) != want {
		t.Errorf("outputs = %q, want %q", got, want)
	}
	if !strings.Contains(out.String(), "6 tags, 3 aliases") {
		t.Errorf("report = %q", out.String())
	}
}

// TestRunReleaseAliasesForPairedImages: with two images the plan comes from
// the versions published for both. 0.2.0 is published for the core only (its
// agent leg failed), 0.3.0 for the agent only (its core leg still runs).
func TestRunReleaseAliasesForPairedImages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/orglane/orglane/tags/list":
			_ = json.NewEncoder(w).Encode(map[string]any{"tags": []string{"0.1.0", "0.1.1", "0.2.0", "latest"}})
		case "/v2/orglane/orglane-agent/tags/list":
			_ = json.NewEncoder(w).Encode(map[string]any{"tags": []string{"0.1.0", "0.1.1", "0.3.0"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	output := filepath.Join(t.TempDir(), "output")
	var out bytes.Buffer
	err := run(t.Context(), []string{"release-aliases", "-image", "ghcr.io/orglane/orglane", "-image", "ghcr.io/orglane/orglane-agent",
		"-registry-url", srv.URL, "-github-output", output}, nil, &out)
	if err != nil {
		t.Fatalf("release-aliases: %v\n%s", err, out.String())
	}
	got, err := os.ReadFile(output) //nolint:gosec // test file in a temp dir
	if err != nil {
		t.Fatal(err)
	}
	if want := "aliases<<EOF\nlatest 0.1.1\n0.1 0.1.1\nEOF\n"; string(got) != want {
		t.Errorf("outputs = %q, want %q", got, want)
	}
	if !strings.Contains(out.String(), "2 tags published for every image, 2 aliases") {
		t.Errorf("report = %q", out.String())
	}

	// An image whose tags can't be listed fails the whole plan.
	err = run(t.Context(), []string{"release-aliases", "-image", "ghcr.io/orglane/orglane", "-image", "ghcr.io/orglane/missing",
		"-registry-url", srv.URL, "-github-output", filepath.Join(t.TempDir(), "output")}, nil, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "ghcr.io/orglane/missing") {
		t.Errorf("release-aliases with an unlistable image: err = %v, want an error naming it", err)
	}
}

func TestRunReleaseAliasesValidatesTheImage(t *testing.T) {
	for _, image := range []string{
		"",
		"orglane",
		"ghcr.io/orglane/orglane:latest",
		"ghcr.io/Orglane/orglane",
		"ghcr.io/orglane/orglane@sha256:abc",
		"ghcr.io/orglane/",
	} {
		err := run(t.Context(), []string{"release-aliases", "-image", image, "-registry-url", "http://127.0.0.1:1"}, nil, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "-image") {
			t.Errorf("release-aliases -image %q: err = %v, want an -image error", image, err)
		}
	}
}

func TestSplitImageAcceptsARegistryPort(t *testing.T) {
	host, repo, err := splitImage("localhost:5000/orglane/orglane")
	if err != nil || host != "localhost:5000" || repo != "orglane/orglane" {
		t.Errorf("splitImage = %q, %q, %v", host, repo, err)
	}
}
