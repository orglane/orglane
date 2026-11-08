package forgejo

import (
	"strconv"
	"strings"
)

// The Markdown of comments and descriptions is scanned for its text (where
// mentions are), not rendered, as CommonMark 0.31.2
// (https://spec.commonmark.org/0.31.2/) and its reference implementation
// commonmark.js parse it. This file is the block structure: a pass over the
// lines (ended by LF, CRLF or CR) that tracks container blocks (block
// quotes and list items, with lazy continuation lines and tabs counted in
// columns) and leaf blocks (ATX and setext headings, fenced and indented
// code, the seven kinds of HTML blocks, thematic breaks and paragraphs).
// Paragraphs lose their link reference definitions; the inline content of
// what remains, and of headings, is read by markdown_inline.go. Code and
// HTML blocks have no text.
//
// Every line is looked at a bounded number of times, and containers nest at
// most maxNesting deep (deeper markers stay text), so the work is linear in
// the length of the text: a large adversarial body cannot occupy a worker.
// Generic Markdown parsers do not promise that (goldmark takes seconds on a
// few hundred kilobytes of nested list markers, and longer on unclosed
// links).

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

// textBlock is the inline content of a paragraph (which may start with
// link reference definitions) or a heading.
type textBlock struct {
	content   string
	paragraph bool
}

// blocks is the block structure of a Markdown text while its lines are
// read.
type blocks struct {
	open      []container
	leaf      leaf
	para      []string // the lines of the open paragraph
	fenceChar byte
	fenceLen  int
	htmlType  int // the open HTML block's kind, 1 to 7
	text      []textBlock
	refs      map[string]bool // the normalized labels of link reference definitions
}

// textBlocks passes the text of each paragraph and heading in the Markdown
// text to f, in order, until f returns false.
func textBlocks(text string, f func(string) bool) {
	b := &blocks{refs: map[string]bool{}}
	text = strings.ReplaceAll(text, "\x00", "�")
	for text != "" {
		i := strings.IndexAny(text, "\r\n")
		if i < 0 {
			b.line(text)
			break
		}
		b.line(text[:i])
		if text[i] == '\r' && i+1 < len(text) && text[i+1] == '\n' {
			i++
		}
		text = text[i+1:]
	}
	b.closeTo(0)
	// Reference definitions apply to the whole document, so the inline
	// content is read once every paragraph has given up its definitions.
	for i := range b.text {
		if !b.text[i].paragraph {
			continue
		}
		content := b.text[i].content
		n := stripReferences(content, b.refs)
		if content = content[n:]; n > 0 && strings.Trim(content, " \t\f\v\r\n") == "" {
			content = ""
		}
		b.text[i].content = content
	}
	for _, t := range b.text {
		if t.content != "" && !f(inlineText(trimJS(t.content), b.refs)) {
			return
		}
	}
}

// stripReferences returns the length of the link reference definitions at
// the start of a paragraph's content, adding their labels to refs.
func stripReferences(content string, refs map[string]bool) int {
	if content == "" || content[0] != '[' {
		return 0
	}
	p := newInlineScanner(content, refs)
	pos := 0
	for pos < len(content) && content[pos] == '[' {
		n := p.reference(pos)
		if n == 0 {
			break
		}
		pos += n
	}
	return pos
}

// closeLeaf ends the open leaf block, keeping a paragraph's content.
func (b *blocks) closeLeaf() {
	if b.leaf == paragraphLeaf {
		b.text = append(b.text, textBlock{content: b.paraContent(), paragraph: true})
		b.para = b.para[:0]
	}
	b.leaf = noLeaf
}

// paraContent is the open paragraph's content: its lines, each ended by LF.
func (b *blocks) paraContent() string {
	if len(b.para) == 0 {
		return ""
	}
	return strings.Join(b.para, "\n") + "\n"
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

// line takes one line as commonmark.js does: it matches the open
// containers and the open leaf, then opens new blocks, then adds the rest
// of the line to a paragraph (maybe lazily).
func (b *blocks) line(s string) {
	l := &cursor{s: s}
	matched := 0
	for ; matched < len(b.open); matched++ {
		l.findNextNonspace()
		c := b.open[matched]
		if c.quote {
			if l.indented || l.peek(l.nextNonspace) != '>' {
				break
			}
			l.advanceNextNonspace()
			l.advanceOffset(1, false)
			if isSpaceOrTab(l.peek(l.offset)) {
				l.advanceOffset(1, true)
			}
			continue
		}
		if l.blank {
			if !c.content {
				break
			}
			l.advanceNextNonspace()
			continue
		}
		if l.indent < c.width {
			break
		}
		l.advanceOffset(c.width, true)
	}
	all := matched == len(b.open)
	if all {
		l.findNextNonspace()
		switch b.leaf {
		case fencedLeaf:
			if l.indent <= 3 && closesFence(s[l.nextNonspace:], b.fenceChar, b.fenceLen) {
				b.leaf = noLeaf
			}
			return
		case htmlLeaf:
			if l.blank && b.htmlType >= 6 {
				b.leaf = noLeaf
			} else if b.htmlType <= 5 && htmlBlockEnds(b.htmlType, s[l.offset:]) {
				b.leaf = noLeaf
			}
			return
		case indentedLeaf:
			if l.indent >= 4 || l.blank {
				return
			}
			b.leaf = noLeaf
		case paragraphLeaf:
			if l.blank {
				b.closeLeaf()
				return
			}
		}
	}

	tipPara := b.leaf == paragraphLeaf // the paragraph is the innermost open block
	containerPara := all && tipPara    // and the line continues it
	for {
		l.findNextNonspace()
		r := s[l.nextNonspace:]
		if !l.indented && (r == "" || !strings.ContainsRune("#`~*+_=<>0123456789-", rune(r[0]))) {
			l.advanceNextNonspace()
			break
		}
		if !l.indented {
			if r[0] == '>' && len(b.open) < maxNesting {
				l.advanceNextNonspace()
				l.advanceOffset(1, false)
				if isSpaceOrTab(l.peek(l.offset)) {
					l.advanceOffset(1, true)
				}
				b.start(matched)
				b.open = append(b.open, container{quote: true})
				matched = len(b.open)
				tipPara, containerPara = false, false
				continue
			}
			if n := atxMarker(r); n > 0 {
				l.advanceNextNonspace()
				l.advanceOffset(n, false)
				b.start(matched)
				b.text = append(b.text, textBlock{content: atxContent(s[l.offset:])})
				return
			}
			if f := openingFence(r); f != "" {
				b.start(matched)
				b.leaf, b.fenceChar, b.fenceLen = fencedLeaf, f[0], len(f)
				return
			}
			if t := htmlBlockStart(r, !tipPara); t > 0 {
				b.start(matched)
				b.leaf, b.htmlType = htmlLeaf, t
				if t <= 5 && htmlBlockEnds(t, s[l.offset:]) {
					b.leaf = noLeaf // it ends on its first line
				}
				return
			}
			if containerPara && setextUnderline(r) {
				content := b.paraContent()
				content = content[stripReferences(content, b.refs):]
				b.para = b.para[:0]
				if content != "" {
					b.leaf = noLeaf // the paragraph is a heading
					b.text = append(b.text, textBlock{content: content})
					return
				}
				// Only definitions: the paragraph stays open, empty.
			}
			if thematicBreak(r) {
				b.start(matched)
				return
			}
			if len(b.open) < maxNesting {
				if width, ok := l.listItem(containerPara); ok {
					b.start(matched)
					b.open = append(b.open, container{width: width})
					matched = len(b.open)
					tipPara, containerPara = false, false
					continue
				}
			}
		}
		if l.indented && !tipPara && !l.blank {
			l.advanceOffset(4, true)
			b.start(matched)
			b.leaf = indentedLeaf
			return
		}
		l.advanceNextNonspace()
		break
	}

	switch {
	case tipPara && !l.blank:
		b.para = append(b.para, l.rest()) // a continuation, maybe lazy
	case l.blank:
		b.closeTo(matched)
	default:
		b.start(matched)
		b.leaf = paragraphLeaf
		b.para = append(b.para, l.rest())
	}
}

// cursor is a position in a line as commonmark.js tracks it: a byte offset,
// its column (tabs stop every four columns) and whether the tab at the
// offset is partly consumed; and the next character that is no space or
// tab.
type cursor struct {
	s                  string
	offset, column     int
	partialTab         bool
	nextNonspace       int
	nextNonspaceColumn int
	indent             int
	indented, blank    bool
}

// peek returns the byte at i, or -1 at the end.
func (l *cursor) peek(i int) int {
	if i < len(l.s) {
		return int(l.s[i])
	}
	return -1
}

// findNextNonspace finds the next character after the offset that is no
// space or tab, and the indentation up to it.
func (l *cursor) findNextNonspace() {
	i, cols := l.offset, l.column
loop:
	for ; i < len(l.s); i++ {
		switch l.s[i] {
		case ' ':
			cols++
		case '\t':
			cols += 4 - cols%4
		default:
			break loop
		}
	}
	l.blank = i == len(l.s)
	l.nextNonspace, l.nextNonspaceColumn = i, cols
	l.indent = cols - l.column
	l.indented = l.indent >= 4
}

// advanceNextNonspace moves the offset to the next non-space character.
func (l *cursor) advanceNextNonspace() {
	l.offset, l.column, l.partialTab = l.nextNonspace, l.nextNonspaceColumn, false
}

// advanceOffset moves count bytes, or count columns (where a tab may be
// consumed in part).
func (l *cursor) advanceOffset(count int, columns bool) {
	for count > 0 && l.offset < len(l.s) {
		if l.s[l.offset] != '\t' {
			l.partialTab = false
			l.offset++
			l.column++
			count--
			continue
		}
		toTab := 4 - l.column%4
		if !columns {
			l.partialTab = false
			l.column += toTab
			l.offset++
			count--
			continue
		}
		l.partialTab = toTab > count
		step := min(toTab, count)
		l.column += step
		if !l.partialTab {
			l.offset++
		}
		count -= step
	}
}

// rest is the line from the offset, the rest of a partly consumed tab as
// spaces.
func (l *cursor) rest() string {
	if l.partialTab {
		return "    "[:4-l.column%4] + l.s[l.offset+1:]
	}
	return l.s[l.offset:]
}

// listItem starts a list item at the next non-space character if there is
// a list marker ("-", "+", "*", or 1 to 9 digits and "." or ")", then a
// space, a tab or the end), and returns the item's content indent. After a
// paragraph, an item interrupts it only when it has content and, if
// ordered, starts at 1.
func (l *cursor) listItem(afterPara bool) (int, bool) {
	r := l.s[l.nextNonspace:]
	m := 0
	switch r[0] {
	case '-', '+', '*':
		m = 1
	default:
		for m < len(r) && m < 9 && isDigit(r[m]) {
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
	if m < len(r) && !isSpaceOrTab(int(r[m])) {
		return 0, false
	}
	if afterPara && strings.Trim(r[m:], " \t\f\v\r\n") == "" {
		return 0, false // an empty item cannot interrupt a paragraph
	}
	markerOffset := l.indent
	l.advanceNextNonspace()
	l.advanceOffset(m, true)
	startCol, startOffset := l.column, l.offset
	for {
		l.advanceOffset(1, true)
		if l.column-startCol >= 5 || !isSpaceOrTab(l.peek(l.offset)) {
			break
		}
	}
	spaces := l.column - startCol
	if spaces >= 5 || spaces < 1 || l.offset >= len(l.s) {
		// The content is an indented code block, or the item is empty.
		l.column, l.offset, l.partialTab = startCol, startOffset, false
		if isSpaceOrTab(l.peek(l.offset)) {
			l.advanceOffset(1, true)
		}
		return markerOffset + m + 1, true
	}
	return markerOffset + m + spaces, true
}

// isSpaceOrTab reports whether the byte c (or -1) is a space or a tab.
func isSpaceOrTab(c int) bool { return c == ' ' || c == '\t' }

// atxMarker returns the length of an ATX heading's opening sequence and the
// spaces after it ("## "), or 0.
func atxMarker(r string) int {
	n := 0
	for n < len(r) && r[n] == '#' {
		n++
	}
	if n == 0 || n > 6 {
		return 0
	}
	if n == len(r) {
		return n
	}
	if !isSpaceOrTab(int(r[n])) {
		return 0
	}
	for n < len(r) && isSpaceOrTab(int(r[n])) {
		n++
	}
	return n
}

// atxContent is an ATX heading's content without its closing sequence.
func atxContent(s string) string {
	t := strings.TrimRight(s, " \t")
	j := len(t)
	for j > 0 && t[j-1] == '#' {
		j--
	}
	switch {
	case j == len(t):
		return s
	case strings.Trim(t[:j], " \t") == "":
		return ""
	case t[j-1] == ' ' || t[j-1] == '\t':
		return strings.TrimRight(t[:j], " \t")
	}
	return s
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

// closesFence reports whether r closes a code block opened by a fence of
// n times c: a fence of the same character, at least as long, then spaces
// or tabs.
func closesFence(r string, c byte, n int) bool {
	f := fenceOf(r)
	return f != "" && f[0] == c && len(f) >= n && strings.Trim(r[len(f):], " \t") == ""
}

// setextUnderline reports whether r underlines a paragraph ("===", "---").
func setextUnderline(r string) bool {
	r = strings.TrimRight(r, " \t")
	return r != "" && (r[0] == '=' || r[0] == '-') && strings.Count(r, r[:1]) == len(r)
}

// thematicBreak reports whether r is three or more "*", "-" or "_" with
// optional spaces or tabs.
func thematicBreak(r string) bool {
	c, n := r[0], 0
	if c != '*' && c != '-' && c != '_' {
		return false
	}
	for i := 0; i < len(r); i++ {
		switch r[i] {
		case c:
			n++
		case ' ', '\t':
		default:
			return false
		}
	}
	return n >= 3
}

// htmlBlockStart returns the kind (1 to 7) of the HTML block that r
// starts, or 0. A lone tag of another name (kind 7) cannot interrupt a
// paragraph, also not a lazily continued one.
func htmlBlockStart(r string, lone bool) int {
	if r[0] != '<' {
		return 0
	}
	lower := strings.ToLower(r[:min(len(r), 12)])
	for _, tag := range rawTextTags {
		if strings.HasPrefix(lower[1:], tag) && (len(r) == 1+len(tag) || r[1+len(tag)] == '>' || isJSSpaceAt(r, 1+len(tag))) {
			return 1
		}
	}
	switch {
	case strings.HasPrefix(r, "<!--"):
		return 2
	case strings.HasPrefix(r, "<?"):
		return 3
	case len(r) > 2 && r[1] == '!' && isASCIILetter(r[2]):
		return 4
	case strings.HasPrefix(r, "<![CDATA["):
		return 5
	}
	name := 1
	if name < len(r) && r[name] == '/' {
		name++
	}
	end := name
	for end < len(r) && (isASCIILetter(r[end]) || end > name && isDigit(r[end])) {
		end++
	}
	if end > name && htmlBlockTags[strings.ToLower(r[name:end])] &&
		(end == len(r) || r[end] == '>' || strings.HasPrefix(r[end:], "/>") || isJSSpaceAt(r, end)) {
		return 6
	}
	if lone {
		f := finder{s: r}
		if end := f.tag(0); end > 0 && trimJS(r[end:]) == "" {
			return 7
		}
	}
	return 0
}

// rawTextTags are the tag names of CommonMark's HTML blocks of kind 1,
// which end at a line with the closing tag of any of them.
var rawTextTags = []string{"script", "pre", "textarea", "style"}

// htmlBlockEnds reports whether the line s ends an HTML block of kind t
// (1 to 5).
func htmlBlockEnds(t int, s string) bool {
	switch t {
	case 1:
		for _, tag := range rawTextTags {
			if containsFold(s, "</"+tag+">") {
				return true
			}
		}
		return false
	case 2:
		return strings.Contains(s, "-->")
	case 3:
		return strings.Contains(s, "?>")
	case 4:
		return strings.Contains(s, ">")
	default:
		return strings.Contains(s, "]]>")
	}
}

// htmlBlockTags are the tag names of CommonMark's HTML blocks of kind 6.
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
