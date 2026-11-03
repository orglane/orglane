package risk

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// Status is the kind of change git reports for a file.
type Status string

// Change statuses, as reported by `git diff --name-status`.
const (
	Added    Status = "added"
	Modified Status = "modified"
	Deleted  Status = "deleted"
	Renamed  Status = "renamed"
)

// Line is a single added or removed line of a diff.
type Line struct {
	Number int    // line number in the new (added) or old (removed) file
	Text   string // line content without the leading '+' or '-'
}

// FileChange describes one changed file between two revisions.
type FileChange struct {
	Status  Status
	Path    string // path after the change (the old path for deletions)
	OldPath string // path before the change; differs from Path only for renames
	Added   []Line
	Removed []Line
	// OldContent and NewContent are the full file before and after the
	// change, loaded for test code so the classifier can tell executable
	// code from comments and literals. nil when unknown.
	OldContent, NewContent []byte
}

// GitDiff returns the changes between two revisions of the repository in dir.
// Renames are detected, so moving a file is not reported as delete + add.
func GitDiff(ctx context.Context, dir, base, head string) ([]FileChange, error) {
	names, err := git(ctx, dir, "diff", "--no-ext-diff", "--no-textconv", "-M", "--name-status", "-z", base, head)
	if err != nil {
		return nil, err
	}
	patch, err := git(ctx, dir, "diff", "--no-ext-diff", "--no-textconv", "--no-color", "-M", "-U0", base, head)
	if err != nil {
		return nil, err
	}
	changes, err := ParseNameStatus(names)
	if err != nil {
		return nil, err
	}
	lines, err := ParsePatch(patch)
	if err != nil {
		return nil, err
	}
	for i := range changes {
		c := &changes[i]
		if l, ok := lines[c.Path]; ok {
			c.Added = l.Added
			c.Removed = l.Removed
		}
		// Whole files, not just changed lines: commenting assertions out in
		// place only adds "/*" and "*/" lines.
		if c.Status != Deleted && isTestCode(c.Path) {
			if c.NewContent, err = git(ctx, dir, "show", head+":"+c.Path); err != nil {
				return nil, err
			}
		}
		if c.Status != Added && isTestCode(c.OldPath) {
			if c.OldContent, err = git(ctx, dir, "show", base+":"+c.OldPath); err != nil {
				return nil, err
			}
		}
	}
	return changes, nil
}

// FetchPull fetches a pull request's head and its base branch from remote
// into the repository in dir, without checking anything out: the pull
// request's code is only read as data. It returns the fetched head commit and
// the merge base, so GitDiff(mergeBase, head) is what the pull request
// changes.
func FetchPull(ctx context.Context, dir, remote string, number int, baseRef string) (head, mergeBase string, err error) {
	headRef := fmt.Sprintf("refs/orglane/pull/%d/head", number)
	baseLocal := fmt.Sprintf("refs/orglane/pull/%d/base", number)
	if _, err := git(ctx, dir, "fetch", "--quiet", "--no-tags", "--no-recurse-submodules", remote,
		fmt.Sprintf("+refs/pull/%d/head:%s", number, headRef),
		fmt.Sprintf("+refs/heads/%s:%s", baseRef, baseLocal)); err != nil {
		return "", "", err
	}
	out, err := git(ctx, dir, "rev-parse", "--verify", headRef+"^{commit}")
	if err != nil {
		return "", "", err
	}
	head = strings.TrimSpace(string(out))
	out, err = git(ctx, dir, "merge-base", baseLocal, headRef)
	if err != nil {
		return "", "", err
	}
	return head, strings.TrimSpace(string(out)), nil
}

func git(ctx context.Context, dir string, args ...string) ([]byte, error) {
	args = append([]string{"-c", "core.quotePath=false"}, args...)
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // fixed binary; arguments are revisions chosen by the caller
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// ParseNameStatus parses the output of `git diff --name-status -z`.
func ParseNameStatus(out []byte) ([]FileChange, error) {
	fields := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	if len(fields) == 1 && fields[0] == "" {
		return nil, nil
	}
	var changes []FileChange
	for i := 0; i < len(fields); {
		code := fields[i]
		if code == "" {
			return nil, fmt.Errorf("name-status: empty status at field %d", i)
		}
		switch code[0] {
		case 'R', 'C':
			if i+2 >= len(fields) {
				return nil, fmt.Errorf("name-status: truncated %s entry", code)
			}
			oldPath, newPath := fields[i+1], fields[i+2]
			if code[0] == 'R' {
				changes = append(changes, FileChange{Status: Renamed, Path: newPath, OldPath: oldPath})
			} else {
				// A copy leaves the source untouched: treat it as a new file.
				changes = append(changes, FileChange{Status: Added, Path: newPath, OldPath: newPath})
			}
			i += 3
		default:
			if i+1 >= len(fields) {
				return nil, fmt.Errorf("name-status: truncated %s entry", code)
			}
			path := fields[i+1]
			status := Modified
			switch code[0] {
			case 'A':
				status = Added
			case 'D':
				status = Deleted
			}
			changes = append(changes, FileChange{Status: status, Path: path, OldPath: path})
			i += 2
		}
	}
	return changes, nil
}

// PatchLines holds the added and removed lines of one file.
type PatchLines struct {
	Added   []Line
	Removed []Line
}

// ParsePatch parses a unified diff (as produced by `git diff -U0`) and returns
// the added and removed lines keyed by the file path after the change (the
// old path for deleted files).
func ParsePatch(patch []byte) (map[string]PatchLines, error) {
	result := map[string]PatchLines{}
	var (
		oldPath          string
		current          string
		inHunk           bool
		oldLine, newLine int
	)
	scanner := bufio.NewScanner(bytes.NewReader(patch))
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "diff --git "):
			oldPath, current, inHunk = "", "", false
		case !inHunk && strings.HasPrefix(line, "--- "):
			p, err := patchPath(strings.TrimPrefix(line, "--- "), "a/")
			if err != nil {
				return nil, err
			}
			oldPath = p
		case !inHunk && strings.HasPrefix(line, "+++ "):
			p, err := patchPath(strings.TrimPrefix(line, "+++ "), "b/")
			if err != nil {
				return nil, err
			}
			current = p
			if current == "" {
				current = oldPath
			}
		case strings.HasPrefix(line, "@@ "):
			o, n, err := parseHunkHeader(line)
			if err != nil {
				return nil, err
			}
			oldLine, newLine, inHunk = o, n, true
		case inHunk && strings.HasPrefix(line, "+"):
			pl := result[current]
			pl.Added = append(pl.Added, Line{Number: newLine, Text: line[1:]})
			result[current] = pl
			newLine++
		case inHunk && strings.HasPrefix(line, "-"):
			pl := result[current]
			pl.Removed = append(pl.Removed, Line{Number: oldLine, Text: line[1:]})
			result[current] = pl
			oldLine++
		case inHunk && strings.HasPrefix(line, " "):
			// Context line (only present when the diff has context).
			oldLine++
			newLine++
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading patch: %w", err)
	}
	return result, nil
}

// patchPath extracts the path from a `---`/`+++` header. It returns "" for
// /dev/null.
func patchPath(s, prefix string) (string, error) {
	// Git appends a tab after paths that contain spaces.
	s = strings.TrimSuffix(s, "\t")
	if s == "/dev/null" {
		return "", nil
	}
	if strings.HasPrefix(s, `"`) {
		unquoted, err := strconv.Unquote(s)
		if err != nil {
			return "", fmt.Errorf("unquoting path %s: %w", s, err)
		}
		s = unquoted
	}
	return strings.TrimPrefix(s, prefix), nil
}

// parseHunkHeader returns the start lines of a hunk header such as
// "@@ -12,3 +14,0 @@ func f()".
func parseHunkHeader(line string) (oldStart, newStart int, err error) {
	parts := strings.Fields(line)
	if len(parts) < 3 || !strings.HasPrefix(parts[1], "-") || !strings.HasPrefix(parts[2], "+") {
		return 0, 0, fmt.Errorf("malformed hunk header %q", line)
	}
	if oldStart, err = hunkStart(parts[1][1:]); err != nil {
		return 0, 0, fmt.Errorf("hunk header %q: %w", line, err)
	}
	if newStart, err = hunkStart(parts[2][1:]); err != nil {
		return 0, 0, fmt.Errorf("hunk header %q: %w", line, err)
	}
	return oldStart, newStart, nil
}

func hunkStart(r string) (int, error) {
	start, count, hasCount := strings.Cut(r, ",")
	n, err := strconv.Atoi(start)
	if err != nil {
		return 0, err
	}
	// With -U0, a hunk that only adds (or only removes) lines reports the
	// line *before* the change for the empty side, e.g. "-12,0".
	if hasCount && count == "0" {
		n++
	}
	return n, nil
}
