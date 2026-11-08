package github

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// maxMentionText bounds how much of a body is scanned for mentions.
// GitHub limits bodies and comments to 65,536 characters, which is at most
// 256 KiB of UTF-8; anything longer did not come from GitHub's own limits.
const maxMentionText = 256 << 10

// Mentions returns the logins mentioned as @login in Markdown text, in
// order, deduplicated (case-insensitively), at most 50.
//
// Text in code is skipped as GitHub renders it: fenced code blocks (``` or
// ~~~, at most three spaces indented, closed by a fence of the same
// character at least as long), indented code blocks (four spaces or a tab
// after a blank line) and code spans (a backtick run closed by the next run
// of the same length within the paragraph). A mention starts at "@" that
// follows neither a letter, digit or one of "-_./@`\" (so e-mail addresses
// and escaped "\@" are no mentions); the login is GitHub's login pattern
// (letters, digits and single inner hyphens, at most 39 characters) and is
// not followed by another letter, digit or "_". "@org/team" mentions a team,
// which has no target in M2, and is skipped.
//
// The work is linear in the length of text (at most maxMentionText bytes
// are read), so an adversarial body cannot occupy a worker.
func Mentions(text string) []string {
	if len(text) > maxMentionText {
		text = text[:maxMentionText]
	}
	m := mentionScanner{seen: map[string]bool{}}
	var para []string
	flush := func() {
		if len(para) > 0 {
			m.paragraph(strings.Join(para, "\n"))
			para = para[:0]
		}
	}
	var fence byte
	fenceLen := 0
	for len(text) > 0 && len(m.out) < maxTargets {
		line, rest, _ := strings.Cut(text, "\n")
		text = rest
		line = strings.TrimSuffix(line, "\r")
		trimmed := strings.TrimLeft(line, " ")
		indent := len(line) - len(trimmed)
		if fence != 0 {
			if indent <= 3 && fenceRun(trimmed, fence) >= fenceLen && strings.TrimSpace(strings.TrimLeft(trimmed, string(fence))) == "" {
				fence = 0
			}
			continue
		}
		if indent <= 3 && len(trimmed) > 0 && (trimmed[0] == '`' || trimmed[0] == '~') {
			if n := fenceRun(trimmed, trimmed[0]); n >= 3 && (trimmed[0] == '~' || !strings.Contains(trimmed[n:], "`")) {
				flush()
				fence, fenceLen = trimmed[0], n
				continue
			}
		}
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		if len(para) == 0 && (indent >= 4 || strings.HasPrefix(trimmed, "\t")) {
			continue // an indented code block
		}
		para = append(para, line)
	}
	flush()
	return m.out
}

// fenceRun is the length of the run of c at the start of s.
func fenceRun(s string, c byte) int {
	n := 0
	for n < len(s) && s[n] == c {
		n++
	}
	return n
}

type mentionScanner struct {
	out  []string
	seen map[string]bool
}

// paragraph scans a paragraph's text outside its code spans. Backtick runs
// are matched through an index of the runs of each length, whose cursors
// only move forward, so the work is linear.
func (m *mentionScanner) paragraph(p string) {
	type run struct{ start, n int }
	var runs []run
	byLen := map[int][]int{} // run length -> indexes into runs, ascending
	for i := 0; i < len(p); {
		if p[i] != '`' || (i > 0 && p[i-1] == '\\') {
			i++
			continue
		}
		n := fenceRun(p[i:], '`')
		byLen[n] = append(byLen[n], len(runs))
		runs = append(runs, run{i, n})
		i += n
	}
	cursor := map[int]int{}
	pos := 0 // start of the text not yet scanned
	for i := 0; i < len(runs); i++ {
		r := runs[i]
		idx := byLen[r.n]
		c := cursor[r.n]
		for c < len(idx) && idx[c] <= i {
			c++
		}
		cursor[r.n] = c
		if c == len(idx) {
			continue // no closing run: literal backticks
		}
		j := idx[c]
		m.text(p[pos:r.start])
		pos = runs[j].start + runs[j].n
		i = j
	}
	m.text(p[pos:])
}

// text scans plain text for mentions.
func (m *mentionScanner) text(s string) {
	for i := 0; i < len(s) && len(m.out) < maxTargets; i++ {
		if s[i] != '@' {
			continue
		}
		if i > 0 {
			prev, _ := utf8.DecodeLastRuneInString(s[:i])
			if isWordRune(prev) || strings.ContainsRune("-_./@`\\", prev) {
				continue
			}
		}
		j := i + 1
		for j < len(s) && j-i <= 40 && (isASCIIAlnum(s[j]) || s[j] == '-') {
			j++
		}
		login := strings.TrimRight(s[i+1:j], "-")
		next, _ := utf8.DecodeRuneInString(s[j:])
		switch {
		case !loginPattern.MatchString(login):
		case j < len(s) && (isWordRune(next) || next == '_'):
			// "@bob_x", "@bøb" or a token over 39 characters: no login.
		case len(login) == j-i-1 && next == '/':
			// "@org/team" mentions a team.
		default:
			if key := strings.ToLower(login); !m.seen[key] {
				m.seen[key] = true
				m.out = append(m.out, login)
			}
		}
		i = j - 1
	}
}

func isASCIIAlnum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }
