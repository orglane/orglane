package mail

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	htmltemplate "html/template"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strings"
	texttemplate "text/template"

	"golang.org/x/text/language"
)

// Templates and catalogs.
//
// Every email has a plain-text and an HTML template (templates/<kind>.txt,
// templates/<kind>.gohtml inside templates/layout.gohtml). The HTML is simple
// and accessible: a language attribute, a real heading, readable contrast,
// descriptive link text and the link also written out. It loads nothing
// from anywhere: no images, fonts or tracking pixels.
//
// Strings come from locales/<tag>.json, with {name} placeholders. English
// is complete; other catalogs may hold a subset, and missing strings fall
// back to English. Counted strings have ".one" and ".other" variants.

//go:embed locales/*.json templates/*
var files embed.FS

var (
	catalogs   = map[string]map[string]string{}
	supported  []language.Tag
	matcher    language.Matcher
	htmlKinds  = map[string]*htmltemplate.Template{}
	textKinds  = map[string]*texttemplate.Template{}
	placeholds = regexp.MustCompile(`\{([a-zA-Z]+)\}`)
)

// Message kinds.
const (
	KindInvitation = "invitation"
	KindTest       = "test"
)

// fallbackLocale is the complete catalog.
const fallbackLocale = "en"

func init() {
	if err := loadCatalogs(); err != nil {
		panic(err)
	}
	if err := loadTemplates(); err != nil {
		panic(err)
	}
}

func loadCatalogs() error {
	entries, err := fs.Glob(files, "locales/*.json")
	if err != nil {
		return err
	}
	slices.Sort(entries)
	tags := []language.Tag{language.MustParse(fallbackLocale)}
	for _, name := range entries {
		raw, err := files.ReadFile(name)
		if err != nil {
			return err
		}
		var cat map[string]string
		if err := json.Unmarshal(raw, &cat); err != nil {
			return fmt.Errorf("mail: %s: %w", name, err)
		}
		tag := strings.TrimSuffix(path.Base(name), ".json")
		catalogs[tag] = cat
		if tag != fallbackLocale {
			tags = append(tags, language.MustParse(tag))
		}
	}
	if catalogs[fallbackLocale] == nil {
		return fmt.Errorf("mail: the %s catalog is missing", fallbackLocale)
	}
	supported = tags
	matcher = language.NewMatcher(tags)
	return nil
}

func loadTemplates() error {
	layout, err := files.ReadFile("templates/layout.gohtml")
	if err != nil {
		return err
	}
	stub := map[string]any{"t": func(string, ...any) string { return "" }, "tn": func(string, int, ...any) string { return "" }}
	for _, kind := range []string{KindInvitation, KindTest} {
		h, err := htmltemplate.New(kind).Funcs(stub).Parse(string(layout))
		if err != nil {
			return err
		}
		if h, err = h.ParseFS(files, "templates/"+kind+".gohtml"); err != nil {
			return err
		}
		htmlKinds[kind] = h
		t, err := texttemplate.New(kind+".txt").Funcs(stub).ParseFS(files, "templates/"+kind+".txt")
		if err != nil {
			return err
		}
		textKinds[kind] = t
	}
	return nil
}

// Locales returns the locales emails can be written in.
func Locales() []string {
	out := make([]string, 0, len(supported))
	for _, t := range supported {
		out = append(out, t.String())
	}
	return out
}

// Negotiate picks the supported locale that best matches an
// Accept-Language header (or a single tag); English when nothing matches.
func Negotiate(acceptLanguage string) string {
	tags, _, err := language.ParseAcceptLanguage(acceptLanguage)
	if err != nil || len(tags) == 0 {
		return fallbackLocale
	}
	_, i, conf := matcher.Match(tags...)
	if conf == language.No {
		return fallbackLocale
	}
	return supported[i].String()
}

// translate returns the string for key in locale (falling back to English)
// with placeholders replaced from pairs ("name", value, ...).
func translate(locale, key string, pairs ...any) string {
	msg, ok := catalogs[locale][key]
	if !ok {
		msg, ok = catalogs[fallbackLocale][key]
	}
	if !ok {
		panic(fmt.Sprintf("mail: no message %q", key))
	}
	values := map[string]string{}
	for i := 0; i+1 < len(pairs); i += 2 {
		values[fmt.Sprint(pairs[i])] = fmt.Sprint(pairs[i+1])
	}
	return placeholds.ReplaceAllStringFunc(msg, func(m string) string {
		if v, ok := values[m[1:len(m)-1]]; ok {
			return v
		}
		return m
	})
}

// translateCount picks the ".one" or ".other" variant of key for n.
func translateCount(locale, key string, n int, pairs ...any) string {
	if n == 1 {
		return translate(locale, key+".one", pairs...)
	}
	return translate(locale, key+".other", pairs...)
}

// Invitation is the content of an invitation email.
type Invitation struct {
	// Name is the invited person's display name.
	Name string
	// Inviter is the display name of who invited them.
	Inviter string
	// Workspace is the workspace host, e.g. work.acme.com.
	Workspace string
	// WorkEmail is the person's work email.
	WorkEmail string
	// Link is the invitation link.
	Link string
	// Days is how long the link stays valid.
	Days int
}

// Test is the content of a test email.
type Test struct {
	Workspace string
	// Sender is the display name of who sent the test.
	Sender string
}

// RenderInvitation writes an invitation email in locale.
func RenderInvitation(locale string, d Invitation) (Message, error) {
	return render(KindInvitation, locale, d, translate(locale, "invitation.subject", "inviter", d.Inviter, "workspace", d.Workspace),
		translate(locale, "invitation.preheader"), d.Workspace)
}

// RenderTest writes a test email in locale.
func RenderTest(locale string, d Test) (Message, error) {
	return render(KindTest, locale, d, translate(locale, "test.subject", "workspace", d.Workspace),
		translate(locale, "test.preheader"), d.Workspace)
}

func render(kind, locale string, data any, subject, preheader, workspace string) (Message, error) {
	locale = Negotiate(locale)
	funcs := map[string]any{
		"t":  func(key string, pairs ...any) string { return translate(locale, key, pairs...) },
		"tn": func(key string, n int, pairs ...any) string { return translateCount(locale, key, n, pairs...) },
	}
	dir := "ltr"
	if tag, err := language.Parse(locale); err == nil {
		if base, _ := tag.Base(); slices.Contains([]string{"ar", "fa", "he", "ur"}, base.String()) {
			dir = "rtl"
		}
	}
	var html, text bytes.Buffer
	h, err := htmlKinds[kind].Clone()
	if err != nil {
		return Message{}, err
	}
	err = h.Funcs(funcs).ExecuteTemplate(&html, "layout", htmlData{
		Lang: locale, Dir: dir, Subject: subject, Preheader: preheader, Workspace: workspace, D: data,
	})
	if err != nil {
		return Message{}, fmt.Errorf("mail: render %s (html): %w", kind, err)
	}
	t, err := textKinds[kind].Clone()
	if err != nil {
		return Message{}, err
	}
	if err := t.Funcs(funcs).Execute(&text, data); err != nil {
		return Message{}, fmt.Errorf("mail: render %s (text): %w", kind, err)
	}
	return Message{Subject: subject, Text: text.String(), HTML: html.String()}, nil
}

// htmlData is what the HTML layout sees; D is the kind's data.
type htmlData struct {
	Lang, Dir, Subject, Preheader, Workspace string
	D                                        any
}
