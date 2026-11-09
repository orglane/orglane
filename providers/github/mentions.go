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
// Text that GitHub does not render as Markdown is skipped: fenced code
// blocks (``` or ~~~, at most three spaces indented, in block quotes and
// list items too), indented code blocks (four spaces or a tab after a blank
// line), code spans (a backtick run closed by the next run of the same
// length within the paragraph), link text, destinations, titles and
// reference labels, raw HTML tags and comments, autolinks, and the content
// of pre/script/style/textarea blocks. Headings, list items and block quotes
// end a paragraph for code-span matching. This is a linear approximation of
// GitHub's renderer, not a complete CommonMark implementation.
//
// A mention starts at "@" that follows neither a letter, digit or one of
// "-_./@`\" (so e-mail addresses and escaped "\@" are no mentions); the
// login is GitHub's login pattern (letters, digits and single inner hyphens,
// at most 39 characters) and is not followed by another letter, digit or
// "_". "@org/team" mentions a team, which has no target in M2, and is
// skipped.
//
// The work is linear in the length of text (at most maxMentionText bytes
// are read), so an adversarial body cannot occupy a worker.
func Mentions(text string) []string {
	if len(text) > maxMentionText {
		text = text[:maxMentionText]
	}
	// GitHub treats CR, CRLF and LF as line endings.
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
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
	fenceQuoted := false
	rawHTML := false
	htmlComment := false
	quoted := false
	for len(text) > 0 && len(m.out) < maxTargets {
		line, rest, _ := strings.Cut(text, "\n")
		text = rest
		content, lineQuoted, listStart := blockContent(line, true, true)
		if rawHTML {
			if htmlRawEnd(content) {
				rawHTML = false
			}
			quoted = lineQuoted
			continue
		}
		if htmlComment {
			if strings.Contains(content, "-->") {
				htmlComment = false
			}
			quoted = lineQuoted
			continue
		}
		if htmlCommentStart(content) {
			flush()
			htmlComment = !strings.Contains(content, "-->")
			quoted = lineQuoted
			continue
		}
		if htmlRawStart(content) {
			flush()
			rawHTML = !htmlRawEnd(content)
			quoted = lineQuoted
			continue
		}
		if fence != 0 {
			if fenceQuoted && !lineQuoted {
				// A fenced block in a block quote ends with the quote: its
				// content lines carry the quote marker.
				fence = 0
				fenceQuoted = false
			} else {
				// Inside a fence only the container markers that led to it
				// are removed; a list marker is content there.
				closeContent, _, _ := blockContent(line, fenceQuoted, false)
				trimmed := strings.TrimLeft(closeContent, " ")
				indent := len(closeContent) - len(trimmed)
				if indent <= 3 && fenceRun(trimmed, fence) >= fenceLen && strings.TrimSpace(strings.TrimLeft(trimmed, string(fence))) == "" {
					fence = 0
				}
				quoted = lineQuoted
				continue
			}
		}
		trimmed := strings.TrimLeft(content, " ")
		indent := len(content) - len(trimmed)
		if indent <= 3 && len(trimmed) > 0 && (trimmed[0] == '`' || trimmed[0] == '~') {
			if n := fenceRun(trimmed, trimmed[0]); n >= 3 && (trimmed[0] == '~' || !strings.Contains(trimmed[n:], "`")) {
				flush()
				fence, fenceLen, fenceQuoted = trimmed[0], n, lineQuoted
				quoted = lineQuoted
				continue
			}
		}
		if strings.TrimSpace(content) == "" {
			flush()
			quoted = lineQuoted
			continue
		}
		if listStart {
			flush()
		}
		if len(para) == 0 && linkDefinition(content) {
			flush()
			quoted = lineQuoted
			continue
		}
		if atxHeading(content) {
			// A heading is its own block: a code span cannot continue into
			// the following paragraph.
			flush()
			para = append(para, content)
			flush()
			quoted = lineQuoted
			continue
		}
		if setextUnderline(content) || thematicBreak(content) {
			flush()
			quoted = lineQuoted
			continue
		}
		if lineQuoted != quoted {
			flush()
		}
		if len(para) == 0 && (indent >= 4 || strings.HasPrefix(trimmed, "\t")) {
			quoted = lineQuoted
			continue // an indented code block
		}
		para = append(para, content)
		quoted = lineQuoted
	}
	flush()
	return m.out
}

// blockContent removes the block container markers a line may start with:
// block quotes (any nesting) and, when stripList is true, one list marker.
// The returned string is the line's content for block recognition; list
// markers are content inside a fence and are not stripped there.
func blockContent(line string, stripQuote, stripList bool) (content string, quoted, list bool) {
	s := line
	if stripQuote {
		for {
			n := 0
			for n < len(s) && n < 3 && s[n] == ' ' {
				n++
			}
			if n >= len(s) || s[n] != '>' {
				break
			}
			s = s[n+1:]
			if len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
				s = s[1:]
			}
			quoted = true
		}
	}
	if !stripList {
		return s, quoted, false
	}
	n := 0
	for n < len(s) && n < 3 && s[n] == ' ' {
		n++
	}
	if k := listMarkerLen(s[n:]); k > 0 {
		return s[n+k:], quoted, true
	}
	return s, quoted, false
}

// linkDefinition reports whether s starts a link reference definition
// ("[label]: destination").
func linkDefinition(s string) bool {
	n := 0
	for n < len(s) && n < 3 && s[n] == ' ' {
		n++
	}
	if n >= len(s) || s[n] != '[' {
		return false
	}
	j := n + 1
	for j < len(s) && s[j] != ']' {
		if s[j] == '\\' {
			j++
		}
		j++
	}
	return j+1 < len(s) && s[j] == ']' && s[j+1] == ':'
}

// listMarkerLen is the length of a bullet or ordered list marker at the
// start of s, including its following space or tab; 0 without one.
func listMarkerLen(s string) int {
	if len(s) >= 2 && (s[0] == '-' || s[0] == '*' || s[0] == '+') && (s[1] == ' ' || s[1] == '\t') {
		return 2
	}
	n := 0
	for n < len(s) && n < 9 && s[n] >= '0' && s[n] <= '9' {
		n++
	}
	if n > 0 && n+1 < len(s) && (s[n] == '.' || s[n] == ')') && (s[n+1] == ' ' || s[n+1] == '\t') {
		return n + 2
	}
	return 0
}

// atxHeading reports whether s (after its container markers) is an ATX
// heading line.
func atxHeading(s string) bool {
	n := 0
	for n < len(s) && n < 3 && s[n] == ' ' {
		n++
	}
	rest := s[n:]
	h := 0
	for h < len(rest) && rest[h] == '#' {
		h++
	}
	return h >= 1 && h <= 6 && (h == len(rest) || rest[h] == ' ' || rest[h] == '\t')
}

// setextUnderline reports whether s is a setext heading underline.
func setextUnderline(s string) bool {
	n := 0
	for n < len(s) && n < 3 && s[n] == ' ' {
		n++
	}
	rest := strings.TrimRight(s[n:], " \t")
	if rest == "" || (rest[0] != '=' && rest[0] != '-') {
		return false
	}
	for i := range len(rest) {
		if rest[i] != rest[0] {
			return false
		}
	}
	return true
}

// htmlCommentStart reports whether s starts an HTML comment block.
func htmlCommentStart(s string) bool {
	n := 0
	for n < len(s) && n < 3 && s[n] == ' ' {
		n++
	}
	return strings.HasPrefix(s[n:], "<!--")
}

// htmlRawStart reports whether s starts a raw-text HTML block (pre,
// script, style or textarea), whose content is not Markdown.
func htmlRawStart(s string) bool {
	n := 0
	for n < len(s) && n < 3 && s[n] == ' ' {
		n++
	}
	s = strings.ToLower(s[n:])
	for _, tag := range []string{"<pre", "<script", "<style", "<textarea"} {
		if !strings.HasPrefix(s, tag) {
			continue
		}
		if len(s) == len(tag) || s[len(tag)] == '>' || s[len(tag)] == ' ' || s[len(tag)] == '\t' {
			return true
		}
	}
	return false
}

// htmlRawEnd reports whether s contains the end of a raw-text HTML block.
func htmlRawEnd(s string) bool {
	s = strings.ToLower(s)
	for _, tag := range []string{"</pre", "</script", "</style", "</textarea"} {
		for i := 0; ; {
			j := strings.Index(s[i:], tag)
			if j < 0 {
				break
			}
			k := i + j + len(tag)
			if k == len(s) || s[k] == '>' || s[k] == ' ' || s[k] == '\t' {
				return true
			}
			i = k
		}
	}
	return false
}

// thematicBreak reports whether s is a thematic break (---, ***, ___).
func thematicBreak(s string) bool {
	n := 0
	for n < len(s) && n < 3 && s[n] == ' ' {
		n++
	}
	rest := strings.TrimRight(s[n:], " \t")
	if len(rest) < 3 || (rest[0] != '-' && rest[0] != '*' && rest[0] != '_') {
		return false
	}
	count := 0
	for i := range len(rest) {
		switch rest[i] {
		case rest[0]:
			count++
		case ' ', '\t':
		default:
			return false
		}
	}
	return count >= 3
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

// paragraph scans a paragraph's text outside its code spans. Link
// destinations and titles and raw HTML tags are blanked first (they are not
// rendered), then backtick runs are matched through an index of the runs of
// each length, whose cursors only move forward, so the work is linear.
func (m *mentionScanner) paragraph(p string) {
	visible := []byte(p)
	escaped := escapeMask(visible)
	hideLinkDestinations(visible, escaped)
	hideHTMLTags(visible, escaped)
	type run struct{ start, n int }
	var runs []run
	byLen := map[int][]int{} // run length -> indexes into runs, ascending
	for i := 0; i < len(visible); {
		if visible[i] != '`' || escaped[i] {
			i++
			continue
		}
		n := runLen(visible[i:], '`')
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
		m.text(string(visible[pos:r.start]))
		pos = runs[j].start + runs[j].n
		i = j
	}
	m.text(string(visible[pos:]))
}

// escapeMask marks the bytes a backslash escapes (a backslash escapes the
// next byte, and an escaped backslash does not escape another byte). Linear
// and allocation-sized like the text.
func escapeMask(b []byte) []bool {
	escaped := make([]bool, len(b))
	for i := 0; i < len(b); i++ {
		if b[i] == '\\' && i+1 < len(b) {
			escaped[i+1] = true
			i++
		}
	}
	return escaped
}

// hideLinkDestinations blanks inline links and images (their text,
// destinations and titles) and reference links (text and label).
// Parentheses and brackets are paired in one stack pass, so the work is
// linear.
func hideLinkDestinations(b []byte, escaped []bool) {
	match := make([]int, len(b))
	for i := range match {
		match[i] = -1
	}
	var stack []int
	for i := 0; i < len(b); i++ {
		if escaped[i] {
			continue
		}
		switch b[i] {
		case '(':
			stack = append(stack, i)
		case ')':
			if n := len(stack); n > 0 {
				j := stack[n-1]
				stack = stack[:n-1]
				match[j] = i
			}
		}
	}
	nextClose := make([]int, len(b)+1)
	nextClose[len(b)] = -1
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] == ']' && !escaped[i] {
			nextClose[i] = i
		} else {
			nextClose[i] = nextClose[i+1]
		}
	}
	open := make([]int, len(b))
	for i := range open {
		open[i] = -1
	}
	var brackets []int
	for i := range b {
		if escaped[i] {
			continue
		}
		switch b[i] {
		case '[':
			brackets = append(brackets, i)
		case ']':
			if n := len(brackets); n > 0 {
				open[i] = brackets[n-1]
				brackets = brackets[:n-1]
			}
		}
	}
	for i := 0; i+1 < len(b); i++ {
		if escaped[i] || open[i] < 0 {
			continue
		}
		switch {
		case b[i] == ']' && b[i+1] == '(' && match[i+1] > i+1:
			hideRange(b, open[i], match[i+1]+1)
			i = match[i+1]
		case b[i] == ']' && b[i+1] == '[' && nextClose[i+2] > i+2:
			hideRange(b, open[i], nextClose[i+2]+1)
			i = nextClose[i+2]
		}
	}
}

// hideHTMLTags blanks raw HTML tags, comments, declarations, processing
// instructions and autolinks; the text between tags stays visible. The
// scan advances past every hidden range, so the work is linear.
func hideHTMLTags(b []byte, escaped []bool) {
	for i := 0; i+1 < len(b); i++ {
		if b[i] != '<' || escaped[i] {
			continue
		}
		if b[i+1] == '!' && i+3 < len(b) && b[i+2] == '-' && b[i+3] == '-' {
			j := indexFrom(b, i+4, "-->")
			if j < 0 {
				hideRange(b, i, len(b))
				return
			}
			hideRange(b, i, j+3)
			i = j + 2
			continue
		}
		if !isASCIILetter(b[i+1]) && b[i+1] != '/' && b[i+1] != '!' && b[i+1] != '?' {
			continue
		}
		quote := byte(0)
		end := -1
		for j := i + 1; j < len(b); j++ {
			switch {
			case quote != 0:
				if b[j] == quote {
					quote = 0
				}
			case b[j] == '"' || b[j] == '\'':
				quote = b[j]
			case b[j] == '>':
				end = j
			}
			if end >= 0 {
				break
			}
		}
		if end < 0 {
			// No ">" after this "<": no later "<" can find one either.
			return
		}
		hideRange(b, i, end+1)
		i = end
	}
}

func isASCIILetter(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// hideRange blanks b[start:end] with spaces.
func hideRange(b []byte, start, end int) {
	if start < 0 {
		start = 0
	}
	if end > len(b) {
		end = len(b)
	}
	for i := start; i < end; i++ {
		b[i] = ' '
	}
}

// indexFrom is bytes.Index of the string substr in b[from:], or -1.
func indexFrom(b []byte, from int, substr string) int {
	if from > len(b) {
		return -1
	}
	i := strings.Index(string(b[from:]), substr)
	if i < 0 {
		return -1
	}
	return from + i
}

// runLen is the length of the run of c at the start of b.
func runLen(b []byte, c byte) int {
	n := 0
	for n < len(b) && b[n] == c {
		n++
	}
	return n
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
