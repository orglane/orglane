package onboarding_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/orglane/orglane/core/internal/onboarding"
)

func TestRegistryChecksCompletionAndAuthorization(t *testing.T) {
	id := uuid.New()
	v := onboarding.Viewer{PersonID: id}
	complete := false
	step := onboarding.CheckFunc(func(context.Context, onboarding.Viewer, uuid.UUID) (*onboarding.Result, error) {
		return &onboarding.Result{ID: "future:account", Title: "Link account", Required: true, Complete: complete, ActionURL: "/apps"}, nil
	})
	hidden := onboarding.CheckFunc(func(context.Context, onboarding.Viewer, uuid.UUID) (*onboarding.Result, error) { return nil, nil })
	registry := onboarding.Registry{step, hidden}
	result, err := registry.Progress(t.Context(), v, id)
	if err != nil || len(result) != 1 || result[0].Complete {
		t.Fatalf("initial: %+v %v", result, err)
	}
	complete = true
	result, err = registry.Progress(t.Context(), v, id)
	if err != nil || !result[0].Complete {
		t.Fatalf("stale completion: %+v %v", result, err)
	}
	if _, err = (onboarding.Registry{step, step}).Progress(t.Context(), v, id); err == nil {
		t.Fatal("duplicate IDs accepted")
	}
	for _, link := range []string{"", "//example.test", "https://example.test", "/\\example.test"} {
		bad := onboarding.CheckFunc(func(context.Context, onboarding.Viewer, uuid.UUID) (*onboarding.Result, error) {
			return &onboarding.Result{ID: "bad", Title: "Bad", Required: true, ActionURL: link}, nil
		})
		if _, err = (onboarding.Registry{bad}).Progress(t.Context(), v, id); err == nil {
			t.Errorf("unsafe or impossible action accepted: %q", link)
		}
	}
}
