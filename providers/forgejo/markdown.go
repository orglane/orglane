package forgejo

import (
	"strconv"
	"strings"
)

// The Markdown of comments and descriptions is scanned for its text (where
// mentions are), not rendered. A line-by-line pass over CommonMark's block
// structure (https://spec.commonmark.org/0.31.2/) finds the inline content
// of paragraphs and headings and skips code blocks, HTML blocks and
// thematic breaks. Container blocks (block quotes and list items) are
// tracked as CommonMark does, with lazy continuation lines, so a fence in
// a quote is a fence and two list items are two blocks. Within inline
// content, code spans, raw HTML and autolinks are blanked and backslash
// escapes resolved; links, emphasis, entities and tables stay text.
//
// Every line is looked at a bounded number of times, containers nest at
// most maxNesting deep (deeper markers stay text) and searches for the end
// of raw HTML remember their results, so the work is linear in the length
// of the text: a large adversarial body cannot occupy a worker. Generic
// Markdown parsers do not promise that (goldmark takes seconds on a few
// hundred kilobytes of nested list markers, and longer on unclosed links).

// maxNesting bounds the open block quotes and list items: the blank lines
// a list item takes cost one step per open container.
const maxNesting = 32

// leaf is the kind of the open leaf block, at the innermost container.
type leaf int

const (
	noLeaf leaf = iota
	paragraphLeaf
	fencedLeaf
	indentedLeaf
	htmlLeaf
)

// container is an open block quote, or a list item whose content is
// indented width columns. A list item without content yet ends at a blank
// line (it starts with at most one).
type container struct {
	quote   bool
	width   int
	content bool
}

// blocks walks the block structure of a Markdown text and passes the
// inline content of each paragraph and heading to inline, until it returns
// false.
type blocks struct {
	inline  func(string) bool
	done    bool
	open    []container
	leaf    leaf
	para    []string
	fence   string // the opening fence of a fenced code block
	htmlEnd string // what ends an HTML block ("" for a blank line)
}

// inlineContent passes the inline content of each paragraph and heading in
// text to f, in order, until f returns false.
func inlineContent(text string, f func(string) bool) {
	b := &blocks{inline: f}
	for !b.done && text != "" {
		line := text
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			line, text = text[:i], text[i+1:]
		} else {
			text = ""
		}
		b.line(strings.TrimSuffix(line, "\r"))
	}
	if !b.done {
		b.closeLeaf()
	}
}

func (b *blocks) emit(s string) {
	if !b.done && !b.inline(s) {
		b.done = true
	}
}

// closeLeaf ends the open leaf block, passing on a paragraph.
func (b *blocks) closeLeaf() {
	if b.leaf == paragraphLeaf {
		b.emit(strings.Join(b.para, "\n"))
		b.para = b.para[:0]
	}
	b.leaf = noLeaf
}

// closeTo ends the open leaf and the containers after the first n.
func (b *blocks) closeTo(n int) {
	b.closeLeaf()
	b.open = b.open[:n]
}

// start is closeTo for a new block in the nth container.
func (b *blocks) start(n int) {
	b.closeTo(n)
	if n > 0 {
		b.open[n-1].content = true
	}
}

// line takes one line, as CommonMark's two phases do: it matches the open
// containers, then opens new blocks or continues the open leaf.
func (b *blocks) line(s string) {
	if strings.IndexByte(s, '\t') >= 0 {
		s = expandTabs(s)
	}
	pos, matched := 0, 0
	for ; matched < len(b.open); matched++ {
		c := b.open[matched]
		ind := indentAt(s, pos)
		if c.quote {
			if ind > 3 || pos+ind >= len(s) || s[pos+ind] != '>' {
				break
			}
			pos = quoteContent(s, pos+ind)
			continue
		}
		if pos+ind == len(s) {
			if !c.content {
				break
			}
			continue // a blank line goes on in a list item
		}
		if ind < c.width {
			break
		}
		pos += c.width
	}
	all := matched == len(b.open)
	if all {
		ind := indentAt(s, pos)
		blank := pos+ind == len(s)
		switch b.leaf {
		case fencedLeaf:
			if ind <= 3 && closesFence(s[pos+ind:], b.fence) {
				b.leaf = noLeaf
			}
			return
		case htmlLeaf:
			if b.htmlEnd == "" && blank || b.htmlEnd != "" && containsFold(s[pos:], b.htmlEnd) {
				b.leaf = noLeaf
			}
			return
		case indentedLeaf:
			if blank || ind >= 4 {
				return
			}
			b.leaf = noLeaf
		}
	}

	lazy := b.leaf == paragraphLeaf // the line may continue a paragraph
	afterPara := all && lazy        // the paragraph is the innermost block
	opened := false
	for {
		ind := indentAt(s, pos)
		r := s[pos+ind:]
		if r == "" {
			break
		}
		if ind >= 4 {
			if lazy && !opened || afterPara {
				break
			}
			b.start(matched)
			b.leaf = indentedLeaf
			return
		}
		if r[0] == '>' && len(b.open) < maxNesting {
			b.start(matched)
			b.open = append(b.open, container{quote: true})
			matched = len(b.open)
			pos = quoteContent(s, pos+ind)
			opened, afterPara = true, false
			continue
		}
		if heading, ok := atxHeading(r); ok {
			b.start(matched)
			b.emit(heading)
			return
		}
		if f := openingFence(r); f != "" {
			b.start(matched)
			b.leaf, b.fence = fencedLeaf, f
			return
		}
		if end, ok := htmlBlockStart(r, lazy && !opened); ok {
			b.start(matched)
			b.leaf, b.htmlEnd = htmlLeaf, end
			if end != "" && containsFold(r[1:], end) {
				b.leaf = noLeaf // it ends on its first line
			}
			return
		}
		if afterPara && setextUnderline(r) {
			b.closeLeaf() // the paragraph is a heading
			return
		}
		if thematicBreak(r) {
			b.start(matched)
			return
		}
		if width, ok := listItem(r, afterPara); ok && len(b.open) < maxNesting {
			b.start(matched)
			b.open = append(b.open, container{width: ind + width})
			matched = len(b.open)
			pos = min(pos+ind+width, len(s))
			opened, afterPara = true, false
			continue
		}
		break
	}

	r := strings.TrimLeft(s[pos:], " ")
	switch {
	case r == "":
		b.closeTo(matched)
	case lazy && !opened:
		b.para = append(b.para, r) // a continuation, maybe lazy
	default:
		b.start(matched)
		b.leaf = paragraphLeaf
		b.para = append(b.para, r)
	}
}

// expandTabs replaces the tabs of a line by spaces to the next multiple of
// four columns. Only the indentation matters to the block structure, and
// mentions treat both as whitespace.
func expandTabs(s string) string {
	var sb strings.Builder
	col := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\t' {
			n := 4 - col%4
			sb.WriteString("    "[:n])
			col += n
			continue
		}
		sb.WriteByte(s[i])
		col++
	}
	return sb.String()
}

// indentAt counts the spaces at s[pos:].
func indentAt(s string, pos int) int {
	n := 0
	for pos+n < len(s) && s[pos+n] == ' ' {
		n++
	}
	return n
}

// quoteContent is where the content of a block quote starts, given the
// position of its ">": after one optional space.
func quoteContent(s string, gt int) int {
	if gt+1 < len(s) && s[gt+1] == ' ' {
		return gt + 2
	}
	return gt + 1
}

// atxHeading returns the content of an ATX heading line ("# title").
func atxHeading(r string) (string, bool) {
	n := 0
	for n < len(r) && r[n] == '#' {
		n++
	}
	if n == 0 || n > 6 || n < len(r) && r[n] != ' ' {
		return "", false
	}
	return strings.TrimSpace(r[n:]), true
}

// fenceOf returns the run of three or more backticks or tildes at the
// start of s, or "".
func fenceOf(s string) string {
	if s == "" || s[0] != '`' && s[0] != '~' {
		return ""
	}
	i := 1
	for i < len(s) && s[i] == s[0] {
		i++
	}
	if i < 3 {
		return ""
	}
	return s[:i]
}

// openingFence returns the fence that opens a fenced code block in r, or
// "". A backtick fence's info string has no backtick: "```x``` @bob" is a
// code span in a paragraph.
func openingFence(r string) string {
	f := fenceOf(r)
	if f != "" && f[0] == '`' && strings.IndexByte(r[len(f):], '`') >= 0 {
		return ""
	}
	return f
}

// closesFence reports whether r closes the code block of the opening
// fence: a fence of the same character, at least as long, then spaces.
func closesFence(r, open string) bool {
	f := fenceOf(r)
	return f != "" && f[0] == open[0] && len(f) >= len(open) && strings.Trim(r[len(f):], " ") == ""
}

// setextUnderline reports whether r underlines a paragraph ("===", "---").
func setextUnderline(r string) bool {
	r = strings.TrimRight(r, " ")
	return r != "" && (r[0] == '=' || r[0] == '-') && strings.Count(r, r[:1]) == len(r)
}

// thematicBreak reports whether r is three or more "*", "-" or "_" with
// optional spaces.
func thematicBreak(r string) bool {
	c, n := r[0], 0
	if c != '*' && c != '-' && c != '_' {
		return false
	}
	for i := 0; i < len(r); i++ {
		switch r[i] {
		case c:
			n++
		case ' ':
		default:
			return false
		}
	}
	return n >= 3
}

// listItem returns the content indent of a list item that r starts ("-",
// "+", "*", or 1 to 9 digits and "." or ")", then a space or the end).
// After a paragraph, an item interrupts it only when it has content and,
// if ordered, starts at 1.
func listItem(r string, afterPara bool) (int, bool) {
	m := 0
	switch r[0] {
	case '-', '+', '*':
		m = 1
	default:
		for m < len(r) && m < 9 && r[m] >= '0' && r[m] <= '9' {
			m++
		}
		if m == 0 || m == len(r) || r[m] != '.' && r[m] != ')' {
			return 0, false
		}
		if afterPara {
			if n, _ := strconv.Atoi(r[:m]); n != 1 {
				return 0, false
			}
		}
		m++
	}
	if m < len(r) && r[m] != ' ' {
		return 0, false
	}
	spaces := indentAt(r, m)
	switch {
	case m+spaces == len(r):
		if afterPara {
			return 0, false // an empty item cannot interrupt a paragraph
		}
		return m + 1, true
	case spaces > 4:
		return m + 1, true // the content is an indented code block
	default:
		return m + spaces, true
	}
}

// htmlBlockStart reports whether r starts an HTML block and returns what
// ends it: a line containing the returned string, or a blank line for "".
// After a paragraph, maybe lazily continued, a lone tag of another name
// (type 7) is inline HTML.
func htmlBlockStart(r string, afterPara bool) (string, bool) {
	if r[0] != '<' {
		return "", false
	}
	lower := strings.ToLower(r[:min(len(r), 12)])
	for tag := range rawTextTags {
		if strings.HasPrefix(lower[1:], tag) && tagNameEnds(r, 1+len(tag)) {
			return "</" + tag + ">", true
		}
	}
	switch {
	case strings.HasPrefix(r, "<!--"):
		return "-->", true
	case strings.HasPrefix(r, "<?"):
		return "?>", true
	case strings.HasPrefix(r, "<![CDATA["):
		return "]]>", true
	case strings.HasPrefix(r, "<!") && len(r) > 2 && isASCIILetter(r[2]):
		return ">", true
	}
	name := 1
	if name < len(r) && r[name] == '/' {
		name++
	}
	end := name
	for end < len(r) && (isASCIILetter(r[end]) || end > name && r[end] >= '0' && r[end] <= '9') {
		end++
	}
	if end > name && htmlBlockTags[strings.ToLower(r[name:end])] &&
		(tagNameEnds(r, end) || strings.HasPrefix(r[end:], "/>")) {
		return "", true
	}
	if !afterPara {
		// Type 7: a complete open or closing tag alone on its line.
		f := finder{s: r}
		if end := f.tag(0); end > 0 && strings.Trim(r[end:], " ") == "" {
			return "", true
		}
	}
	return "", false
}

// tagNameEnds reports whether a tag name ends at r[i]: at a space, ">" or
// the end of the line.
func tagNameEnds(r string, i int) bool {
	return i == len(r) || r[i] == ' ' || r[i] == '>'
}

// rawTextTags are the tag names of CommonMark's HTML blocks of type 1,
// which end at their closing tag.
var rawTextTags = map[string]bool{"script": true, "pre": true, "style": true, "textarea": true}

// htmlBlockTags are the tag names of CommonMark's HTML blocks of type 6.
var htmlBlockTags = func() map[string]bool {
	m := map[string]bool{}
	for _, t := range strings.Fields(`address article aside base basefont blockquote body caption center col
		colgroup dd details dialog dir div dl dt fieldset figcaption figure footer form frame frameset
		h1 h2 h3 h4 h5 h6 head header hr html iframe legend li link main menu menuitem nav noframes ol
		optgroup option p param search section summary table tbody td tfoot th thead title tr track ul`) {
		m[t] = true
	}
	return m
}()

func isASCIILetter(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func isAttrNameStart(c byte) bool {
	return isASCIILetter(c) || c == '_' || c == ':'
}

// containsFold reports whether s contains the lower-case ASCII substr,
// ignoring case.
func containsFold(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if strings.EqualFold(s[i:i+len(substr)], substr) {
			return true
		}
	}
	return false
}

// inlineText is the text of inline content, as CommonMark reads it from
// left to right: code spans, raw HTML (tags, comments, declarations,
// processing instructions, CDATA) and autolinks are blanked, and backslash
// escapes are resolved ("\@" is "@"). A code span is a backtick run up to
// the next run of exactly the same length, across lines; a run without one
// stays literal. A backslash escapes the first backtick of a run outside
// code (it is literal, and the rest of the run may open a span), while
// inside a code span a backslash is literal, so an escaped run still
// closes one.
//
// The backtick runs are indexed once (the next closing run of each length,
// found from the end), and searches for the end of raw HTML remember their
// result, so the scan is linear.
func inlineText(s string) string {
	runs, next := backtickRuns(s)
	f := finder{s: s}
	var b strings.Builder
	b.Grow(len(s))
	k := 0
	for i := 0; i < len(s); {
		switch c := s[i]; {
		case c == '\\' && i+1 < len(s) && isASCIIPunct(s[i+1]):
			b.WriteByte(s[i+1])
			i += 2
		case c == '`':
			for runs[k].start+runs[k].n <= i {
				k++
			}
			r := runs[k]
			if next[k] < 0 || i != r.start+r.escaped {
				b.WriteString(s[i : r.start+r.n]) // literal backticks
				i = r.start + r.n
				k++
				continue
			}
			closing := runs[next[k]]
			b.WriteByte(' ')
			i = closing.start + closing.n
			k = next[k] + 1
		case c == '<':
			if end := f.rawHTML(i); end > 0 {
				b.WriteByte(' ')
				i = end
				continue
			}
			b.WriteByte(c)
			i++
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

// backtickRun is a maximal run of n backticks at start; escaped is 1 if a
// backslash escapes its first backtick.
type backtickRun struct{ start, n, escaped int }

// backtickRuns returns the backtick runs of s and, for each, the index of
// the run that closes the code span it opens, or -1.
func backtickRuns(s string) ([]backtickRun, []int) {
	var runs []backtickRun
	for i := 0; i < len(s); {
		if s[i] != '`' {
			i++
			continue
		}
		backslashes := 0
		for k := i - 1; k >= 0 && s[k] == '\\'; k-- {
			backslashes++
		}
		j := i
		for j < len(s) && s[j] == '`' {
			j++
		}
		runs = append(runs, backtickRun{i, j - i, backslashes % 2})
		i = j
	}
	next := make([]int, len(runs))
	last := map[int]int{}
	for k := len(runs) - 1; k >= 0; k-- {
		next[k] = -1
		if open := runs[k].n - runs[k].escaped; open > 0 {
			if v, ok := last[open]; ok {
				next[k] = v
			}
		}
		last[runs[k].n] = k
	}
	return runs, next
}

// finder finds strings in s at or after a position. It remembers the last
// result for each string, and the positions mostly grow, so the searches
// for the end of unclosed comments or quotes do not rescan the text.
type finder struct {
	s    string
	memo map[string][2]int // the position searched from, the result
}

// index returns the position of sub in s at or after from, or -1.
func (f *finder) index(from int, sub string) int {
	if m, ok := f.memo[sub]; ok && m[0] <= from && (m[1] < 0 || from <= m[1]) {
		return m[1]
	}
	i := strings.Index(f.s[from:], sub)
	if i >= 0 {
		i += from
	}
	if f.memo == nil {
		f.memo = map[string][2]int{}
	}
	f.memo[sub] = [2]int{from, i}
	return i
}

// after returns the position after sub in s at or after from, or -1.
func (f *finder) after(from int, sub string) int {
	if i := f.index(from, sub); i >= 0 {
		return i + len(sub)
	}
	return -1
}

// rawHTML returns the end of the raw HTML or autolink starting at s[i]
// ("<"), or -1.
func (f *finder) rawHTML(i int) int {
	if end := autolink(f.s, i); end > 0 {
		return end
	}
	r := f.s[i:]
	switch {
	case strings.HasPrefix(r, "<!-->"):
		return i + 5
	case strings.HasPrefix(r, "<!--->"):
		return i + 6
	case strings.HasPrefix(r, "<!--"):
		return f.after(i+4, "-->")
	case strings.HasPrefix(r, "<?"):
		return f.after(i+2, "?>")
	case strings.HasPrefix(r, "<![CDATA["):
		return f.after(i+9, "]]>")
	case len(r) > 2 && r[1] == '!' && isASCIILetter(r[2]):
		return f.after(i+3, ">")
	}
	return f.tag(i)
}

// tag returns the end of the open or closing tag at s[i], or -1.
func (f *finder) tag(i int) int {
	s := f.s
	j := i + 1
	closing := j < len(s) && s[j] == '/'
	if closing {
		j++
	}
	if j == len(s) || !isASCIILetter(s[j]) {
		return -1
	}
	for j < len(s) && (isASCIILetter(s[j]) || isDigit(s[j]) || s[j] == '-') {
		j++
	}
	for !closing {
		k := skipSpace(s, j)
		if k == j || k == len(s) || !isAttrNameStart(s[k]) {
			break
		}
		j = k + 1
		for j < len(s) && (isAttrNameStart(s[j]) || isDigit(s[j]) || s[j] == '.' || s[j] == '-') {
			j++
		}
		k = skipSpace(s, j)
		if k == len(s) || s[k] != '=' {
			continue // an attribute without a value
		}
		k = skipSpace(s, k+1)
		if k == len(s) {
			return -1
		}
		if q := s[k : k+1]; q == `"` || q == "'" {
			if j = f.after(k+1, q); j < 0 {
				return -1
			}
			continue
		}
		j = k
		for j < len(s) && !strings.ContainsRune(" \t\n\"'=<>`", rune(s[j])) {
			j++
		}
		if j == k {
			return -1
		}
	}
	j = skipSpace(s, j)
	if !closing && strings.HasPrefix(s[j:], "/>") {
		return j + 2
	}
	if j < len(s) && s[j] == '>' {
		return j + 1
	}
	return -1
}

// autolink returns the end of the URI or e-mail autolink at s[i] ("<"), or
// -1. It ends at the first space or "<", so it is short to look for.
func autolink(s string, i int) int {
	j := i + 1
	for j < len(s) && j-i <= 33 && (isASCIILetter(s[j]) || j > i+1 && (isDigit(s[j]) || s[j] == '+' || s[j] == '.' || s[j] == '-')) {
		j++
	}
	if n := j - i - 1; n >= 2 && n <= 32 && j < len(s) && s[j] == ':' {
		j++
		for j < len(s) && s[j] > ' ' && s[j] != '<' && s[j] != '>' {
			j++
		}
		if j < len(s) && s[j] == '>' {
			return j + 1
		}
		return -1
	}
	j = i + 1
	for j < len(s) && (isASCIILetter(s[j]) || isDigit(s[j]) || strings.IndexByte(".!#$%&'*+/=?^_`{|}~-", s[j]) >= 0) {
		j++
	}
	if j == i+1 || j == len(s) || s[j] != '@' {
		return -1
	}
	for {
		start := j + 1
		j = start
		for j < len(s) && j-start < 63 && (isASCIILetter(s[j]) || isDigit(s[j]) || s[j] == '-') {
			j++
		}
		if j == start || s[start] == '-' || s[j-1] == '-' || j == len(s) {
			return -1
		}
		switch s[j] {
		case '>':
			return j + 1
		case '.':
			continue
		default:
			return -1
		}
	}
}

func skipSpace(s string, i int) int {
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n') {
		i++
	}
	return i
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isASCIIPunct(c byte) bool {
	return c >= '!' && c <= '/' || c >= ':' && c <= '@' || c >= '[' && c <= '`' || c >= '{' && c <= '~'
}
