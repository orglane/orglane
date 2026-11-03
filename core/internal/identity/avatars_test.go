package identity_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/orglane/orglane/core/internal/identity"
	"github.com/orglane/orglane/demo"
)

const safeSVG = `<?xml version="1.0" encoding="UTF-8"?>
<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 128 128" width="128" height="128" role="img" aria-label="MO">
  <title>Maya Okafor</title>
  <defs>
    <linearGradient id="bg" x1="0" y1="0" x2="1" y2="1">
      <stop offset="0" stop-color="#2563eb"/>
      <stop offset="1" stop-color="#7c3aed"/>
    </linearGradient>
  </defs>
  <!-- initials -->
  <rect width="128" height="128" rx="64" fill="url(#bg)"/>
  <text x="64" y="64" dy="0.35em" text-anchor="middle" font-family="sans-serif" font-size="52" fill="#fff">MO</text>
</svg>`

func TestValidateAvatarAcceptsSafeImages(t *testing.T) {
	for name, tc := range map[string]struct {
		contentType string
		data        string
	}{
		"svg":  {identity.AvatarSVG, safeSVG},
		"png":  {identity.AvatarPNG, "\x89PNG\r\n\x1a\nrest"},
		"webp": {identity.AvatarWebP, "RIFF\x00\x00\x00\x00WEBPVP8 "},
	} {
		t.Run(name, func(t *testing.T) {
			if err := identity.ValidateAvatar(tc.contentType, []byte(tc.data)); err != nil {
				t.Fatalf("ValidateAvatar: %v", err)
			}
		})
	}
}

func TestValidateAvatarRejectsUnsafeSVG(t *testing.T) {
	const open = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10">`
	for name, svg := range map[string]string{
		"script":            open + `<script>alert(1)</script></svg>`,
		"namespaced script": `<svg xmlns="http://www.w3.org/2000/svg" xmlns:s="http://www.w3.org/2000/svg">` + `<s:script>alert(1)</s:script></svg>`,
		"event handler":     `<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"></svg>`,
		"foreignObject":     open + `<foreignObject><div xmlns="http://www.w3.org/1999/xhtml">x</div></foreignObject></svg>`,
		"image":             open + `<image href="https://evil.example/x.png"/></svg>`,
		"use":               open + `<use href="#a"/></svg>`,
		"anchor":            open + `<a href="javascript:alert(1)"><rect width="1" height="1"/></a></svg>`,
		"xlink href":        `<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink"><rect xlink:href="#x"/></svg>`,
		"style attribute":   open + `<rect style="fill:url(https://evil.example/)"/></svg>`,
		"style element":     open + `<style>@import url(https://evil.example/);</style></svg>`,
		"external url":      open + `<rect fill="url(https://evil.example/#g)"/></svg>`,
		"data url":          open + `<rect fill="data:text/html,x"/></svg>`,
		"doctype":           `<!DOCTYPE svg [<!ENTITY x "y">]>` + open + `</svg>`,
		"entity":            open + `<text>&x;</text></svg>`,
		"processing instr":  `<?xml-stylesheet href="https://evil.example/a.css"?>` + open + `</svg>`,
		"wrong namespace":   `<svg xmlns="http://www.w3.org/1999/xhtml"></svg>`,
		"no namespace":      `<svg viewBox="0 0 1 1"></svg>`,
		"child namespace":   open + `<g xmlns="http://www.w3.org/1999/xhtml"></g></svg>`,
		"html root":         `<html xmlns="http://www.w3.org/1999/xhtml"><script>alert(1)</script></html>`,
		"two roots":         open + `</svg>` + open + `</svg>`,
		"trailing text":     open + `</svg>alert(1)`,
		"not xml":           `<svg xmlns="http://www.w3.org/2000/svg"><rect></svg>`,
		"animate":           open + `<animate attributeName="href" to="javascript:alert(1)"/></svg>`,
		"set":               open + `<set attributeName="onmouseover" to="alert(1)"/></svg>`,
		// CSS decodes escapes before recognizing functions: these are all
		// url(https://evil.example/...) to a browser.
		"escaped u":             open + `<rect fill="\75rl(https://evil.example/a.svg#g)"/></svg>`,
		"escaped r":             open + `<rect fill="u\72l(https://evil.example/a.svg#g)"/></svg>`,
		"escaped l":             open + `<rect stroke="ur\6c(https://evil.example/a.svg#g)"/></svg>`,
		"escaped with space":    open + `<rect fill="\000075 rl(https://evil.example/a.svg#g)"/></svg>`,
		"escaped paren":         open + `<rect fill="url\28https://evil.example/a.svg#g)"/></svg>`,
		"escaped stop-color":    open + `<stop stop-color="\75rl(https://evil.example/#g)"/></svg>`,
		"escaped font-family":   open + `<text font-family="\75rl(https://evil.example/f.woff)">A</text></svg>`,
		"upper-case url":        open + `<rect fill="URL(https://evil.example/a.svg#g)"/></svg>`,
		"url with quotes":       open + `<rect fill="url('https://evil.example/a.svg#g')"/></svg>`,
		"local ref then more":   open + `<rect fill="url(#bg) url(https://evil.example/#g)"/></svg>`,
		"image-set":             open + `<rect fill="image-set('https://evil.example/a.png' 1x)"/></svg>`,
		"var()":                 open + `<rect fill="var(--x)"/></svg>`,
		"color function":        open + `<rect fill="rgb(1,2,3)"/></svg>`,
		"font-family quotes":    open + `<text font-family="'a';background:url(https://evil.example/)">A</text></svg>`,
		"length with function":  open + `<rect width="calc(1px)"/></svg>`,
		"transform with escape": open + `<g transform="\75rl(https://evil.example/)"></g></svg>`,
		"id with escape":        open + `<linearGradient id="a\62"></linearGradient></svg>`,
		"aria-label escape":     `<svg xmlns="http://www.w3.org/2000/svg" aria-label="\75rl(x)"></svg>`,
	} {
		t.Run(name, func(t *testing.T) {
			err := identity.ValidateAvatar(identity.AvatarSVG, []byte(svg))
			if !errors.Is(err, identity.ErrUnsafeAvatar) {
				t.Fatalf("ValidateAvatar = %v, want ErrUnsafeAvatar", err)
			}
		})
	}
}

func TestValidateAvatarRejectsWrongTypeAndSize(t *testing.T) {
	for name, tc := range map[string]struct {
		contentType string
		data        []byte
	}{
		"empty":           {identity.AvatarPNG, nil},
		"too large":       {identity.AvatarPNG, append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, identity.MaxAvatarBytes)...)},
		"not png":         {identity.AvatarPNG, []byte(safeSVG)},
		"not webp":        {identity.AvatarWebP, []byte("RIFF....AVI ")},
		"html":            {"text/html", []byte("<p>hi</p>")},
		"svg as png":      {identity.AvatarPNG, []byte(safeSVG)},
		"unknown type":    {"image/gif", []byte("GIF89a")},
		"type parameters": {"image/svg+xml; charset=utf-8", []byte(safeSVG)},
		"huge svg":        {identity.AvatarSVG, []byte(strings.Replace(safeSVG, "<title>", "<title>"+strings.Repeat("a", identity.MaxAvatarBytes), 1))},
	} {
		t.Run(name, func(t *testing.T) {
			if err := identity.ValidateAvatar(tc.contentType, tc.data); !errors.Is(err, identity.ErrUnsafeAvatar) {
				t.Fatalf("ValidateAvatar = %v, want ErrUnsafeAvatar", err)
			}
		})
	}
}

// TestDemoAvatarsAreValid: the generated initials avatars of the demo
// dataset stay inside the allowed subset.
func TestDemoAvatarsAreValid(t *testing.T) {
	d, err := demo.Load(demo.Acme)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range d.People {
		data, contentType, _, err := d.Avatar(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := identity.ValidateAvatar(contentType, data); err != nil {
			t.Errorf("%s: %v", p.Username, err)
		}
	}
	for _, name := range []string{"Liam O'Connor", "Mateo Rodríguez", "Elif Yılmaz", "Anna-Lena Groß"} {
		if err := identity.ValidateAvatar(identity.AvatarSVG, demo.InitialsAvatar(name, name)); err != nil {
			t.Errorf("InitialsAvatar(%q): %v", name, err)
		}
	}
}
