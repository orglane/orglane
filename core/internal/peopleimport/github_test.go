package peopleimport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestGitHubSourceBounds(t *testing.T) {
	for _, size := range []int{1, 1001, 2000, 2001} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.Header.Get("Authorization") != "Bearer transient-secret" {
					t.Error("missing credential")
				}
				switch {
				case r.URL.Path == "/orgs/acme":
					_, _ = fmt.Fprint(w, `{"id":42}`)
				case strings.HasSuffix(r.URL.Path, "/members"):
					page, _ := strconv.Atoi(r.URL.Query().Get("page"))
					members := []githubUser{}
					for id := (page-1)*100 + 1; id <= size && id <= page*100; id++ {
						members = append(members, githubUser{ID: int64(id), Login: fmt.Sprintf("u%d", id)})
					}
					// A malicious pagination URL must never be used.
					w.Header().Set("Link", `<http://invalid.example/steal>; rel="next"`)
					_ = json.NewEncoder(w).Encode(members)
				case strings.HasPrefix(r.URL.Path, "/user/"):
					id, _ := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/user/"), 10, 64)
					_ = json.NewEncoder(w).Encode(githubUser{ID: id, Login: fmt.Sprintf("u%d", id), Name: "Example", Avatar: "https://avatars.githubusercontent.com/u/1"})
				default:
					t.Error("unexpected path")
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			source := &githubSource{ctx: context.Background(), organization: "acme", token: "transient-secret", origin: server.URL, client: server.Client()}
			data, err := source.Read(nil)
			if size > GitHubMaxRows {
				if !errors.Is(err, ErrGitHub) || len(data.Rows) != 0 {
					t.Fatal("overflow must fail atomically")
				}
				return
			}
			if err != nil || len(data.Rows) != size {
				t.Fatalf("size=%d error=%v", len(data.Rows), err)
			}
			if data.Rows[0].Cells[2] != "" {
				t.Fatal("absent email invented")
			}
			if source.Key() != "github:42" || data.Rows[0].GitHub.UserID != 1 {
				t.Fatal("stable identity missing")
			}
			b, _ := json.Marshal(data)
			if strings.Contains(string(b), "transient-secret") {
				t.Fatal("credential in dataset")
			}
			if requests > githubMaxRequests {
				t.Fatal("request budget exceeded")
			}
		})
	}
}
func TestGitHubFailureIsAtomicAndFixed(t *testing.T) {
	for _, status := range []int{301, 302, 401, 403, 429, 500} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			count := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				count++
				w.Header().Set("Location", "http://invalid.example/steal")
				w.Header().Set("Retry-After", "999999")
				w.WriteHeader(status)
				_, _ = fmt.Fprint(w, "transient-secret")
			}))
			defer server.Close()
			client := server.Client()
			client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
			source := &githubSource{ctx: t.Context(), organization: "acme", token: "transient-secret", origin: server.URL, client: client}
			data, err := source.Read(nil)
			if !errors.Is(err, ErrGitHub) || len(data.Rows) != 0 || count != 1 {
				t.Fatalf("expected single atomic fixed failure, got %v requests=%d", err, count)
			}
		})
	}
}

func TestGitHubIncompleteOrRepeatedSourceFails(t *testing.T) {
	for _, scenario := range []string{"profile_failure", "repeat", "rate_exhausted", "canceled", "oversized", "credential_echo"} {
		t.Run(scenario, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path == "/orgs/acme" {
					if scenario == "rate_exhausted" {
						w.Header().Set("X-RateLimit-Remaining", "0")
					}
					if scenario == "oversized" {
						_, _ = fmt.Fprint(w, strings.Repeat("x", githubMaxResponse+1))
						return
					}
					_, _ = fmt.Fprint(w, `{"id":42}`)
					return
				}
				if strings.HasSuffix(r.URL.Path, "/members") {
					if scenario == "repeat" {
						_, _ = fmt.Fprint(w, `[{"id":1},{"id":1}]`)
					} else {
						_, _ = fmt.Fprint(w, `[{"id":1},{"id":2}]`)
					}
					return
				}
				if r.URL.Path == "/user/1" {
					if scenario == "credential_echo" {
						_, _ = fmt.Fprint(w, `{"id":1,"login":"first","name":"test-credential"}`)
						return
					}
					_, _ = fmt.Fprint(w, `{"id":1,"login":"first","name":"First Person"}`)
					return
				}
				w.WriteHeader(500)
				_, _ = fmt.Fprint(w, "secret-response-body")
			}))
			defer server.Close()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if scenario == "canceled" {
				cancel()
			}
			// #nosec G101 -- synthetic credential used only by the local fixture.
			source := &githubSource{ctx: ctx, token: "test-credential", organization: "acme", origin: server.URL, client: server.Client()}
			data, err := source.Read(nil)
			if !errors.Is(err, ErrGitHub) || len(data.Rows) != 0 {
				t.Fatalf("partial data survived: rows=%d err=%v", len(data.Rows), err)
			}
			if scenario == "rate_exhausted" && calls != 1 {
				t.Fatal("continued after exhausted limit")
			}
			if scenario == "canceled" && calls != 0 {
				t.Fatal("request after cancellation")
			}
		})
	}
}

func TestGitHubMemberLoginValidation(t *testing.T) {
	for _, login := range []string{"member_shortcode", "a_b", strings.Repeat("a", 39), "bad/login", "bad?login", "", strings.Repeat("a", 40)} {
		t.Run(login, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/orgs/acme":
					_, _ = fmt.Fprint(w, `{"id":42}`)
				case "/orgs/acme/members":
					if r.URL.Query().Get("page") == "1" {
						_, _ = fmt.Fprint(w, `[{"id":1}]`)
					} else {
						_, _ = fmt.Fprint(w, `[]`)
					}
				case "/user/1":
					_ = json.NewEncoder(w).Encode(githubUser{ID: 1, Login: login, Name: "Example"})
				default:
					t.Error("unexpected URL")
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			source := &githubSource{ctx: t.Context(), organization: "acme", origin: server.URL, client: server.Client()}
			data, err := source.Read(nil)
			valid := login == "member_shortcode" || login == "a_b" || len(login) == 39
			if valid {
				if err != nil || len(data.Rows) != 1 {
					t.Fatalf("valid member rejected: %v", err)
				}
			} else if !errors.Is(err, ErrGitHub) || len(data.Rows) != 0 {
				t.Fatal("malformed member accepted")
			}
		})
	}
	if githubSlug.MatchString("org_shortcode") {
		t.Fatal("organization validation relaxed")
	}
}
