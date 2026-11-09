// Package gate decides whether a pull request may be merged without a
// maintainer, following ADR 0017 as amended by ADR 0020:
//
//   - the pull request comes from a branch of this repository (external
//     contributions are never merged automatically);
//   - the required CI checks exist and, like every other check, are green;
//   - the risk tier is not high (high needs a maintainer);
//   - the latest "Codex review:" comment says APPROVE and names the head
//     commit SHA;
//   - no review thread is unresolved.
package gate

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/orglane/orglane/tools/ci/internal/risk"
)

// ReviewPrefix starts the AI reviewer's verdict comment, for example
// "Codex review: APPROVE" or "Codex review: CHANGES REQUESTED".
const ReviewPrefix = "Codex review:"

// Comment is a pull request conversation comment.
type Comment struct {
	Author string
	// Trusted is true when the author has write access to the repository.
	// Only trusted verdicts count: anyone else (including fork contributors)
	// could post a fake approval.
	Trusted   bool
	Body      string
	CreatedAt time.Time
}

// Check is the state of one check: a job of the current workflow run, a
// check run or a commit status.
type Check struct {
	Name string
	// State is one of the conclusions below or "pending".
	State string
}

// Check states.
const (
	Pending = "pending"
	Success = "success"
)

// Input is everything the gate looks at.
type Input struct {
	FromFork bool
	// HeadSHA is the pull request's head commit. An approval counts only if
	// it names this commit: commit dates are chosen by the author and cannot
	// bind a review to the code it saw.
	HeadSHA string
	// ComputedTier is the tier computed by trusted code; empty if unknown.
	ComputedTier string
	// Code is true when the change touches more than docs (trusted
	// computation). Required checks may only be skipped when it is false.
	Code bool
	// Labels are the pull request's labels; a risk/* label set by a
	// maintainer can raise, never lower, the computed tier.
	Labels []string
	Checks []Check
	// RequiredChecks must be present among Checks (CI jobs).
	RequiredChecks    []string
	Comments          []Comment
	UnresolvedThreads int
}

// Verdict is the outcome of the gate.
type Verdict string

// Verdicts.
const (
	Eligible           Verdict = "eligible"
	MaintainerRequired Verdict = "maintainer required"
	NotEligible        Verdict = "not eligible"
)

// Requirement is one row of the gate's report.
type Requirement struct {
	Name   string
	OK     bool
	Detail string
}

// Result is the gate's decision with the reasons for it.
type Result struct {
	Verdict      Verdict
	Tier         string
	Requirements []Requirement
}

// Evaluate applies the merge policy to in.
func Evaluate(in Input) Result {
	tier, tierDetail := effectiveTier(in.ComputedTier, in.Labels)
	reqs := []Requirement{
		{
			Name:   "Branch of this repository",
			OK:     !in.FromFork,
			Detail: ifElse(in.FromFork, "external contribution: never merged automatically", "same-repository branch"),
		},
		checksRequirement(in.Checks, in.RequiredChecks, in.Code),
		{
			Name:   "Risk tier below high",
			OK:     tier != "" && tier != risk.High.String(),
			Detail: tierDetail,
		},
		reviewRequirement(in.Comments, in.HeadSHA),
		{
			Name:   "No unresolved review threads",
			OK:     in.UnresolvedThreads == 0,
			Detail: fmt.Sprintf("%d unresolved", in.UnresolvedThreads),
		},
	}

	res := Result{Verdict: Eligible, Tier: tier, Requirements: reqs}
	switch {
	case in.FromFork:
		res.Verdict = NotEligible
	case tier == risk.High.String():
		res.Verdict = MaintainerRequired
	case slices.ContainsFunc(reqs, func(r Requirement) bool { return !r.OK }):
		res.Verdict = NotEligible
	}
	return res
}

func effectiveTier(computed string, labels []string) (tier, detail string) {
	best, known := risk.Low, false
	var parts []string
	if computed != "" {
		t, err := risk.ParseTier(computed)
		if err != nil {
			return "", fmt.Sprintf("risk job reported an unknown tier %q", computed)
		}
		best, known = t, true
		parts = append(parts, "computed "+t.String())
	}
	for _, l := range labels {
		if !strings.HasPrefix(l, "risk/") {
			continue
		}
		t, err := risk.ParseTier(l)
		if err != nil {
			continue
		}
		parts = append(parts, "label "+l)
		if !known || t > best {
			best, known = t, true
		}
	}
	if !known {
		return "", "unknown: the risk job did not report a tier"
	}
	detail = strings.Join(parts, ", ")
	if best == risk.High {
		detail = "high (" + detail + "): maintainer required"
	} else {
		detail = best.String() + " (" + detail + ")"
	}
	return best.String(), detail
}

func checksRequirement(checks []Check, required []string, code bool) Requirement {
	var failed, pending []string
	for _, name := range required {
		i := slices.IndexFunc(checks, func(c Check) bool { return c.Name == name })
		switch {
		case i < 0:
			pending = append(pending, name+" (not reported yet)")
		case checks[i].State == "skipped" && code:
			failed = append(failed, name+" (skipped although code changed)")
		}
	}
	for _, c := range checks {
		switch c.State {
		case Success, "skipped", "neutral":
		case Pending, "queued", "in_progress", "waiting", "requested", "":
			pending = append(pending, c.Name)
		default:
			failed = append(failed, fmt.Sprintf("%s (%s)", c.Name, c.State))
		}
	}
	slices.Sort(failed)
	slices.Sort(pending)
	r := Requirement{Name: "Other checks green", OK: len(failed) == 0 && len(pending) == 0}
	switch {
	case len(failed) > 0:
		r.Detail = "failing: " + strings.Join(failed, ", ")
		if len(pending) > 0 {
			r.Detail += "; pending: " + strings.Join(pending, ", ")
		}
	case len(pending) > 0:
		r.Detail = "pending: " + strings.Join(pending, ", ")
	case len(checks) == 0:
		r.Detail = "no other checks"
	default:
		r.Detail = fmt.Sprintf("%d checks passed or skipped", len(checks))
	}
	return r
}

// ParseReview returns the verdict of a review comment ("APPROVE",
// "CHANGES REQUESTED", ...) and whether the comment is a review comment.
func ParseReview(body string) (verdict string, ok bool) {
	body = strings.TrimSpace(body)
	if !strings.HasPrefix(body, ReviewPrefix) {
		return "", false
	}
	first, _, _ := strings.Cut(strings.TrimPrefix(body, ReviewPrefix), "\n")
	first = strings.ToUpper(strings.TrimSpace(first))
	first = strings.Trim(first, "*_`")
	return strings.TrimSpace(first), true
}

var shaToken = regexp.MustCompile(`(?i)\b[0-9a-f]{7,40}\b`)

// namesCommit reports whether body mentions sha (in full, or as a prefix of
// at least 7 hex digits).
func namesCommit(body, sha string) bool {
	if len(sha) < 7 {
		return false
	}
	for _, tok := range shaToken.FindAllString(body, -1) {
		if strings.HasPrefix(strings.ToLower(sha), strings.ToLower(tok)) {
			return true
		}
	}
	return false
}

// approved reports whether a verdict line approves the change. Only an
// unqualified APPROVE (or APPROVED) counts, optionally followed by the
// commit it approves ("APPROVE for 4f2c9d1", "APPROVED (4f2c9d1)").
func approved(verdict string) bool {
	fields := strings.Fields(verdict)
	if len(fields) == 0 {
		return false
	}
	if word := strings.TrimRight(fields[0], ".!:"); word != "APPROVE" && word != "APPROVED" {
		return false
	}
	for _, f := range fields[1:] {
		f = strings.Trim(f, "()[]`@:,.#")
		switch {
		case f == "", f == "FOR", f == "OF", f == "AT", f == "COMMIT", f == "HEAD", f == "SHA":
		case len(f) >= 7 && shaToken.MatchString(f) && shaToken.FindString(f) == f:
		default:
			return false
		}
	}
	return true
}

func reviewRequirement(comments []Comment, headSHA string) Requirement {
	r := Requirement{Name: "AI review approved the head commit"}
	var latest *Comment
	var ignored []string
	for i := range comments {
		c := &comments[i]
		if _, ok := ParseReview(c.Body); !ok {
			continue
		}
		if !c.Trusted {
			if !slices.Contains(ignored, c.Author) {
				ignored = append(ignored, c.Author)
			}
			continue
		}
		if latest == nil || c.CreatedAt.After(latest.CreatedAt) {
			latest = c
		}
	}
	if latest == nil {
		r.Detail = fmt.Sprintf("no %q comment from a user with write access yet", ReviewPrefix)
		if len(ignored) > 0 {
			r.Detail += fmt.Sprintf(" (ignored comments by %s)", strings.Join(ignored, ", "))
		}
		return r
	}
	verdict, _ := ParseReview(latest.Body)
	when := latest.CreatedAt.UTC().Format(time.RFC3339)
	switch {
	case !approved(verdict):
		r.Detail = fmt.Sprintf("latest review by %s at %s says %q", latest.Author, when, verdict)
	case !namesCommit(latest.Body, headSHA):
		r.Detail = fmt.Sprintf("APPROVE by %s at %s does not name the head commit %s", latest.Author, when, short(headSHA))
	default:
		r.OK = true
		r.Detail = fmt.Sprintf("APPROVE of %s by %s at %s", short(headSHA), latest.Author, when)
	}
	return r
}

// Markdown renders the result for the job summary.
func (r Result) Markdown() string {
	var b strings.Builder
	icon := map[Verdict]string{Eligible: "✅", MaintainerRequired: "👤", NotEligible: "❌"}[r.Verdict]
	fmt.Fprintf(&b, "## Merge checklist: %s %s\n\n", icon, r.Verdict)
	switch r.Verdict {
	case Eligible:
		b.WriteString("All ADR 0017 requirements are met: this pull request may be merged without a maintainer.\n\n")
	case MaintainerRequired:
		b.WriteString("This pull request is `risk/high` (ADR 0017): a maintainer must review and merge it.\n\n")
	case NotEligible:
		b.WriteString("This pull request may not be merged automatically yet.\n\n")
	}
	b.WriteString("| Requirement | | Detail |\n|---|---|---|\n")
	for _, req := range r.Requirements {
		fmt.Fprintf(&b, "| %s | %s | %s |\n", req.Name, ifElse(req.OK, "✅", "❌"), strings.ReplaceAll(req.Detail, "|", `\|`))
	}
	return b.String()
}

// Text renders the result as plain text.
func (r Result) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "merge checklist: %s\n", r.Verdict)
	for _, req := range r.Requirements {
		fmt.Fprintf(&b, "  [%s] %s: %s\n", ifElse(req.OK, "ok", "--"), req.Name, req.Detail)
	}
	return b.String()
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func ifElse(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}
