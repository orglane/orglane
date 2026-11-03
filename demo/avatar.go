package demo

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"hash/fnv"
	"strings"
	"unicode"
)

// avatarPalettes are gradient color pairs for initials avatars.
var avatarPalettes = [...][2]string{
	{"#2563eb", "#7c3aed"},
	{"#0891b2", "#2563eb"},
	{"#059669", "#0d9488"},
	{"#d97706", "#dc2626"},
	{"#db2777", "#9333ea"},
	{"#4f46e5", "#0ea5e9"},
	{"#65a30d", "#059669"},
	{"#ea580c", "#db2777"},
}

// InitialsAvatar returns a 128x128 SVG avatar with the initials of name on a
// gradient picked from seed (the username), so the same person always gets
// the same colors. The SVG uses only the subset of SVG allowed for avatars
// (see identity.ValidateAvatar): no scripts, styles, links or external
// references.
func InitialsAvatar(name, seed string) []byte {
	h := fnv.New32a()
	_, _ = h.Write([]byte(seed))
	colors := avatarPalettes[h.Sum32()%uint32(len(avatarPalettes))]

	var b bytes.Buffer
	esc := func(s string) string {
		var e bytes.Buffer
		_ = xml.EscapeText(&e, []byte(s))
		return e.String()
	}
	initials := Initials(name)
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 128 128" width="128" height="128" role="img" aria-label="%s">`+"\n", esc(name))
	fmt.Fprintf(&b, "  <title>%s</title>\n", esc(name))
	b.WriteString("  <defs>\n")
	b.WriteString(`    <linearGradient id="bg" x1="0" y1="0" x2="1" y2="1">` + "\n")
	fmt.Fprintf(&b, `      <stop offset="0" stop-color="%s"/>`+"\n", colors[0])
	fmt.Fprintf(&b, `      <stop offset="1" stop-color="%s"/>`+"\n", colors[1])
	b.WriteString("    </linearGradient>\n")
	b.WriteString("  </defs>\n")
	b.WriteString(`  <rect width="128" height="128" rx="64" fill="url(#bg)"/>` + "\n")
	fmt.Fprintf(&b, `  <text x="64" y="64" dy="0.35em" text-anchor="middle" font-family="Inter, system-ui, sans-serif" font-size="48" font-weight="600" fill="#ffffff">%s</text>`+"\n", esc(initials))
	b.WriteString("</svg>\n")
	return b.Bytes()
}

// Initials returns the upper-case initials of the first and last word of
// name, e.g. "MR" for "Mateo Rodríguez" and "LO" for "Liam O'Connor".
func Initials(name string) string {
	words := strings.Fields(name)
	if len(words) == 0 {
		return "?"
	}
	first := func(w string) string {
		for _, r := range w {
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				return string(unicode.ToUpper(r))
			}
		}
		return ""
	}
	out := first(words[0])
	if len(words) > 1 {
		out += first(words[len(words)-1])
	}
	if out == "" {
		return "?"
	}
	return out
}
