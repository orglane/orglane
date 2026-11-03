package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/orglane/orglane/tools/ci/internal/gate"
	"github.com/orglane/orglane/tools/ci/internal/github"
	"github.com/orglane/orglane/tools/ci/internal/risk"
)

// statusContext is the commit status the policy workflow sets on the head
// commit of each pull request.
const statusContext = "merge-gate"

// defaultRequired are the CI jobs every pull request must report.
const defaultRequired = "changes,lint,test,build,security"

// runPolicy is the trusted half of CI: it computes a pull request's risk tier
// and merge eligibility with the code of the checkout it runs from (the
// default branch in the Policy workflow, or the orchestrator's main
// checkout), reading the pull request's commits only as data.
func runPolicy(ctx context.Context, args []string, stdout *strings.Builder) error {
	fs := flag.NewFlagSet("policy", flag.ContinueOnError)
	pr := fs.Int("pr", 0, "pull request number")
	fromEvent := fs.Bool("from-event", false, "take the pull requests from the workflow_run event in GITHUB_EVENT_PATH")
	dir := fs.String("dir", ".", "trusted repository checkout to fetch the pull request into")
	remote := fs.String("remote", "origin", "git remote of the repository")
	required := fs.String("require", defaultRequired, "comma-separated check names that must be reported")
	apply := fs.Bool("apply", false, "set the risk/* label and the merge-gate commit status (Policy workflow)")
	summary := fs.String("summary", os.Getenv("GITHUB_STEP_SUMMARY"), "file to append the report to")
	if err := fs.Parse(args); err != nil {
		return err
	}

	gh, err := github.NewFromEnv()
	if err != nil {
		return err
	}
	var numbers []int
	switch {
	case *fromEvent:
		if numbers, err = pullsFromEvent(ctx, gh, os.Getenv("GITHUB_EVENT_PATH"), stdout); err != nil {
			return err
		}
	case *pr > 0:
		numbers = []int{*pr}
	default:
		return errors.New("-pr or -from-event is required")
	}

	eligible := true
	for _, n := range numbers {
		res, err := evaluatePull(ctx, gh, n, *dir, *remote, splitList(*required), *apply, *summary, stdout)
		if err != nil {
			return fmt.Errorf("pull request #%d: %w", n, err)
		}
		eligible = eligible && res.Verdict == gate.Eligible
	}
	// In the workflow the verdict is reported through the commit status; a
	// failing job would only mark the default branch red.
	if !*apply && !eligible {
		return errFailed
	}
	return nil
}

func evaluatePull(ctx context.Context, gh *github.Client, number int, dir, remote string, required []string,
	apply bool, summary string, stdout *strings.Builder,
) (gate.Result, error) {
	pr, err := gh.PullRequest(ctx, number)
	if err != nil {
		return gate.Result{}, err
	}
	if pr.State != "open" {
		fmt.Fprintf(stdout, "#%d is %s; nothing to do\n", number, pr.State)
		return gate.Result{Verdict: gate.NotEligible}, nil
	}

	head, base, err := risk.FetchPull(ctx, dir, remote, number, pr.Base.Ref)
	if err != nil {
		return gate.Result{}, err
	}
	if head != pr.Head.SHA {
		return gate.Result{}, fmt.Errorf("fetched head %s, but the pull request head is %s (pushed meanwhile?)", shortSHA(head), shortSHA(pr.Head.SHA))
	}
	changes, err := risk.GitDiff(ctx, dir, base, head)
	if err != nil {
		return gate.Result{}, err
	}
	classified := risk.Classify(changes)

	in, err := collectGateInput(ctx, gh, pr)
	if err != nil {
		return gate.Result{}, err
	}
	in.ComputedTier = classified.Tier.String()
	in.Code = classified.Code
	in.RequiredChecks = required
	res := gate.Evaluate(in)

	fmt.Fprintf(stdout, "#%d %s: risk %s (%d files, code changed: %v)\n", number, shortSHA(head), classified.Tier, len(changes), classified.Code)
	for _, r := range classified.Reasons {
		fmt.Fprintf(stdout, "  - %s\n", strings.ReplaceAll(r, "`", ""))
	}
	fmt.Fprint(stdout, res.Text())

	var md strings.Builder
	fmt.Fprintf(&md, "# #%d at `%s`\n\n## Risk tier: `%s`\n\n", number, shortSHA(head), classified.Tier.Label())
	for i, r := range classified.Reasons {
		if i == 50 {
			fmt.Fprintf(&md, "- … and %d more\n", len(classified.Reasons)-50)
			break
		}
		fmt.Fprintf(&md, "- %s\n", r)
	}
	md.WriteString("\n" + res.Markdown() + "\n")
	if err := appendFile(summary, md.String()); err != nil {
		return res, err
	}

	if apply {
		if err := setRiskLabel(ctx, gh, pr, classified.Tier, stdout); err != nil {
			return res, err
		}
		state := "failure"
		if res.Verdict == gate.Eligible {
			state = "success"
		}
		if err := gh.CreateStatus(ctx, head, state, statusContext, statusDescription(res), runURL()); err != nil {
			return res, err
		}
		fmt.Fprintf(stdout, "set status %s=%s on %s\n", statusContext, state, shortSHA(head))
	}
	return res, nil
}

func collectGateInput(ctx context.Context, gh *github.Client, pr *github.PullRequest) (gate.Input, error) {
	in := gate.Input{FromFork: pr.FromFork(), HeadSHA: pr.Head.SHA, Labels: pr.LabelNames()}

	runs, err := gh.CheckRuns(ctx, pr.Head.SHA)
	if err != nil {
		return in, err
	}
	for _, r := range runs {
		state := r.Conclusion
		if r.Status != "completed" {
			state = gate.Pending
		}
		in.Checks = append(in.Checks, gate.Check{Name: r.Name, State: state})
	}
	statuses, err := gh.Statuses(ctx, pr.Head.SHA)
	if err != nil {
		return in, err
	}
	for _, s := range statuses {
		if s.Context == statusContext {
			continue // the gate's own previous verdict
		}
		in.Checks = append(in.Checks, gate.Check{Name: s.Context, State: s.State})
	}

	comments, err := gh.Comments(ctx, pr.Number)
	if err != nil {
		return in, err
	}
	canWrite := map[string]bool{}
	for _, c := range comments {
		gc := gate.Comment{Author: c.User.Login, Body: c.Body, CreatedAt: c.CreatedAt}
		if _, isReview := gate.ParseReview(c.Body); isReview {
			// author_association is unreliable with GITHUB_TOKEN (private org
			// membership shows as CONTRIBUTOR), so ask for the permission.
			w, seen := canWrite[gc.Author]
			if !seen {
				if w, err = gh.CanWrite(ctx, gc.Author); err != nil {
					return in, err
				}
				canWrite[gc.Author] = w
			}
			gc.Trusted = w
		}
		in.Comments = append(in.Comments, gc)
	}
	if in.UnresolvedThreads, err = gh.UnresolvedThreads(ctx, pr.Number); err != nil {
		return in, err
	}
	return in, nil
}

func setRiskLabel(ctx context.Context, gh *github.Client, pr *github.PullRequest, tier risk.Tier, stdout *strings.Builder) error {
	current := pr.LabelNames()
	for _, l := range risk.Labels() {
		if l != tier.Label() && slices.Contains(current, l) {
			if err := gh.RemoveLabel(ctx, pr.Number, l); err != nil {
				return err
			}
			fmt.Fprintf(stdout, "removed %s\n", l)
		}
	}
	if slices.Contains(current, tier.Label()) {
		return nil
	}
	if err := gh.AddLabel(ctx, pr.Number, tier.Label()); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "added %s\n", tier.Label())
	return nil
}

// statusDescription summarizes a result in one line: the verdict and the
// first unmet requirement.
func statusDescription(res gate.Result) string {
	desc := fmt.Sprintf("%s (risk/%s)", res.Verdict, res.Tier)
	for _, r := range res.Requirements {
		if !r.OK {
			return desc + ": " + r.Detail
		}
	}
	return desc
}

func runURL() string {
	server, repo, id := os.Getenv("GITHUB_SERVER_URL"), os.Getenv("GITHUB_REPOSITORY"), os.Getenv("GITHUB_RUN_ID")
	if server == "" || repo == "" || id == "" {
		return ""
	}
	return server + "/" + repo + "/actions/runs/" + id
}

// workflowRunEvent is the part of a workflow_run event payload the policy
// needs.
type workflowRunEvent struct {
	WorkflowRun *struct {
		Event          string `json:"event"`
		HeadSHA        string `json:"head_sha"`
		HeadBranch     string `json:"head_branch"`
		HeadRepository struct {
			Owner struct {
				Login string `json:"login"`
			} `json:"owner"`
		} `json:"head_repository"`
		PullRequests []struct {
			Number int `json:"number"`
		} `json:"pull_requests"`
	} `json:"workflow_run"`
}

// pullsFromEvent returns the open pull requests a completed CI run belongs
// to.
func pullsFromEvent(ctx context.Context, gh *github.Client, path string, stdout *strings.Builder) ([]int, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path comes from the CI environment
	if err != nil {
		return nil, fmt.Errorf("reading the event: %w", err)
	}
	numbers, ownerBranch, err := parseWorkflowRunEvent(data)
	if err != nil {
		return nil, err
	}
	if len(numbers) == 0 && ownerBranch != "" {
		// GitHub omits pull_requests for runs from forks.
		if numbers, err = gh.OpenPullsForHead(ctx, ownerBranch); err != nil {
			return nil, err
		}
	}
	if len(numbers) == 0 {
		fmt.Fprintln(stdout, "the workflow run belongs to no open pull request")
	}
	return numbers, nil
}

func parseWorkflowRunEvent(data []byte) (numbers []int, ownerBranch string, err error) {
	var ev workflowRunEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		return nil, "", fmt.Errorf("parsing the event: %w", err)
	}
	run := ev.WorkflowRun
	if run == nil {
		return nil, "", errors.New("not a workflow_run event")
	}
	if run.Event != "pull_request" {
		return nil, "", nil
	}
	for _, p := range run.PullRequests {
		numbers = append(numbers, p.Number)
	}
	if run.HeadRepository.Owner.Login != "" && run.HeadBranch != "" {
		ownerBranch = run.HeadRepository.Owner.Login + ":" + run.HeadBranch
	}
	return numbers, ownerBranch, nil
}

func splitList(s string) []string {
	var out []string
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}
