package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

type githubLiveStep struct {
	ID, Name, Uses, Run, Shell, If string
	Env                            map[string]string
	With                           map[string]string
	ContinueOnError                bool `yaml:"continue-on-error"`
}

type githubLiveJob struct {
	Needs       string
	Environment string
	Permissions map[string]string
	Env         map[string]string
	Steps       []githubLiveStep
}

func githubLiveWorkflow(t *testing.T) (map[string]githubLiveJob, map[string]yaml.Node) {
	t.Helper()
	data, err := os.ReadFile("../../.github/workflows/github-live.yml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		On          map[string]yaml.Node
		Permissions map[string]string
		Concurrency struct {
			Group            string
			CancelInProgress bool `yaml:"cancel-in-progress"`
		}
		Jobs map[string]githubLiveJob
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	if len(workflow.Permissions) != 0 || workflow.Concurrency.Group != "github-live" || workflow.Concurrency.CancelInProgress {
		t.Fatal("live runs must be serialized without cancellation and default permissions must be empty")
	}
	return workflow.Jobs, workflow.On
}

// Catch PR triggers, a bypassed trust barrier, or credentials escaping the
// environment-protected job. Shell behavior below also exercises the barrier.
func TestGitHubLiveWorkflowSecretBoundary(t *testing.T) {
	jobs, triggers := githubLiveWorkflow(t)
	if len(triggers) != 3 {
		t.Fatalf("unexpected workflow triggers: %v", triggers)
	}
	for _, name := range []string{"push", "schedule", "workflow_dispatch"} {
		if _, ok := triggers[name]; !ok {
			t.Fatalf("missing trigger %s", name)
		}
	}
	var push struct{ Branches []string }
	pushTrigger := triggers["push"]
	if err := pushTrigger.Decode(&push); err != nil || !reflect.DeepEqual(push.Branches, []string{"main"}) {
		t.Fatalf("push must select only main: %+v (%v)", push, err)
	}
	var schedule []struct{ Cron string }
	scheduleTrigger := triggers["schedule"]
	if err := scheduleTrigger.Decode(&schedule); err != nil || len(schedule) != 1 {
		t.Fatalf("missing nightly schedule: %+v (%v)", schedule, err)
	}
	fields := strings.Fields(schedule[0].Cron)
	if len(fields) != 5 || strings.Join(fields[2:], " ") != "* * *" {
		t.Fatalf("schedule must run every night: %q", schedule[0].Cron)
	}
	guard, live := jobs["trusted"], jobs["live"]
	if len(jobs) != 2 || guard.Environment != "" || len(guard.Permissions) != 0 || live.Needs != "trusted" || live.Environment != "github-live" {
		t.Fatal("only the job behind the unprivileged trust barrier may access github-live")
	}
	if !reflect.DeepEqual(live.Permissions, map[string]string{"contents": "read", "actions": "read"}) {
		t.Fatalf("excess live job permissions: %v", live.Permissions)
	}
	for name, job := range jobs {
		if len(job.Env) != 0 {
			t.Fatalf("%s exposes job-wide credentials", name)
		}
		for _, step := range job.Steps {
			if step.ContinueOnError {
				t.Fatalf("%s ignores errors", step.Name)
			}
			if step.Uses != "" && name == "trusted" {
				t.Fatal("trust barrier must not execute checked-out code or actions")
			}
			if strings.HasPrefix(step.Uses, "actions/checkout@") && (step.With["persist-credentials"] != "false" || step.With["ref"] != "${{ github.sha }}") {
				t.Fatal("checkout must use the event SHA without persisted credentials")
			}
			if step.ID == "tests" || step.ID == "janitor" {
				for _, variable := range githubLiveVariables {
					kind := "vars"
					if strings.HasSuffix(variable, "PRIVATE_KEY") || strings.HasSuffix(variable, "SECRET") || strings.HasSuffix(variable, "TOKEN") {
						kind = "secrets"
					}
					if step.Env[variable] != "${{ "+kind+"."+variable+" }}" {
						t.Errorf("%s missing environment mapping for %s", step.ID, variable)
					}
				}
				if step.Env["ORGLANE_REQUIRE_GITHUB_LIVE"] != "1" {
					t.Fatal("required runs must fail on missing configuration")
				}
			} else if len(step.Env) != 0 {
				t.Fatalf("credentials exposed to setup step %s", step.Name)
			}
		}
	}
	if len(live.Steps) != 4 || live.Steps[2].ID != "tests" || live.Steps[3].ID != "janitor" ||
		live.Steps[3].If != "${{ always() && steps.tests.outcome != 'skipped' }}" ||
		live.Steps[2].Run != "bash .github/scripts/github-live.sh tests" ||
		live.Steps[3].Run != "bash .github/scripts/github-live.sh janitor" ||
		live.Steps[3].Env["GITHUB_TOKEN"] != "${{ github.token }}" || live.Steps[2].Env["GITHUB_TOKEN"] != "" {
		t.Fatal("tests and unconditional cleanup must use the checked entrypoint; only cleanup gets the run lookup token")
	}
}

func TestGitHubLiveTrustBarrier(t *testing.T) {
	jobs, _ := githubLiveWorkflow(t)
	if len(jobs["trusted"].Steps) != 1 || jobs["trusted"].Steps[0].Shell != "bash" {
		t.Fatal("missing bash trust barrier")
	}
	for _, tt := range []struct {
		name, repo, event, refType, ref string
		allowed                         bool
	}{
		{"main push", "orglane/orglane", "push", "branch", "refs/heads/main", true},
		{"nightly", "orglane/orglane", "schedule", "branch", "refs/heads/main", true},
		{"manual main", "orglane/orglane", "workflow_dispatch", "branch", "refs/heads/main", true},
		{"manual review branch", "orglane/orglane", "workflow_dispatch", "branch", "refs/heads/review/71", true},
		{"fork copy", "someone/orglane", "workflow_dispatch", "branch", "refs/heads/main", false},
		{"fork pull request", "orglane/orglane", "pull_request", "branch", "refs/pull/71/merge", false},
		{"pull request target", "orglane/orglane", "pull_request_target", "branch", "refs/heads/main", false},
		{"unreviewed push", "orglane/orglane", "push", "branch", "refs/heads/topic", false},
		{"scheduled branch", "orglane/orglane", "schedule", "branch", "refs/heads/topic", false},
		{"manual tag", "orglane/orglane", "workflow_dispatch", "tag", "refs/tags/main", false},
		{"injected branch", "orglane/orglane", "push", "branch", "refs/heads/main; exit 0", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), "bash", "-e", "-c", jobs["trusted"].Steps[0].Run) //nolint:gosec // exercise the actual workflow barrier without credentials
			cmd.Env = append(os.Environ(), "GITHUB_REPOSITORY="+tt.repo, "GITHUB_EVENT_NAME="+tt.event, "GITHUB_REF_TYPE="+tt.refType, "GITHUB_REF="+tt.ref)
			if out, err := cmd.CombinedOutput(); (err == nil) != tt.allowed {
				t.Fatalf("allowed = %v, want %v: %s (%v)", err == nil, tt.allowed, out, err)
			}
		})
	}
}

var githubLiveVariables = []string{
	"ORGLANE_TEST_GITHUB_ORG", "ORGLANE_TEST_GITHUB_APP_ID", "ORGLANE_TEST_GITHUB_APP_CLIENT_ID",
	"ORGLANE_TEST_GITHUB_INSTALLATION_ID", "ORGLANE_TEST_GITHUB_USER_LOGIN", "ORGLANE_TEST_GITHUB_USER_ID",
	"ORGLANE_TEST_GITHUB_APP_PRIVATE_KEY", "ORGLANE_TEST_GITHUB_WEBHOOK_SECRET", "ORGLANE_TEST_GITHUB_USER_TOKEN",
}

const (
	githubLiveListJanitor    = "test -tags integration -list ^TestGitHubLiveJanitor$ ./providers/github\n"
	githubLiveListAcceptance = "test -tags integration -list ^TestM2AcceptanceGitHubLive$ ./core/internal/apps\n"
	githubLiveProviderTests  = "test -tags integration -count=1 -timeout 10m -v -skip ^TestGitHubLiveJanitor$ ./providers/github\n"
	githubLiveAcceptanceTest = "test -tags integration -count=1 -timeout 10m -v -run ^TestM2AcceptanceGitHubLive$ ./core/internal/apps\n"
	githubLiveJanitor        = "test -tags integration -count=1 -timeout 5m -v -run ^TestGitHubLiveJanitor$ ./providers/github\n"
)

// Substitute only the go process boundary, never a provider API. This catches
// silent no-test success, ignored failures, caching and accidental full-suite
// execution by the janitor without needing live credentials or Docker.
func TestGitHubLiveInvocation(t *testing.T) {
	for _, tt := range []struct {
		name                 string
		mode, missing        string
		listing, m2Listing   string
		testExit, m2TestExit string
		wantCalls            string
		wantExit             int
	}{
		{"tests", "tests", "", "TestGitHubLiveJanitor", "TestM2AcceptanceGitHubLive", "0", "0", githubLiveListJanitor + githubLiveListAcceptance + githubLiveProviderTests + githubLiveAcceptanceTest, 0},
		{"janitor", "janitor", "", "TestGitHubLiveJanitor", "TestM2AcceptanceGitHubLive", "0", "0", githubLiveListJanitor + githubLiveJanitor, 0},
		{"tests fail", "tests", "", "TestGitHubLiveJanitor", "TestM2AcceptanceGitHubLive", "17", "0", githubLiveListJanitor + githubLiveListAcceptance + githubLiveProviderTests, 17},
		{"acceptance fails", "tests", "", "TestGitHubLiveJanitor", "TestM2AcceptanceGitHubLive", "0", "18", githubLiveListJanitor + githubLiveListAcceptance + githubLiveProviderTests + githubLiveAcceptanceTest, 18},
		{"cleanup fails", "janitor", "", "TestGitHubLiveJanitor", "TestM2AcceptanceGitHubLive", "19", "0", githubLiveListJanitor + githubLiveJanitor, 19},
		{"listing fails", "tests", "", "__listing_failure__", "TestM2AcceptanceGitHubLive", "0", "0", githubLiveListJanitor, 23},
		{"acceptance listing fails", "tests", "", "TestGitHubLiveJanitor", "__listing_failure__", "0", "0", githubLiveListJanitor + githubLiveListAcceptance, 23},
		{"missing janitor", "tests", "", "TestOther", "TestM2AcceptanceGitHubLive", "0", "0", githubLiveListJanitor, 1},
		{"missing acceptance", "tests", "", "TestGitHubLiveJanitor", "TestOther", "0", "0", githubLiveListJanitor + githubLiveListAcceptance, 1},
		{"missing janitor cleanup", "janitor", "", "TestOther", "TestM2AcceptanceGitHubLive", "0", "0", githubLiveListJanitor, 1},
		{"required flag", "tests", "ORGLANE_REQUIRE_GITHUB_LIVE", "TestGitHubLiveJanitor", "TestM2AcceptanceGitHubLive", "0", "0", "", 1},
		{"invalid mode", "other", "", "TestGitHubLiveJanitor", "TestM2AcceptanceGitHubLive", "0", "0", "", 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			out, calls, code := runGitHubLiveScript(t, githubLiveRun{mode: tt.mode, missing: tt.missing, listing: tt.listing, m2Listing: tt.m2Listing, testExit: tt.testExit, m2TestExit: tt.m2TestExit})
			if code != tt.wantExit || calls != tt.wantCalls {
				t.Fatalf("exit = %d, calls = %q, want exit %d calls %q, output = %s", code, calls, tt.wantExit, tt.wantCalls, out)
			}
			if tt.missing != "" && calls != "" {
				t.Fatal("missing configuration executed Go")
			}
		})
	}
	for _, variable := range githubLiveVariables {
		t.Run("missing "+variable, func(t *testing.T) {
			out, calls, code := runGitHubLiveScript(t, githubLiveRun{mode: "tests", missing: variable, listing: "TestGitHubLiveJanitor", m2Listing: "TestM2AcceptanceGitHubLive", testExit: "0", m2TestExit: "0"})
			if code != 1 || calls != "" || !strings.Contains(out, variable) || strings.Contains(out, "sentinel") {
				t.Fatalf("missing config must fail before Go and print names only: exit %d, calls %q, output %s", code, calls, out)
			}
		})
	}
}

type githubLiveRun struct {
	mode, missing, listing, m2Listing, testExit, m2TestExit string
}

func runGitHubLiveScript(t *testing.T, run githubLiveRun) (string, string, int) {
	t.Helper()
	dir := t.TempDir()
	const goScript = `#!/bin/bash
printf '%s\n' "$*" >> "$CALLS"
if [[ " $* " == *" -list "* ]]; then
  case "$*" in
    *TestM2AcceptanceGitHubLive*)
      if [[ "$M2_LISTING" == __listing_failure__ ]]; then exit 23; fi
      printf '%s\n' "$M2_LISTING" ;;
    *)
      if [[ "$LISTING" == __listing_failure__ ]]; then exit 23; fi
      printf '%s\n' "$LISTING" ;;
  esac
  exit 0
fi
case "$*" in
  *TestM2AcceptanceGitHubLive*) exit "$M2_TEST_EXIT" ;;
  *) exit "$TEST_EXIT" ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "go"), []byte(goScript), 0o700); err != nil { //nolint:gosec // isolated executable process fixture
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "bash", "../../.github/scripts/github-live.sh", run.mode) //nolint:gosec // fixed repository script with table-driven test arguments
	cmd.Env = append(os.Environ(),
		"PATH="+dir+":"+os.Getenv("PATH"),
		"CALLS="+filepath.Join(dir, "calls"),
		"LISTING="+run.listing,
		"M2_LISTING="+run.m2Listing,
		"TEST_EXIT="+run.testExit,
		"M2_TEST_EXIT="+run.m2TestExit,
		"ORGLANE_REQUIRE_GITHUB_LIVE=1",
	)
	for _, variable := range githubLiveVariables {
		cmd.Env = append(cmd.Env, variable+"=sentinel-do-not-print")
	}
	if run.missing != "" {
		cmd.Env = append(cmd.Env, run.missing+"=")
	}
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatal(err)
		}
		code = exitErr.ExitCode()
	}
	calls, err := os.ReadFile(filepath.Join(dir, "calls")) //nolint:gosec // private process fixture output
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(out), string(calls), code
}
