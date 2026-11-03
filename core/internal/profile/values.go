package profile

import (
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Value limits. Long text matches the database constraint
// (core/migrations/00006_profile_fields.sql).
const (
	maxTextLength     = 200
	maxLongTextLength = 2000
	maxURLLength      = 2048
	minYear           = 1900
	maxYear           = 2100
)

var (
	phonePattern    = regexp.MustCompile(`^\+[1-9][0-9]{6,14}$`)
	telegramPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{4,31}$`)
	githubPattern   = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?$`)
	gitlabPattern   = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,254}$`)
	xPattern        = regexp.MustCompile(`^[A-Za-z0-9_]{1,15}$`)
	mastodonPattern = regexp.MustCompile(`^[A-Za-z0-9_]{1,30}@[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*\.[A-Za-z]{2,63}$`)
)

// normalize validates a value for the field and returns its stored form.
// The empty result means "no value" (clear it). Errors wrap ErrInvalid and
// name the field key.
//
//   - text: one line, at most 200 characters;
//   - long_text: at most 2000 characters, line breaks allowed;
//   - date: YYYY-MM-DD between the years 1900 and 2100;
//   - select: the key of one of the field's options;
//   - phone: international format, stored as E.164 (+ and 7-15 digits;
//     spaces, dashes, dots and parentheses are dropped, a leading 00
//     becomes +);
//   - url: an absolute http or https URL without credentials;
//   - handle: the account name on the handle's service, without a leading
//     @ (Mastodon: user@instance).
func (f Field) normalize(raw string) (string, error) {
	bad := func(format string, args ...any) error { return invalid(f.Key, format, args...) }
	if !utf8.ValidString(raw) || strings.ContainsRune(raw, 0) {
		return "", bad("must be valid text")
	}
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", nil
	}
	switch f.Type {
	case TypeText:
		if utf8.RuneCountInString(v) > maxTextLength || hasControl(v, false) {
			return "", bad("must be one line of at most %d characters", maxTextLength)
		}
	case TypeLongText:
		v = strings.ReplaceAll(v, "\r\n", "\n")
		if utf8.RuneCountInString(v) > maxLongTextLength || hasControl(v, true) {
			return "", bad("must be at most %d characters", maxLongTextLength)
		}
	case TypeDate:
		d, err := time.Parse(time.DateOnly, v)
		if err != nil || d.Year() < minYear || d.Year() > maxYear {
			return "", bad("must be a date as YYYY-MM-DD between %d and %d", minYear, maxYear)
		}
	case TypeSelect:
		if !slices.ContainsFunc(f.Options, func(o Option) bool { return o.Key == v }) {
			return "", bad("must be one of the field's options")
		}
	case TypePhone:
		v = strings.Map(func(r rune) rune {
			if strings.ContainsRune(" -.() ", r) {
				return -1
			}
			return r
		}, v)
		if strings.HasPrefix(v, "00") {
			v = "+" + v[2:]
		}
		if !phonePattern.MatchString(v) {
			return "", bad("must be a phone number in international format, such as +44 20 7946 0958")
		}
	case TypeURL:
		u, err := url.Parse(v)
		if err != nil || len(v) > maxURLLength || hasControl(v, false) || strings.ContainsAny(v, " \t") ||
			(u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.Opaque != "" {
			return "", bad("must be an http or https address, such as https://example.com")
		}
	case TypeHandle:
		return f.normalizeHandle(v)
	default:
		return "", bad("has an unknown type %q", f.Type)
	}
	return v, nil
}

func (f Field) normalizeHandle(v string) (string, error) {
	v = strings.TrimPrefix(v, "@")
	var ok bool
	switch f.HandleKind {
	case HandleTelegram:
		ok = telegramPattern.MatchString(v)
	case HandleGitHub:
		ok = githubPattern.MatchString(v) && !strings.Contains(v, "--")
	case HandleGitLab:
		ok = gitlabPattern.MatchString(v)
	case HandleX:
		ok = xPattern.MatchString(v)
	case HandleMastodon:
		if ok = mastodonPattern.MatchString(v); ok {
			user, instance, _ := strings.Cut(v, "@")
			v = user + "@" + strings.ToLower(instance) // the instance is a host name
		}
	}
	if !ok {
		return "", invalid(f.Key, "must be a valid %s handle", f.HandleKind)
	}
	return v, nil
}

// hasControl reports control characters; newline and tab are allowed in
// multi-line text.
func hasControl(s string, multiline bool) bool {
	return strings.ContainsFunc(s, func(r rune) bool {
		if multiline && (r == '\n' || r == '\t') {
			return false
		}
		return unicode.IsControl(r) || r == ' ' || r == ' '
	})
}

// maskYear turns a stored date (YYYY-MM-DD) into day and month only
// (--MM-DD, ISO 8601 without a year).
func maskYear(date string) string {
	if len(date) == len(time.DateOnly) {
		return "--" + date[5:]
	}
	return date
}
