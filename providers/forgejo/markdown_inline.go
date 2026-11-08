package forgejo

import (
	"strings"
	"unicode/utf8"

	"golang.org/x/text/cases"
)

// The inline content of a paragraph or heading is read from left to right
// as commonmark.js reads it, keeping only its text: code spans, raw HTML
// (tags, comments, declarations, processing instructions, CDATA),
// autolinks, images and the destinations, titles and labels of links are
// blanked; the text of links stays; backslash escapes are resolved. Link
// reference definitions are recognized, so "[x][label]" is a link only if
// "label" is defined. Emphasis and entities stay as they are written.
//
// Each step is bounded: the backtick runs are indexed once (the next
// closing run of each length, found from the end); a link destination's end
// is looked up in an index of its parentheses' nesting (the first position
// where it drops below the start's); link labels are at most 1000
// characters; and searches for the end of a title, of raw HTML or of a
// destination remember their result. The scan is linear in the length of
// the content.

// inlineScanner reads one inline content.
type inlineScanner struct {
	s    string
	refs map[string]bool
	f    finder
	// bal[i] is the nesting of unescaped parentheses before s[i], and
	// lower[i] the first j > i with bal[j] < bal[i], or -1; built on the
	// first link destination without angle brackets.
	bal, lower []int32
}

func newInlineScanner(s string, refs map[string]bool) *inlineScanner {
	return &inlineScanner{s: s, refs: refs, f: finder{s: s}}
}

// bracket is an open "[" or "![" of a possible link or image.
type bracket struct {
	index        int // of "[" in the content
	out          int // of "[" (or "!") in the text
	image        bool
	bracketAfter bool // another bracket opened after this one
}

// inlineText is the text of inline content s.
func inlineText(s string, refs map[string]bool) string {
	p := newInlineScanner(s, refs)
	runs, next := backtickRuns(s)
	out := make([]byte, 0, len(s))
	var stack []bracket
	inactiveBelow := 0 // the link openers below it in the stack are inactive
	pop := func() {
		stack = stack[:len(stack)-1]
		inactiveBelow = min(inactiveBelow, len(stack))
	}
	push := func(b bracket) {
		if len(stack) > 0 {
			stack[len(stack)-1].bracketAfter = true
		}
		stack = append(stack, b)
	}
	k := 0 // the next backtick run
	for i := 0; i < len(s); {
		switch c := s[i]; c {
		case '\\':
			switch {
			case i+1 < len(s) && s[i+1] == '\n':
				out = append(out, '\n') // a hard line break
				i += 2
			case i+1 < len(s) && isASCIIPunct(s[i+1]):
				out = append(out, s[i+1])
				i += 2
			default:
				out = append(out, c)
				i++
			}
		case '`':
			for runs[k].start+runs[k].n <= i {
				k++
			}
			r := runs[k]
			if next[k] < 0 || i != r.start+r.escaped {
				out = append(out, s[i:r.start+r.n]...) // literal backticks
				i = r.start + r.n
				k++
				continue
			}
			closing := runs[next[k]]
			out = append(out, ' ')
			i = closing.start + closing.n
			k = next[k] + 1
		case '<':
			if end := p.f.rawHTML(i); end > 0 {
				out = append(out, ' ')
				i = end
				continue
			}
			out = append(out, c)
			i++
		case '[':
			push(bracket{index: i, out: len(out)})
			out = append(out, c)
			i++
		case '!':
			if i+1 < len(s) && s[i+1] == '[' {
				push(bracket{index: i + 1, out: len(out), image: true})
				out = append(out, "!["...)
				i += 2
				continue
			}
			out = append(out, c)
			i++
		case ']':
			start := i + 1
			if len(stack) == 0 {
				out = append(out, c)
				i = start
				continue
			}
			op := stack[len(stack)-1]
			if !op.image && len(stack)-1 < inactiveBelow {
				out = append(out, c) // no links in links
				pop()
				i = start
				continue
			}
			end, ok := p.linkEnd(start, op)
			pop()
			if !ok {
				out = append(out, c)
				i = start
				continue
			}
			if op.image {
				out = append(out[:op.out], ' ') // its description is no text
			} else {
				out[op.out] = ' '
				out = append(out, ' ')
				inactiveBelow = len(stack)
			}
			i = end
		default:
			out = append(out, c)
			i++
		}
	}
	return string(out)
}

// linkEnd returns the end of the link or image whose text ends with the
// "]" before s[start] (an inline link, or a full, collapsed or shortcut
// reference to a defined label), or false.
func (p *inlineScanner) linkEnd(start int, op bracket) (int, bool) {
	s := p.s
	if start < len(s) && s[start] == '(' {
		if end, ok := p.inlineLink(start + 1); ok {
			return end, true
		}
	}
	n := p.linkLabel(start)
	label := ""
	switch {
	case n > 2:
		label = s[start : start+n]
	case !op.bracketAfter:
		label = s[op.index:start]
	}
	if label != "" && p.refs[normalizeLabel(label)] {
		return start + n, true
	}
	return 0, false
}

// inlineLink returns the end of an inline link's "(destination title)"
// whose destination starts at s[i] (after "("), or false.
func (p *inlineScanner) inlineLink(i int) (int, bool) {
	s := p.s
	end, ok := p.destination(p.spnl(i))
	if !ok {
		return 0, false
	}
	j := p.spnl(end)
	if j > 0 && isWSChar(s[j-1]) && j < len(s) {
		if t, ok := p.title(j); ok {
			j = t
		}
	}
	if j = p.spnl(j); j < len(s) && s[j] == ')' {
		return j + 1, true
	}
	return 0, false
}

// reference parses a link reference definition at s[pos] and adds its
// label to refs; it returns the definition's length, or 0.
func (p *inlineScanner) reference(pos int) int {
	s := p.s
	n := p.linkLabel(pos)
	if n == 0 || pos+n >= len(s) || s[pos+n] != ':' {
		return 0
	}
	label := s[pos : pos+n]
	end, ok := p.destination(p.spnl(pos + n + 1))
	if !ok {
		return 0
	}
	j, titled := p.spnl(end), false
	if j != end {
		if t, ok := p.title(j); ok {
			j, titled = t, true
		} else {
			j = end
		}
	}
	lineEnd, ok := atLineEnd(s, j)
	if !ok && titled {
		lineEnd, ok = atLineEnd(s, end) // without the title, which must end its line
	}
	if !ok {
		return 0
	}
	key := normalizeLabel(label)
	if key == "" {
		return 0
	}
	p.refs[key] = true
	return lineEnd - pos
}

// atLineEnd skips spaces and tabs at s[i] and returns the position after
// the line ending, if nothing else is left on the line.
func atLineEnd(s string, i int) (int, bool) {
	for i < len(s) && isSpaceOrTab(int(s[i])) {
		i++
	}
	switch {
	case i == len(s):
		return i, true
	case s[i] == '\n':
		return i + 1, true
	}
	return 0, false
}

// spnl skips spaces and tabs, including at most one line ending (the
// spec's and cmark's rule; commonmark.js skips spaces only).
func (p *inlineScanner) spnl(i int) int {
	s := p.s
	for i < len(s) && isSpaceOrTab(int(s[i])) {
		i++
	}
	if i < len(s) && s[i] == '\n' {
		i++
		for i < len(s) && isSpaceOrTab(int(s[i])) {
			i++
		}
	}
	return i
}

// destination returns the end of a link destination at s[i]: in angle
// brackets, or up to whitespace or an unbalanced ")" (which may end it
// right away: the destination is empty).
func (p *inlineScanner) destination(i int) (int, bool) {
	s := p.s
	if i < len(s) && s[i] == '<' {
		for j := i + 1; j < len(s); j++ {
			switch s[j] {
			case '>':
				return j + 1, true
			case '<', '\n':
				return 0, false
			case '\\':
				if j+1 == len(s) || s[j+1] == '\n' {
					return 0, false
				}
				j++
			}
		}
		return 0, false
	}
	p.indexParentheses()
	ws := p.f.indexAny(i, " \t\n\v\f\r")
	if ws < 0 {
		ws = len(s)
	}
	if y := p.lower[i]; y >= 0 && int(y)-1 < ws {
		return int(y) - 1, true // the ")" that ends the link
	}
	if ws == i || p.bal[ws] != p.bal[i] {
		return 0, false
	}
	return ws, true
}

// indexParentheses builds bal and lower.
func (p *inlineScanner) indexParentheses() {
	if p.bal != nil {
		return
	}
	s := p.s
	p.bal = make([]int32, len(s)+1)
	backslashes := 0
	for i := 0; i < len(s); i++ {
		d := int32(0)
		if backslashes%2 == 0 {
			switch s[i] {
			case '(':
				d = 1
			case ')':
				d = -1
			}
		}
		p.bal[i+1] = p.bal[i] + d
		if s[i] == '\\' {
			backslashes++
		} else {
			backslashes = 0
		}
	}
	p.lower = make([]int32, len(s)+1)
	var stack []int32
	for y := range p.bal {
		for len(stack) > 0 && p.bal[y] < p.bal[stack[len(stack)-1]] {
			p.lower[stack[len(stack)-1]] = int32(y)
			stack = stack[:len(stack)-1]
		}
		stack = append(stack, int32(y))
	}
	for _, x := range stack {
		p.lower[x] = -1
	}
}

// title returns the end of a link title at s[i]: in double or single
// quotes, or in parentheses (without unescaped ones inside).
func (p *inlineScanner) title(i int) (int, bool) {
	if i >= len(p.s) {
		return 0, false
	}
	set := ""
	switch p.s[i] {
	case '"':
		set = `"`
	case '\'':
		set = "'"
	case '(':
		set = "()"
	default:
		return 0, false
	}
	j := p.f.unescaped(i+1, set)
	if j < 0 || p.s[j] == '(' {
		return 0, false
	}
	return j + 1, true
}

// linkLabel returns the length of a link label at s[i] ("[...]": at most
// 1000 characters, no unescaped brackets), or 0.
func (p *inlineScanner) linkLabel(i int) int {
	s := p.s
	if i >= len(s) || s[i] != '[' {
		return 0
	}
	items, units := 0, 1 // characters inside, UTF-16 units of the label
	for j := i + 1; j < len(s); {
		if items > 1000 {
			return 0
		}
		switch s[j] {
		case ']':
			if units+1 > 1001 {
				return 0
			}
			return j + 1 - i
		case '[':
			return 0
		case '\\':
			if j+1 == len(s) {
				return 0
			}
			r, w := utf8.DecodeRuneInString(s[j+1:])
			units += 1 + utf16Len(r)
			j += 1 + w
		default:
			r, w := utf8.DecodeRuneInString(s[j:])
			units += utf16Len(r)
			j += w
		}
		items++
	}
	return 0
}

func utf16Len(r rune) int {
	if r >= 0x10000 {
		return 2
	}
	return 1
}

// labelCaser is Unicode case folding, which CommonMark applies to link
// labels. It is full folding: it expands "ß" to "ss" and "ﬀ" to "ff".
// cases.Fold is stateless, so the shared Caser is safe for concurrent use.
var labelCaser = cases.Fold()

// foldLabel is the full Unicode case fold of a link label. CaseFolding.txt
// maps the Cherokee small letters to the capital letters (U+AB70 to
// U+13A0, U+13F8 to U+13F0) and leaves the capital letters unchanged.
// golang.org/x/text v0.43.0 instead folds a capital letter to its small
// letter, because it applies the simple lower-case mapping, so map the
// small letters it returns back to the capitals. The mapping is idempotent
// and does not change the fold of any other script.
func foldLabel(s string) string {
	folded := labelCaser.String(s)
	if !strings.ContainsFunc(folded, isCherokeeSmall) {
		return folded
	}
	return strings.Map(foldCherokee, folded)
}

func isCherokeeSmall(r rune) bool {
	return ('\uAB70' <= r && r <= '\uABBF') || ('\u13F8' <= r && r <= '\u13FD')
}

func foldCherokee(r rune) rune {
	switch {
	case '\uAB70' <= r && r <= '\uABBF':
		return r - '\uAB70' + '\u13A0'
	case '\u13F8' <= r && r <= '\u13FD':
		return r - '\u13F8' + '\u13F0'
	}
	return r
}

// normalizeLabel is a link label as definitions and references are
// matched: without brackets, trimmed, inner whitespace collapsed, case
// folded.
func normalizeLabel(label string) string {
	inner := trimJS(label[1 : len(label)-1])
	var b strings.Builder
	space := false
	for i := 0; i < len(inner); i++ {
		switch c := inner[i]; c {
		case ' ', '\t', '\r', '\n':
			space = true
		default:
			if space {
				b.WriteByte(' ')
				space = false
			}
			b.WriteByte(c)
		}
	}
	return foldLabel(b.String())
}

// backtickRun is a maximal run of n backticks at start; escaped is 1 if a
// backslash escapes its first backtick.
type backtickRun struct{ start, n, escaped int }

// backtickRuns returns the backtick runs of s and, for each, the index of
// the run that closes the code span it opens, or -1. A backslash escapes
// the first backtick of a run outside code (it is literal, and the rest of
// the run may open a span), while inside a code span a backslash is
// literal, so an escaped run still closes one.
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
// result for each search, and the positions mostly grow, so the searches
// for the end of unclosed comments, quotes or titles do not rescan the
// text.
type finder struct {
	s    string
	memo map[string][2]int // the position searched from, the result
}

// remember returns the remembered result of the search key from from, if
// it still holds.
func (f *finder) remember(key string, from int) (int, bool) {
	m, ok := f.memo[key]
	if ok && m[0] <= from && (m[1] < 0 || from <= m[1]) {
		return m[1], true
	}
	return 0, false
}

func (f *finder) store(key string, from, result int) int {
	if f.memo == nil {
		f.memo = map[string][2]int{}
	}
	f.memo[key] = [2]int{from, result}
	return result
}

// index returns the position of sub in s at or after from, or -1.
func (f *finder) index(from int, sub string) int {
	if i, ok := f.remember(sub, from); ok {
		return i
	}
	i := strings.Index(f.s[from:], sub)
	if i >= 0 {
		i += from
	}
	return f.store(sub, from, i)
}

// indexAny returns the position of a byte of chars in s at or after from,
// or -1.
func (f *finder) indexAny(from int, chars string) int {
	key := "any:" + chars
	if i, ok := f.remember(key, from); ok {
		return i
	}
	i := strings.IndexAny(f.s[from:], chars)
	if i >= 0 {
		i += from
	}
	return f.store(key, from, i)
}

// unescaped returns the position of a byte of chars in s at or after from
// that no backslash escapes, or -1.
func (f *finder) unescaped(from int, chars string) int {
	key := "unescaped:" + chars
	if i, ok := f.remember(key, from); ok {
		return i
	}
	result := -1
	for i := from; i < len(f.s); {
		j := strings.IndexAny(f.s[i:], chars)
		if j < 0 {
			break
		}
		j += i
		backslashes := 0
		for k := j - 1; k >= 0 && f.s[k] == '\\'; k-- {
			backslashes++
		}
		if backslashes%2 == 0 {
			result = j
			break
		}
		i = j + 1
	}
	return f.store(key, from, result)
}

// after returns the position after sub in s at or after from, or -1.
func (f *finder) after(from int, sub string) int {
	if i := f.index(from, sub); i >= 0 {
		return i + len(sub)
	}
	return -1
}

// rawHTML returns the end of the autolink or raw HTML starting at s[i]
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
		for j < len(s) && s[j] > ' ' && !strings.ContainsRune("\"'=<>`", rune(s[j])) {
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

// isJSSpace reports whether r is whitespace for JavaScript's \s, which
// commonmark.js uses between the parts of HTML tags.
func isJSSpace(r rune) bool {
	switch {
	case r == ' ', r >= '\t' && r <= '\r', r == 0xA0, r == 0x1680, r >= 0x2000 && r <= 0x200A,
		r == 0x2028, r == 0x2029, r == 0x202F, r == 0x205F, r == 0x3000, r == 0xFEFF:
		return true
	}
	return false
}

// isJSSpaceAt reports whether s has \s whitespace at i.
func isJSSpaceAt(s string, i int) bool {
	if i >= len(s) {
		return false
	}
	r, _ := utf8.DecodeRuneInString(s[i:])
	return isJSSpace(r)
}

// skipSpace returns the position after the \s whitespace at s[i:].
func skipSpace(s string, i int) int {
	for i < len(s) {
		r, w := utf8.DecodeRuneInString(s[i:])
		if !isJSSpace(r) {
			break
		}
		i += w
	}
	return i
}

// trimJS trims \s whitespace, as JavaScript's String.prototype.trim does.
func trimJS(s string) string {
	return strings.TrimFunc(s, isJSSpace)
}

// isWSChar reports whether c is an ASCII whitespace character.
func isWSChar(c byte) bool { return c == ' ' || c >= '\t' && c <= '\r' }

// isASCIILetter reports whether c is an ASCII letter.
func isASCIILetter(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// isAttrNameStart reports whether an HTML attribute name may start with c.
func isAttrNameStart(c byte) bool {
	return isASCIILetter(c) || c == '_' || c == ':'
}

// isDigit reports whether c is an ASCII digit.
func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// isASCIIPunct reports whether c is ASCII punctuation, which a backslash
// escapes.
func isASCIIPunct(c byte) bool {
	return c >= '!' && c <= '/' || c >= ':' && c <= '@' || c >= '[' && c <= '`' || c >= '{' && c <= '~'
}
