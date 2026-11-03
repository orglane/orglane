// Command ci holds the repository's CI tooling: the ADR 0017 risk labeler,
// the merge gate, the dependency license check and the release image tags.
//
// It uses only the standard library. The policy command is the trusted part:
// the Policy workflow runs it from the default branch, and it reads pull
// requests only as data, so a pull request cannot change the rules that
// judge it. See README.md.
//
// Usage:
//
//	go run ./tools/ci risk [-base REV] [-head REV]
//	go run ./tools/ci policy -pr N [-apply]
//	go run ./tools/ci policy -from-event -apply
//	go run ./tools/ci licenses -policy server|apache -format pnpm|go-licenses < report
//	go run ./tools/ci release-version -tag v1.2.3
//	go run ./tools/ci release-aliases -image ghcr.io/orglane/orglane
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/orglane/orglane/tools/ci/internal/licenses"
	"github.com/orglane/orglane/tools/ci/internal/risk"
)

// errFailed signals a failed check whose report is already printed.
var errFailed = errors.New("check failed")

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	err := run(ctx, os.Args[1:], os.Stdin, os.Stdout)
	cancel()
	if err != nil {
		if !errors.Is(err, errFailed) {
			fmt.Fprintln(os.Stderr, "error:", err)
		}
		os.Exit(1)
	}
}

const usage = `usage: ci <command> [flags]

commands:
  risk             classify a change into an ADR 0017 risk tier (CI "changes" job)
  policy           compute a pull request's risk tier and merge eligibility (trusted)
  licenses         check dependency licenses against the ADR 0016 policy
  release-version  validate a release tag and print its image version (Images workflow)
  release-aliases  compute the latest and X.Y image aliases from the registry (Images workflow)`

// run executes a command. Its report is buffered and written to stdout even
// when the command fails.
func run(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error {
	var out strings.Builder
	err := dispatch(ctx, args, stdin, &out)
	if _, werr := io.WriteString(stdout, out.String()); werr != nil && err == nil {
		err = werr
	}
	return err
}

func dispatch(ctx context.Context, args []string, stdin io.Reader, stdout *strings.Builder) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	cmd, args := args[0], args[1:]
	switch cmd {
	case "risk":
		return runRisk(ctx, args, stdout)
	case "policy":
		return runPolicy(ctx, args, stdout)
	case "licenses":
		return runLicenses(args, stdin, stdout)
	case "release-version":
		return runReleaseVersion(args, stdout)
	case "release-aliases":
		return runReleaseAliases(ctx, args, stdout)
	}
	return fmt.Errorf("unknown command %q\n%s", cmd, usage)
}

func runRisk(ctx context.Context, args []string, stdout *strings.Builder) error {
	fs := flag.NewFlagSet("risk", flag.ContinueOnError)
	dir := fs.String("dir", ".", "repository directory")
	base := fs.String("base", "HEAD^1", "base revision")
	head := fs.String("head", "HEAD", "head revision")
	output := fs.String("github-output", os.Getenv("GITHUB_OUTPUT"), "file to append step outputs (tier, code) to")
	summary := fs.String("summary", os.Getenv("GITHUB_STEP_SUMMARY"), "file to append a Markdown summary to")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if err := risk.CheckReviewAssets(ctx, *dir, *head); err != nil {
		return err
	}

	changes, err := risk.GitDiff(ctx, *dir, *base, *head)
	if err != nil {
		return err
	}
	res := risk.Classify(changes)

	fmt.Fprintf(stdout, "risk tier: %s (%d files changed, code changed: %v)\n", res.Tier, len(changes), res.Code)
	for _, r := range res.Reasons {
		fmt.Fprintf(stdout, "  - %s\n", strings.ReplaceAll(r, "`", ""))
	}

	if err := appendFile(*output, fmt.Sprintf("tier=%s\ncode=%v\n", res.Tier, res.Code)); err != nil {
		return err
	}
	var md strings.Builder
	fmt.Fprintf(&md, "## Risk tier: `%s`\n\n", res.Tier.Label())
	if !res.Code {
		md.WriteString("Only documentation changed: lint, tests and builds are skipped.\n\n")
	}
	const maxReasons = 50
	for i, r := range res.Reasons {
		if i == maxReasons {
			fmt.Fprintf(&md, "- … and %d more\n", len(res.Reasons)-maxReasons)
			break
		}
		fmt.Fprintf(&md, "- %s\n", r)
	}
	return appendFile(*summary, md.String())
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func runLicenses(args []string, stdin io.Reader, stdout *strings.Builder) error {
	fs := flag.NewFlagSet("licenses", flag.ContinueOnError)
	policyName := fs.String("policy", "server", "license policy: server (AGPL-3.0 parts) or apache (Apache-2.0 packages)")
	format := fs.String("format", "", "input format: pnpm (pnpm licenses list --json) or go-licenses (go-licenses report)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	policy, err := licenses.PolicyByName(*policyName)
	if err != nil {
		return err
	}
	var deps []licenses.Dependency
	switch *format {
	case "pnpm":
		deps, err = licenses.ParsePnpm(stdin)
	case "go-licenses":
		deps, err = licenses.ParseGoLicenses(stdin)
	default:
		return fmt.Errorf("-format must be pnpm or go-licenses, got %q", *format)
	}
	if err != nil {
		return err
	}

	violations := licenses.Check(policy, deps)
	if len(violations) == 0 {
		fmt.Fprintf(stdout, "licenses (%s, %s policy): %d dependencies OK\n", *format, policy.Name, len(deps))
		return nil
	}
	fmt.Fprintf(stdout, "::error title=License check::%d %s dependencies have licenses the %s policy does not allow\n",
		len(violations), *format, policy.Name)
	fmt.Fprintf(stdout, "license check failed (%s, %s policy):\n", *format, policy.Name)
	for _, v := range violations {
		fmt.Fprintf(stdout, "  - %s\n", v)
	}
	fmt.Fprintf(stdout, "allowed: %s\n", strings.Join(policy.Allowed(), ", "))
	fmt.Fprintln(stdout, "Replace the dependency, or ask a maintainer to review the license (ADR 0016) and extend the policy in tools/ci/internal/licenses.")
	return errFailed
}

func appendFile(path, content string) error {
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // path comes from the CI environment
	if err != nil {
		return err
	}
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
