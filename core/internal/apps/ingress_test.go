package apps

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/orglane/orglane/core/internal/ingress"
)

func TestServedRoutesRecord(t *testing.T) {
	git := ingress.AppRoute{Key: "git", Route: "web", Host: "git.acme.com", Upstream: "http://git-web:3000"}
	chat := ingress.AppRoute{Key: "chat", Route: "web", Host: "chat.acme.com", Upstream: "http://chat-web:8080"}
	r := newServedRoutes()
	t0 := time.Now()
	if since, _ := r.lookup(git); !since.IsZero() {
		t.Fatal("a route never served is published")
	}
	r.record([]ingress.AppRoute{git}, t0)
	if since, _ := r.lookup(git); !since.Equal(t0) {
		t.Fatalf("since = %v, want the first serve", since)
	}
	// Served again unchanged: it keeps the time of the first serve.
	r.record([]ingress.AppRoute{git, chat}, t0.Add(time.Second))
	if since, _ := r.lookup(git); !since.Equal(t0) {
		t.Fatalf("since after a second serve = %v, want %v", since, t0)
	}
	// A different upstream or a maintenance route is not the route asked
	// for, and a changed route starts again.
	other := git
	other.Upstream = "http://git-web:4000"
	if since, _ := r.lookup(other); !since.IsZero() {
		t.Fatal("a route with another upstream counts as served")
	}
	r.record([]ingress.AppRoute{other, chat}, t0.Add(2*time.Second))
	if since, _ := r.lookup(git); !since.IsZero() {
		t.Fatal("a replaced route is still published")
	}
	// A configuration without the route drops it; served again later, it
	// counts from then.
	r.record(nil, t0.Add(3*time.Second))
	if since, _ := r.lookup(chat); !since.IsZero() {
		t.Fatal("a route left out of the last configuration is published")
	}
	r.record([]ingress.AppRoute{chat}, t0.Add(4*time.Second))
	if since, _ := r.lookup(chat); !since.Equal(t0.Add(4 * time.Second)) {
		t.Fatalf("since after a gap = %v", since)
	}
}

func TestServedRoutesAwait(t *testing.T) {
	git := ingress.AppRoute{Key: "git", Route: "web", Host: "git.acme.com", Upstream: "http://git-web:3000"}
	const settle, wait = 50 * time.Millisecond, 2 * time.Second

	t.Run("never served", func(t *testing.T) {
		r := newServedRoutes()
		if err := r.await(t.Context(), git, settle, 100*time.Millisecond); !errors.Is(err, errRouteUnpublished) {
			t.Fatalf("await = %v, want errRouteUnpublished", err)
		}
	})
	t.Run("served long ago", func(t *testing.T) {
		r := newServedRoutes()
		r.record([]ingress.AppRoute{git}, time.Now().Add(-time.Minute))
		if err := r.await(t.Context(), git, settle, wait); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("served while waiting, then settles", func(t *testing.T) {
		r := newServedRoutes()
		done := make(chan error, 1)
		start := time.Now()
		go func() { done <- r.await(t.Context(), git, settle, wait) }()
		time.Sleep(20 * time.Millisecond)
		select {
		case err := <-done:
			t.Fatalf("published before it was served: %v", err)
		default:
		}
		served := time.Now()
		r.record([]ingress.AppRoute{git}, served)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if time.Since(served) < settle {
			t.Fatalf("published %v after the serve, before the settle time %v", time.Since(served), settle)
		}
		if time.Since(start) >= wait {
			t.Fatal("await ran into its timeout")
		}
	})
	t.Run("only other routes served", func(t *testing.T) {
		r := newServedRoutes()
		other := git
		other.Maintenance, other.Upstream = true, ""
		go func() {
			for range 5 {
				time.Sleep(10 * time.Millisecond)
				r.record([]ingress.AppRoute{other}, time.Now())
			}
		}()
		if err := r.await(t.Context(), git, settle, 200*time.Millisecond); !errors.Is(err, errRouteUnpublished) {
			t.Fatalf("await = %v, want errRouteUnpublished", err)
		}
	})
	t.Run("canceled", func(t *testing.T) {
		r := newServedRoutes()
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := r.await(ctx, git, settle, wait); !errors.Is(err, context.Canceled) {
			t.Fatalf("await = %v, want context.Canceled", err)
		}
	})
}
