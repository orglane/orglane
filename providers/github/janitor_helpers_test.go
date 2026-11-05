package github

import (
	"encoding/json"
	"regexp"
	"strconv"
	"testing"
	"time"
)

// A resource is eligible only with exact run/attempt ownership and independently
// verified creation and run ages. Names or timestamps alone never prove ownership.
type janitorResource struct {
	Name, Description string
	CreatedAt         time.Time
}
type janitorOrigin struct {
	Repository, RunID, Attempt                  string
	WorkflowPath, HeadRepository, Event, Branch string
	StartedAt, UpdatedAt                        time.Time
	Completed                                   bool
}

// janitorAttempt is the real workflow-attempt response consumed by the live janitor.
type janitorAttempt struct {
	ID             int64     `json:"id"`
	Attempt        int64     `json:"run_attempt"`
	StartedAt      time.Time `json:"run_started_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	Status         string    `json:"status"`
	WorkflowPath   string    `json:"path"`
	Event          string    `json:"event"`
	Branch         string    `json:"head_branch"`
	HeadRepository struct {
		FullName string `json:"full_name"`
	} `json:"head_repository"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

func (run janitorAttempt) origin(expected janitorOrigin) (janitorOrigin, bool) {
	if strconv.FormatInt(run.ID, 10) != expected.RunID || strconv.FormatInt(run.Attempt, 10) != expected.Attempt || run.Repository.FullName != expected.Repository || run.StartedAt.IsZero() {
		return janitorOrigin{}, false
	}
	expected.WorkflowPath = run.WorkflowPath
	expected.Event = run.Event
	expected.Branch = run.Branch
	expected.HeadRepository = run.HeadRepository.FullName
	expected.StartedAt = run.StartedAt
	expected.UpdatedAt = run.UpdatedAt
	expected.Completed = run.Status == "completed"
	return expected, true
}

var janitorNamePattern = regexp.MustCompile(`^orglane-test-([1-9][0-9]{0,19})-([1-9][0-9]{0,9})-([a-z0-9][a-z0-9-]{0,40})$`)
var workflowRepositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9_.-]*/[A-Za-z0-9_-][A-Za-z0-9_.-]*$`)

func janitorOwned(resource janitorResource, origin janitorOrigin, now time.Time, currentRun string) bool {
	// Protected github-live currently permits MAIN only. A branch name or a
	// workflow_dispatch event alone cannot prove the separately required manual
	// maintainer authorization, so non-main manual runs are preserved.
	trustedWorkflow := origin.WorkflowPath == ".github/workflows/github-live.yml" || origin.WorkflowPath == ".github/workflows/github-live.yml@main"
	if currentRun == "" || origin.RunID == currentRun || origin.Repository != "orglane/orglane" || origin.HeadRepository != "orglane/orglane" || !trustedWorkflow || origin.Branch != "main" {
		return false
	}
	switch origin.Event {
	case "push", "schedule", "workflow_dispatch":
	default:
		return false
	}
	match := janitorNamePattern.FindStringSubmatch(resource.Name)
	if len(match) != 4 || match[1] != origin.RunID || match[2] != origin.Attempt || !workflowRepositoryPattern.MatchString(origin.Repository) {
		return false
	}
	if resource.Description != "orglane-test:"+origin.Repository+":"+origin.RunID+":"+origin.Attempt {
		return false
	}
	if origin.StartedAt.IsZero() || !origin.StartedAt.Before(now.Add(-24*time.Hour)) {
		return false
	}
	if !origin.Completed || origin.UpdatedAt.IsZero() || origin.UpdatedAt.Before(origin.StartedAt) || !origin.UpdatedAt.Before(now.Add(-24*time.Hour)) {
		return false
	}
	return resource.CreatedAt.IsZero() || resource.CreatedAt.Before(now.Add(-24*time.Hour))
}

func TestJanitorOwnershipAndAge(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	origin := janitorOrigin{Repository: "orglane/orglane", RunID: "123", Attempt: "2", StartedAt: now.Add(-26 * time.Hour), UpdatedAt: now.Add(-25 * time.Hour), Completed: true, WorkflowPath: ".github/workflows/github-live.yml", HeadRepository: "orglane/orglane", Event: "push", Branch: "main"}
	resource := janitorResource{Name: "orglane-test-123-2-mentions", Description: "orglane-test:orglane/orglane:123:2", CreatedAt: now.Add(-25 * time.Hour)}
	if !janitorOwned(resource, origin, now, "999") {
		t.Fatal("old resource with exact ownership and age proof should be eligible")
	}
	for _, tc := range []struct {
		name   string
		change func(*janitorResource, *janitorOrigin)
	}{
		{"foreign workflow", func(_ *janitorResource, o *janitorOrigin) { o.WorkflowPath = ".github/workflows/untrusted.yml" }},
		{"missing workflow", func(_ *janitorResource, o *janitorOrigin) { o.WorkflowPath = "" }},
		{"foreign head repo", func(_ *janitorResource, o *janitorOrigin) { o.HeadRepository = "fork/orglane" }},
		{"missing head repo", func(_ *janitorResource, o *janitorOrigin) { o.HeadRepository = "" }},
		{"pull request", func(_ *janitorResource, o *janitorOrigin) { o.Event = "pull_request" }},
		{"pull request target", func(_ *janitorResource, o *janitorOrigin) { o.Event = "pull_request_target" }},
		{"unknown event", func(_ *janitorResource, o *janitorOrigin) { o.Event = "" }},
		{"branch push", func(_ *janitorResource, o *janitorOrigin) { o.Branch = "feature" }},
		{"missing branch", func(_ *janitorResource, o *janitorOrigin) { o.Branch = "" }},
		{"unproven manual branch authorization", func(_ *janitorResource, o *janitorOrigin) { o.Event = "workflow_dispatch"; o.Branch = "feature" }},
		{"foreign matching marker repo", func(r *janitorResource, o *janitorOrigin) {
			o.Repository = "other/project"
			r.Description = "orglane-test:other/project:123:2"
		}},
		{"current run", func(r *janitorResource, o *janitorOrigin) {
			o.RunID = "999"
			r.Name = "orglane-test-999-2-mentions"
			r.Description = "orglane-test:orglane/orglane:999:2"
		}},

		{"unknown resource", func(r *janitorResource, _ *janitorOrigin) { r.Name = "real-team" }},
		{"prefix only", func(r *janitorResource, _ *janitorOrigin) { r.Description = "" }},
		{"another repo", func(r *janitorResource, _ *janitorOrigin) { r.Description = "orglane-test:another/repo:123:2" }},
		{"another run", func(_ *janitorResource, o *janitorOrigin) { o.RunID = "456" }},
		{"another attempt", func(_ *janitorResource, o *janitorOrigin) { o.Attempt = "3" }},
		{"still running old attempt", func(_ *janitorResource, o *janitorOrigin) { o.Completed = false }},
		{"recently completed old attempt", func(_ *janitorResource, o *janitorOrigin) { o.UpdatedAt = now.Add(-time.Hour) }},
		{"unknown completion inactivity", func(_ *janitorResource, o *janitorOrigin) { o.UpdatedAt = time.Time{} }},
		{"completion before start", func(_ *janitorResource, o *janitorOrigin) { o.UpdatedAt = o.StartedAt.Add(-time.Hour) }},
		{"completion inactivity boundary", func(_ *janitorResource, o *janitorOrigin) { o.UpdatedAt = now.Add(-24 * time.Hour) }},
		{"unknown run age", func(_ *janitorResource, o *janitorOrigin) { o.StartedAt = time.Time{} }},
		{"recent run", func(_ *janitorResource, o *janitorOrigin) { o.StartedAt = now.Add(-time.Hour) }},
		{"future run", func(_ *janitorResource, o *janitorOrigin) { o.StartedAt = now.Add(time.Hour) }},
		{"recent repo", func(r *janitorResource, _ *janitorOrigin) { r.CreatedAt = now.Add(-time.Hour) }},
		{"age boundary", func(r *janitorResource, _ *janitorOrigin) { r.CreatedAt = now.Add(-24 * time.Hour) }},
		{"path injection", func(r *janitorResource, _ *janitorOrigin) { r.Name = "orglane-test-123-2-../other" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, o := resource, origin
			tc.change(&r, &o)
			if janitorOwned(r, o, now, "999") {
				t.Fatal("unsafe resource was eligible")
			}
		})
	}
	for _, event := range []string{"schedule", "workflow_dispatch"} {
		o := origin
		o.Event = event
		if !janitorOwned(resource, o, now, "999") {
			t.Fatalf("trusted main event %s rejected", event)
		}
	}
	if janitorOwned(resource, origin, now, "") {
		t.Fatal("missing current run must fail closed")
	}
	// GitHub documents a ref-qualified path; live Actions metadata also returns
	// a bare path. Accept only those exact MAIN variants.
	qualified := origin
	qualified.WorkflowPath += "@main"
	if !janitorOwned(resource, qualified, now, "999") {
		t.Fatal("documented workflow-attempt @main path rejected")
	}
	for _, path := range []string{".github/workflows/github-live.yml@feature", ".github/workflows/github-live.yml@main@main", ".github/workflows/github-live.yml@main#other", ".github/workflows/other.yml@main"} {
		untrusted := origin
		untrusted.WorkflowPath = path
		if janitorOwned(resource, untrusted, now, "999") {
			t.Fatalf("untrusted workflow/ref eligible: %s", path)
		}
	}

	team := resource
	team.CreatedAt = time.Time{}
	if !janitorOwned(team, origin, now, "999") {
		t.Fatal("team without created_at must use a completed attempt with verified inactivity")
	}
}

func TestJanitorWorkflowAttemptProtocol(t *testing.T) {
	// API-shaped fixture, including the documented ref-qualified path. No provider
	// endpoint is substituted; only decoding and destructive-cleanup eligibility run.
	const payload = `{"id":123,"run_attempt":2,"path":".github/workflows/github-live.yml@main","event":"workflow_dispatch","head_branch":"main","status":"completed","run_started_at":"2026-11-03T10:00:00Z","updated_at":"2026-11-03T11:00:00Z","repository":{"full_name":"orglane/orglane"},"head_repository":{"full_name":"orglane/orglane"}}`
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	resource := janitorResource{Name: "orglane-test-123-2-api", Description: "orglane-test:orglane/orglane:123:2", CreatedAt: now.Add(-25 * time.Hour)}
	for _, tc := range []struct {
		name    string
		change  func(map[string]any)
		current string
		want    bool
	}{
		{"documented main ref", nil, "999", true},
		{"observed bare path", func(m map[string]any) { m["path"] = ".github/workflows/github-live.yml" }, "999", true},
		{"foreign ref", func(m map[string]any) { m["path"] = ".github/workflows/github-live.yml@feature" }, "999", false},
		{"foreign workflow", func(m map[string]any) { m["path"] = ".github/workflows/ci.yml@main" }, "999", false},
		{"missing path", func(m map[string]any) { delete(m, "path") }, "999", false},
		{"fork head", func(m map[string]any) { m["head_repository"] = map[string]any{"full_name": "fork/orglane"} }, "999", false},
		{"missing head", func(m map[string]any) { m["head_repository"] = nil }, "999", false},
		{"foreign repository", func(m map[string]any) { m["repository"] = map[string]any{"full_name": "other/repo"} }, "999", false},
		{"manual unproven branch", func(m map[string]any) { m["head_branch"] = "feature" }, "999", false},
		{"pull request", func(m map[string]any) { m["event"] = "pull_request" }, "999", false},
		{"wrong attempt", func(m map[string]any) { m["run_attempt"] = 3 }, "999", false},
		{"wrong run", func(m map[string]any) { m["id"] = 456 }, "999", false},
		{"current run", nil, "123", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body map[string]any
			if err := json.Unmarshal([]byte(payload), &body); err != nil {
				t.Fatal(err)
			}
			if tc.change != nil {
				tc.change(body)
			}
			raw, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			var run janitorAttempt
			if err = json.Unmarshal(raw, &run); err != nil {
				t.Fatal(err)
			}
			origin, ok := run.origin(janitorOrigin{Repository: "orglane/orglane", RunID: "123", Attempt: "2"})
			eligible := ok && janitorOwned(resource, origin, now, tc.current)
			if eligible != tc.want {
				t.Fatalf("API-shaped attempt eligibility=%v want %v: %s", eligible, tc.want, raw)
			}
		})
	}
}
