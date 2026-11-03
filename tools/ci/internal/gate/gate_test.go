package gate

import (
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	head    = "4f2c9d1e8a7b6c5d4e3f2a1b0c9d8e7f6a5b4c3d"
	oldHead = "0b1c2d3e4f5a6b7c8d9e0f1a2b3c4d5e6f7a8b9c"
)

var (
	headAt   = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	after    = headAt.Add(time.Hour)
	later    = headAt.Add(2 * time.Hour)
	required = []string{"changes", "lint", "test", "build"}
	allGreen = []Check{{"changes", "success"}, {"lint", "success"}, {"test", "success"}, {"build", "success"}, {"external", "neutral"}}
)

// approve is a trusted approval of the given commit.
func approve(sha string, at time.Time) Comment {
	return review("Codex review: APPROVE\n\nReviewed commit: "+sha+"\nLooks good.", at)
}

func review(body string, at time.Time) Comment {
	return Comment{Author: "reviewer", Trusted: true, Body: body, CreatedAt: at}
}

// eligibleInput is a pull request that meets every requirement.
func eligibleInput() Input {
	return Input{
		HeadSHA:        head,
		ComputedTier:   "medium",
		Code:           true,
		Labels:         []string{"risk/medium", "area/ci"},
		Checks:         slices.Clone(allGreen),
		RequiredChecks: required,
		Comments:       []Comment{approve(head, after)},
	}
}

func TestEvaluate(t *testing.T) {
	tests := []struct {
		name   string
		modify func(*Input)
		want   Verdict
		detail string // substring of the report
	}{
		{name: "all requirements met", modify: func(*Input) {}, want: Eligible, detail: "APPROVE of 4f2c9d1e8a7b by reviewer"},
		{name: "low risk", modify: func(in *Input) { in.ComputedTier, in.Labels = "low", nil }, want: Eligible},
		{
			name:   "external fork",
			modify: func(in *Input) { in.FromFork = true },
			want:   NotEligible,
			detail: "external contribution",
		},
		{
			name:   "external fork with high risk is still not eligible",
			modify: func(in *Input) { in.FromFork, in.ComputedTier = true, "high" },
			want:   NotEligible,
		},
		{
			name:   "high risk",
			modify: func(in *Input) { in.ComputedTier = "high" },
			want:   MaintainerRequired,
			detail: "maintainer required",
		},
		{
			name:   "maintainer raised the tier with a label",
			modify: func(in *Input) { in.Labels = []string{"risk/high"} },
			want:   MaintainerRequired,
		},
		{
			name:   "a label cannot lower the computed tier",
			modify: func(in *Input) { in.ComputedTier, in.Labels = "high", []string{"risk/low"} },
			want:   MaintainerRequired,
		},
		{
			name:   "unknown tier",
			modify: func(in *Input) { in.ComputedTier, in.Labels = "", nil },
			want:   NotEligible,
			detail: "did not report a tier",
		},
		{
			name:   "invalid computed tier",
			modify: func(in *Input) { in.ComputedTier = "huge" },
			want:   NotEligible,
			detail: "unknown tier",
		},
		{
			name:   "failing check",
			modify: func(in *Input) { in.Checks = append(in.Checks, Check{"test", "failure"}) },
			want:   NotEligible,
			detail: "failing: test (failure)",
		},
		{
			name:   "timed out job",
			modify: func(in *Input) { in.Checks = []Check{{"lint", "timed_out"}} },
			want:   NotEligible,
			detail: "lint (timed_out)",
		},
		{
			name:   "required check not reported yet",
			modify: func(in *Input) { in.Checks = in.Checks[1:] },
			want:   NotEligible,
			detail: "changes (not reported yet)",
		},
		{
			name:   "required check skipped although code changed",
			modify: func(in *Input) { in.Checks[2] = Check{"test", "skipped"} },
			want:   NotEligible,
			detail: "test (skipped although code changed)",
		},
		{
			name: "required checks skipped for a docs-only change",
			modify: func(in *Input) {
				in.Code = false
				in.Checks = []Check{{"changes", "success"}, {"lint", "skipped"}, {"test", "skipped"}, {"build", "skipped"}}
			},
			want: Eligible,
		},
		{
			name:   "pending check",
			modify: func(in *Input) { in.Checks = append(in.Checks, Check{"integration", "in_progress"}) },
			want:   NotEligible,
			detail: "pending: integration",
		},
		{
			name:   "no review",
			modify: func(in *Input) { in.Comments = nil },
			want:   NotEligible,
			detail: "no \"Codex review:\" comment from a user with write access",
		},
		{
			name:   "changes requested",
			modify: func(in *Input) { in.Comments = []Comment{review("Codex review: CHANGES REQUESTED\n- fix x", after)} },
			want:   NotEligible,
			detail: `says "CHANGES REQUESTED"`,
		},
		{
			name: "latest review wins",
			modify: func(in *Input) {
				in.Comments = []Comment{review("Codex review: CHANGES REQUESTED", later), approve(head, after)}
			},
			want: NotEligible,
		},
		{
			name: "approval after earlier changes requested",
			modify: func(in *Input) {
				in.Comments = []Comment{review("Codex review: CHANGES REQUESTED", after), approve(head, later)}
			},
			want: Eligible,
		},
		{
			name:   "approval without a commit SHA",
			modify: func(in *Input) { in.Comments = []Comment{review("Codex review: APPROVE", later)} },
			want:   NotEligible,
			detail: "does not name the head commit 4f2c9d1e8a7b",
		},
		{
			// A new push (whatever its committer date) is not covered by an
			// approval of the previous head.
			name:   "approval of a previous head",
			modify: func(in *Input) { in.Comments = []Comment{approve(oldHead, later)} },
			want:   NotEligible,
			detail: "does not name the head commit",
		},
		{
			name:   "approval naming a short SHA",
			modify: func(in *Input) { in.Comments = []Comment{approve(head[:7], later)} },
			want:   Eligible,
		},
		{
			name:   "SHA on the verdict line",
			modify: func(in *Input) { in.Comments = []Comment{review("Codex review: APPROVE for "+head[:10], later)} },
			want:   Eligible,
		},
		{
			name:   "six hex digits are not enough",
			modify: func(in *Input) { in.Comments = []Comment{approve(head[:6], later)} },
			want:   NotEligible,
		},
		{
			name:   "upper-case SHA",
			modify: func(in *Input) { in.Comments = []Comment{approve(strings.ToUpper(head), later)} },
			want:   Eligible,
		},
		{
			name: "qualified approval does not count",
			modify: func(in *Input) {
				in.Comments = []Comment{review("Codex review: APPROVE with nits to fix first", after)}
			},
			want: NotEligible,
		},
		{
			name: "approval from an outside contributor is ignored",
			modify: func(in *Input) {
				in.Comments = []Comment{{Author: "stranger", Body: "Codex review: APPROVE " + head, CreatedAt: later}}
			},
			want:   NotEligible,
			detail: "ignored comments by stranger",
		},
		{
			name: "outside contributor cannot override a member's verdict",
			modify: func(in *Input) {
				in.Comments = append(in.Comments, Comment{Author: "stranger", Body: "Codex review: CHANGES REQUESTED", CreatedAt: later})
			},
			want: Eligible,
		},
		{
			name: "other comments are ignored",
			modify: func(in *Input) {
				in.Comments = append(in.Comments, review("Thanks! Codex review: APPROVE was quick.", later))
			},
			want: Eligible,
		},
		{
			name:   "unresolved threads",
			modify: func(in *Input) { in.UnresolvedThreads = 2 },
			want:   NotEligible,
			detail: "2 unresolved",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := eligibleInput()
			tt.modify(&in)
			got := Evaluate(in)
			md := got.Markdown()
			if got.Verdict != tt.want {
				t.Errorf("verdict = %q, want %q\n%s", got.Verdict, tt.want, md)
			}
			if tt.detail != "" && !strings.Contains(md, tt.detail) {
				t.Errorf("report does not mention %q:\n%s", tt.detail, md)
			}
			if !strings.Contains(got.Text(), string(got.Verdict)) {
				t.Errorf("text report does not include the verdict:\n%s", got.Text())
			}
		})
	}
}

func TestParseReview(t *testing.T) {
	tests := []struct {
		body, verdict string
		ok, approve   bool
	}{
		{"Codex review: APPROVE", "APPROVE", true, true},
		{"  Codex review: approved.\n\ndetails", "APPROVED.", true, true},
		{"Codex review: **APPROVE**", "APPROVE", true, true},
		{"Codex review: CHANGES REQUESTED", "CHANGES REQUESTED", true, false},
		{"Codex review: APPROVE?", "APPROVE?", true, false},
		{"Codex review: APPROVE for 4f2c9d1", "APPROVE FOR 4F2C9D1", true, true},
		{"Codex review: APPROVED (commit 4f2c9d1e8a7b)", "APPROVED (COMMIT 4F2C9D1E8A7B)", true, true},
		{"Codex review: APPROVE but fix later", "APPROVE BUT FIX LATER", true, false},
		{"Codex review: APPROVE 4f2c9", "APPROVE 4F2C9", true, false},
		{"codex review: APPROVE", "", false, false},
		{"> Codex review: APPROVE", "", false, false},
	}
	for _, tt := range tests {
		verdict, ok := ParseReview(tt.body)
		if verdict != tt.verdict || ok != tt.ok {
			t.Errorf("ParseReview(%q) = %q, %v; want %q, %v", tt.body, verdict, ok, tt.verdict, tt.ok)
		}
		if ok && approved(verdict) != tt.approve {
			t.Errorf("approved(%q) = %v, want %v", verdict, approved(verdict), tt.approve)
		}
	}
}
