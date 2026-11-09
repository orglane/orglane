// Command ci contains the self-tested logic of the CI workflow's test tiers:
// which paths may skip the heavy tests, how a package's tests are sharded and
// whether the required `test` aggregate may report success.
//
// The workflow calls it with `go run ./tests/ci <command>`; the cases in
// main_test.go are the regression tests for that logic (run by
// `go test ./tests/ci/` and by the `lint` target).
//
// Usage:
//
//	go run ./tests/ci paths        # git diff of the change -> GITHUB_OUTPUT tier lines
//	go run ./tests/ci shard        # test names on stdin -> one shard's names on stdout
//	go run ./tests/ci aggregate    # needs JSON in NEEDS -> verdict on stdout, exit code
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "usage: ci paths|shard|aggregate\n")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "paths":
		err = runPaths(os.Args[2:], os.Stdout)
	case "shard":
		err = runShard(os.Args[2:], os.Stdin, os.Stdout)
	case "aggregate":
		err = runAggregate(os.Getenv, os.Stdout)
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// skipTier reports whether a path may skip the heavy tiers. This is a
// conservative allowlist: only documentation, the website and the license
// texts leave the Docker e2e, Compose/installer and web e2e suites out.
// Everything else (code, tests, helpers, migrations, deploy files, images,
// workspace configuration) runs them, so a new dependency of an e2e test
// never silently stops running it: the failure mode is running too much.
func skipTier(p string) bool {
	return strings.HasPrefix(p, "docs/") ||
		strings.HasPrefix(p, "website/") ||
		strings.HasSuffix(p, ".md") ||
		p == "LICENSE"
}

// evaluatePaths returns whether the heavy tier and the web e2e tier must run
// for the changed paths. A pull request's full-ci label forces both. With no
// paths (an unexpected empty diff) both run.
func evaluatePaths(paths []string, fullCI bool) (heavy, web bool) {
	if fullCI || len(paths) == 0 {
		return true, true
	}
	for _, p := range paths {
		if !skipTier(p) {
			return true, true
		}
	}
	return false, false
}

// changedPaths lists every path a change touches. Rename detection is off on
// purpose: a rename is reported as the deletion of its old path and the
// addition of its new one, so moving code into a docs path (or the other way)
// cannot hide the old path from the tier selection. Deletions keep their path.
func changedPaths(dir, base, head string) ([]string, error) {
	cmd := exec.CommandContext(context.Background(), "git", "-c", "core.quotePath=false", "diff", "--no-renames", "--name-only", "-z", base, head) //nolint:gosec // fixed binary; revisions chosen by the workflow
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git diff %s %s: %w", base, head, err)
	}
	var paths []string
	for _, p := range strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths, nil
}

func runPaths(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("paths", flag.ContinueOnError)
	dir := fs.String("dir", ".", "repository directory")
	base := fs.String("base", "HEAD^1", "base revision")
	head := fs.String("head", "HEAD", "head revision")
	fullCI := fs.Bool("full-ci", false, "force every tier (the pull request full-ci label)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	paths, err := changedPaths(*dir, *base, *head)
	if err != nil {
		return err
	}
	heavy, web := evaluatePaths(paths, *fullCI)
	if _, err := fmt.Fprintf(stdout, "heavy=%t\nweb=%t\n", heavy, web); err != nil {
		return err
	}
	return nil
}

type shardOptions struct {
	shard  int
	shards int
	only   string
	skip   string
}

// selectShard filters names by the optional only/skip regular expressions
// (the same semantics as go test -run/-skip for the names), sorts them and
// returns every shards-th one. It is an error when nothing is left: a shard
// that is expected to run tests must never pass with zero of them.
func selectShard(names []string, opt shardOptions) ([]string, error) {
	if opt.shards < 1 || opt.shard < 1 || opt.shard > opt.shards {
		return nil, fmt.Errorf("shard %d is not in 1..%d", opt.shard, opt.shards)
	}
	var only, skip *regexp.Regexp
	var err error
	if opt.only != "" {
		if only, err = regexp.Compile(opt.only); err != nil {
			return nil, fmt.Errorf("only: %w", err)
		}
	}
	if opt.skip != "" {
		if skip, err = regexp.Compile(opt.skip); err != nil {
			return nil, fmt.Errorf("skip: %w", err)
		}
	}
	var filtered []string
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if only != nil && !only.MatchString(name) {
			continue
		}
		if skip != nil && skip.MatchString(name) {
			continue
		}
		filtered = append(filtered, name)
	}
	if len(filtered) == 0 {
		return nil, errors.New("no tests match the only/skip filters")
	}
	slices.Sort(filtered)
	var selected []string
	for i, name := range filtered {
		if i%opt.shards == opt.shard-1 {
			selected = append(selected, name)
		}
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("shard %d of %d selected no tests", opt.shard, opt.shards)
	}
	return selected, nil
}

func runShard(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("shard", flag.ContinueOnError)
	shard := fs.Int("shard", 0, "1-based shard number")
	shards := fs.Int("shards", 0, "number of shards")
	only := fs.String("only", "", "regular expression of names to consider (go test -run)")
	skip := fs.String("skip", "", "regular expression of names to exclude (go test -skip)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	names, err := readLines(stdin)
	if err != nil {
		return err
	}
	selected, err := selectShard(names, shardOptions{shard: *shard, shards: *shards, only: *only, skip: *skip})
	if err != nil {
		return err
	}
	for _, name := range selected {
		if _, err := fmt.Fprintln(stdout, name); err != nil {
			return err
		}
	}
	return nil
}

// aggregateJobs are the jobs the `test` check waits for. Everything except
// the explicitly gated heavy tiers must succeed whenever the aggregate runs;
// a heavy job may be skipped only when the changes job decided not to run its
// tier. Any failure or cancellation is an error.
// resultCancelled is the needs result GitHub reports for a job stopped by a
// cancellation (the value itself is spelled with two l's).
const resultCancelled = "cancelled" //nolint:misspell // GitHub's result value

var aggregateJobs = []struct {
	name string
	tier string
}{
	{"changes", "fast"},
	{"unit", "fast"},
	{"integration", "fast"},
	{"apps", "fast"},
	{"agent", "fast"},
	{"apps-e2e", "heavy"},
	{"agent-e2e", "heavy"},
	{"compose", "heavy"},
	{"web-e2e", "web"},
	{"website", "fast"},
}

// evaluateAggregate checks the needs results. heavy and web come from the
// changes job's outputs, so a skip is accepted only for a tier that job
// explicitly unselected.
func evaluateAggregate(needsJSON string, heavy, web bool) error {
	var needs map[string]struct {
		Result string `json:"result"`
	}
	if err := json.Unmarshal([]byte(needsJSON), &needs); err != nil {
		return fmt.Errorf("decode needs: %w", err)
	}
	var problems []string
	for _, job := range aggregateJobs {
		entry, ok := needs[job.name]
		if !ok || entry.Result == "" {
			problems = append(problems, job.name+": missing from needs")
			continue
		}
		result := entry.Result
		want := "success"
		if job.tier == "heavy" && !heavy {
			want = "success or skipped (heavy tier not selected)"
		}
		if job.tier == "web" && !web {
			want = "success or skipped (web tier not selected)"
		}
		switch result {
		case "failure":
			problems = append(problems, fmt.Sprintf("%s: %s (want %s)", job.name, result, want))
		case resultCancelled: //nolint:misspell // GitHub's result value
			problems = append(problems, fmt.Sprintf("%s: %s (want %s)", job.name, result, want))
		case "skipped":
			if want == "success" {
				problems = append(problems, fmt.Sprintf("%s: skipped, but every job is required for this event", job.name))
			}
		case "success":
			// Always acceptable; a heavy job that ran although the tier was
			// not selected still exercised the code.
		default:
			problems = append(problems, fmt.Sprintf("%s: unexpected result %q", job.name, result))
		}
	}
	for _, job := range aggregateJobs {
		if entry, ok := needs[job.name]; ok {
			fmt.Printf("%s: %s\n", job.name, entry.Result)
		}
	}
	if len(problems) > 0 {
		return errors.New("required test jobs did not settle as expected:\n  " + strings.Join(problems, "\n  "))
	}
	return nil
}

func runAggregate(getenv func(string) string, stdout io.Writer) error {
	needs := getenv("NEEDS")
	if needs == "" {
		return errors.New("NEEDS is empty")
	}
	heavy, err := envBool(getenv, "HEAVY")
	if err != nil {
		return err
	}
	web, err := envBool(getenv, "WEB")
	if err != nil {
		return err
	}
	if err := evaluateAggregate(needs, heavy, web); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(stdout, "all required test jobs succeeded or were skipped by the tier selection"); err != nil {
		return err
	}
	return nil
}

func envBool(getenv func(string) string, name string) (bool, error) {
	switch getenv(name) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, fmt.Errorf("%s must be true or false, got %q", name, getenv(name))
	}
}

func readLines(r io.Reader) ([]string, error) {
	var lines []string
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return lines, nil
}
