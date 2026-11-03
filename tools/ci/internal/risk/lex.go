package risk

import (
	"go/scanner"
	"go/token"
	"path"
	"strings"
)

// lexedLine is the executable code of one source line.
type lexedLine struct {
	// Code is the line's executable code: comments and literals that span
	// several lines are dropped; single-line literals are kept verbatim.
	Code string
	// Masked is Code with the contents of every literal replaced by a
	// placeholder, so text inside strings never looks like an assertion.
	Masked string
}

// lexFile returns the executable code of every line of a Go, TypeScript or
// JavaScript file (index 0 is line 1). ok is false when the file cannot be
// lexed; callers must then treat its lines conservatively.
func lexFile(name string, src []byte) (lines []lexedLine, ok bool) {
	if path.Ext(name) == ".go" {
		return lexGo(src)
	}
	return lexJS(src)
}

// lineBuilder collects code fragments per line.
type lineBuilder struct {
	code, masked []strings.Builder
}

func newLineBuilder(src []byte) *lineBuilder {
	n := strings.Count(string(src), "\n") + 1
	return &lineBuilder{code: make([]strings.Builder, n), masked: make([]strings.Builder, n)}
}

// add appends a fragment to line (1-based). Two word-like fragments are
// separated by a space, so "if t.Fatal" never becomes "ift.Fatal".
func (b *lineBuilder) add(line int, code, masked string) {
	if line < 1 || line > len(b.code) {
		return
	}
	appendFragment(&b.code[line-1], code)
	appendFragment(&b.masked[line-1], masked)
}

func appendFragment(sb *strings.Builder, s string) {
	if s == "" {
		return
	}
	if cur := sb.String(); cur != "" && isWordByte(cur[len(cur)-1]) && isWordByte(s[0]) {
		sb.WriteByte(' ')
	}
	sb.WriteString(s)
}

func (b *lineBuilder) lines() []lexedLine {
	out := make([]lexedLine, len(b.code))
	for i := range b.code {
		out[i] = lexedLine{Code: b.code[i].String(), Masked: b.masked[i].String()}
	}
	return out
}

func isWordByte(c byte) bool {
	return c == '_' || c == '$' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

// lexGo tokenizes Go source with go/scanner.
func lexGo(src []byte) ([]lexedLine, bool) {
	fset := token.NewFileSet()
	file := fset.AddFile("", fset.Base(), len(src))
	failed := false
	var s scanner.Scanner
	s.Init(file, src, func(token.Position, string) { failed = true }, 0) // comments are skipped
	b := newLineBuilder(src)
	for {
		pos, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		}
		if tok == token.SEMICOLON && lit == "\n" {
			continue // automatically inserted
		}
		text := lit
		if text == "" {
			text = tok.String()
		}
		line := file.Line(pos)
		switch tok {
		case token.STRING, token.CHAR:
			if strings.Contains(text, "\n") {
				continue // a raw string over several lines is data, not code
			}
			b.add(line, text, `""`)
		default:
			b.add(line, text, text)
		}
	}
	if failed {
		return nil, false
	}
	return b.lines(), true
}

// lexJS is a small TypeScript/JavaScript tokenizer. It tracks line and block
// comments, quoted strings, template literals (their text is data, their
// ${...} substitutions are code, nested to any depth) and regular expression
// literals. It is deliberately not a parser: whenever it cannot tell what the
// code means (a "/" after "}", an unterminated literal or comment, unbalanced
// brackets, JSX-like text), it reports ok=false and the caller treats the
// file conservatively.
func lexJS(src []byte) ([]lexedLine, bool) {
	b := newLineBuilder(src)
	lx := jsLexer{src: src, b: b, line: 1}
	if !lx.run() {
		return nil, false
	}
	return b.lines(), true
}

type jsLexer struct {
	src  []byte
	b    *lineBuilder
	line int
	// prev classifies the last significant token, to tell a regex literal
	// from a division: 0 (start), 'w' (identifier or number), 'k' (keyword
	// before an expression), 'v' (literal, closing bracket or postfix ++/--:
	// a value), 'p' (a ")" closing if/while/for/with), 'b' (a "}"), or the
	// operator byte.
	prev byte
	// frames tracks template literals: a template frame is inside the
	// template's text, a substitution frame inside ${...} with its own brace
	// depth.
	frames   []jsFrame
	parens   []bool // per open "(": whether it follows if/while/for/with
	braces   int    // open "{" outside substitutions
	brackets int
	lastWord string
}

type jsFrame struct {
	template bool
	depth    int
}

func (lx *jsLexer) add(code, masked string) { lx.b.add(lx.line, code, masked) }

func (lx *jsLexer) inTemplateText() bool {
	return len(lx.frames) > 0 && lx.frames[len(lx.frames)-1].template
}

func (lx *jsLexer) run() bool {
	src := lx.src
	for i := 0; i < len(src); {
		if lx.inTemplateText() {
			j := i
			for j < len(src) && src[j] != '`' && (src[j] != '$' || j+1 >= len(src) || src[j+1] != '{') {
				if src[j] == '\\' {
					j++
				}
				j++
			}
			if j >= len(src) {
				return false // unterminated template literal
			}
			if part := string(src[i:j]); strings.Contains(part, "\n") {
				lx.line += strings.Count(part, "\n") // text over several lines: dropped
			} else if part != "" {
				lx.add(part, `""`)
			}
			if src[j] == '`' {
				lx.add("`", "`")
				lx.frames = lx.frames[:len(lx.frames)-1]
				lx.prev, i = 'v', j+1
			} else {
				lx.add("${", "${")
				lx.frames = append(lx.frames, jsFrame{})
				lx.prev, i = '(', j+2
			}
			continue
		}

		c := src[i]
		switch {
		case c == '\n':
			lx.line++
			i++
		case c == ' ' || c == '\t' || c == '\r':
			i++
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			end := strings.Index(string(src[i+2:]), "*/")
			if end < 0 {
				return false // unterminated block comment
			}
			lx.line += strings.Count(string(src[i:i+2+end]), "\n")
			i += 2 + end + 2
		case c == '"' || c == '\'':
			j := i + 1
			for j < len(src) && src[j] != c {
				if src[j] == '\n' {
					return false // unterminated string (or JSX text such as "don't")
				}
				if src[j] == '\\' {
					j++
				}
				j++
			}
			if j >= len(src) {
				return false
			}
			lx.add(string(src[i:j+1]), `""`)
			lx.prev, i = 'v', j+1
		case c == '`':
			lx.add("`", "`")
			lx.frames = append(lx.frames, jsFrame{template: true})
			i++
		case c == '/':
			regex, ok := lx.regexAllowed()
			if !ok {
				return false
			}
			if !regex {
				lx.add("/", "/")
				lx.prev = '/'
				i++
				continue
			}
			j, ok := scanRegex(src, i)
			if !ok {
				return false
			}
			lx.add(string(src[i:j]), "//")
			lx.prev, i = 'v', j
		case isWordByte(c):
			j := i
			for j < len(src) && isWordByte(src[j]) {
				j++
			}
			w := string(src[i:j])
			lx.add(w, w)
			lx.lastWord = w
			lx.prev = 'w'
			if isKeywordBeforeExpression(w) {
				lx.prev = 'k'
			}
			i = j
			continue
		case (c == '+' || c == '-') && i+1 < len(src) && src[i+1] == c:
			// ++ and -- : a "/" after them is always a division (a regex
			// cannot follow a prefix ++, and x++ is a value).
			lx.add(string(src[i:i+2]), string(src[i:i+2]))
			lx.prev, i = 'v', i+2
			continue
		case c == '(':
			lx.parens = append(lx.parens, lx.prev == 'w' && isControlKeyword(lx.lastWord))
			lx.add("(", "(")
			lx.prev, i = '(', i+1
		case c == ')':
			if len(lx.parens) == 0 {
				return false
			}
			control := lx.parens[len(lx.parens)-1]
			lx.parens = lx.parens[:len(lx.parens)-1]
			lx.add(")", ")")
			lx.prev, i = 'v', i+1
			if control {
				lx.prev = 'p'
			}
		case c == '[':
			lx.brackets++
			lx.add("[", "[")
			lx.prev, i = '[', i+1
		case c == ']':
			if lx.brackets--; lx.brackets < 0 {
				return false
			}
			lx.add("]", "]")
			lx.prev, i = 'v', i+1
		case c == '{':
			if n := len(lx.frames); n > 0 {
				lx.frames[n-1].depth++
			} else {
				lx.braces++
			}
			lx.add("{", "{")
			lx.prev, i = '{', i+1
		case c == '}':
			if n := len(lx.frames); n > 0 {
				if lx.frames[n-1].depth == 0 {
					lx.add("}", "}") // end of a ${...} substitution
					lx.frames = lx.frames[:n-1]
					i++
					continue
				}
				lx.frames[n-1].depth--
			} else if lx.braces--; lx.braces < 0 {
				return false
			}
			lx.add("}", "}")
			lx.prev, i = 'b', i+1
		default:
			lx.add(string(c), string(c))
			lx.prev, i = c, i+1
		}
		if lx.prev != 'w' && lx.prev != 'k' {
			lx.lastWord = ""
		}
	}
	return len(lx.frames) == 0 && len(lx.parens) == 0 && lx.braces == 0 && lx.brackets == 0
}

// regexAllowed reports whether a "/" here starts a regular expression
// literal. ok is false when that cannot be decided without parsing: after a
// "}" it may end a block (regex) or an object literal (division).
func (lx *jsLexer) regexAllowed() (regex, ok bool) {
	switch lx.prev {
	case 'w', 'v':
		return false, true
	case 'b':
		return false, false
	}
	return true, true // start, keyword, operator, "(", "[", "{", ")" of if/while/for/with
}

// scanRegex returns the end of the regular expression literal at src[i].
func scanRegex(src []byte, i int) (int, bool) {
	j := i + 1
	inClass := false
	for j < len(src) {
		switch c := src[j]; {
		case c == '\n':
			return 0, false
		case c == '\\':
			j += 2
			continue
		case c == '[':
			inClass = true
		case c == ']':
			inClass = false
		case c == '/' && !inClass:
			j++
			for j < len(src) && isWordByte(src[j]) { // flags
				j++
			}
			return j, true
		}
		j++
	}
	return 0, false
}

func isControlKeyword(w string) bool {
	return w == "if" || w == "while" || w == "for" || w == "with"
}

func isKeywordBeforeExpression(w string) bool {
	switch w {
	case "return", "typeof", "instanceof", "in", "of", "new", "delete", "void", "throw", "case", "do", "else", "yield", "await":
		return true
	}
	return false
}
