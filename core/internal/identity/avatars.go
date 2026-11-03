package identity

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/store"
)

// Avatar content types (the allow-list of person_avatars.content_type).
const (
	AvatarSVG  = "image/svg+xml"
	AvatarPNG  = "image/png"
	AvatarWebP = "image/webp"
)

// MaxAvatarBytes bounds an avatar image (person_avatars.data).
const MaxAvatarBytes = 256 * 1024

// ErrUnsafeAvatar is returned for avatar images that are not of the declared
// type or, for SVG, use anything outside the safe subset (see
// ValidateAvatar).
var ErrUnsafeAvatar = errors.New("identity: avatar image is not allowed")

// Avatar is a person's avatar image.
type Avatar struct {
	ContentType string
	Data        []byte
}

// SetAvatar sets or replaces a person's avatar after validating it with
// ValidateAvatar.
func (s *Service) SetAvatar(ctx context.Context, actor audit.Actor, personID uuid.UUID, avatar Avatar) error {
	if err := ValidateAvatar(avatar.ContentType, avatar.Data); err != nil {
		return err
	}
	return s.inTx(ctx, func(q *store.Queries) error {
		if _, err := q.GetPersonForUpdate(ctx, personID); err != nil {
			return notFound(err, "person")
		}
		if err := q.PutPersonAvatar(ctx, store.PutPersonAvatarParams{PersonID: personID, ContentType: avatar.ContentType, Data: avatar.Data}); err != nil {
			return fmt.Errorf("identity: store avatar: %w", err)
		}
		_, err := audit.Record(ctx, q, audit.Entry{
			Actor:    actor,
			Action:   audit.PersonAvatarSet,
			Target:   personTarget(personID),
			Metadata: map[string]any{"content_type": avatar.ContentType, "bytes": len(avatar.Data)},
		})
		return err
	})
}

// RemoveAvatar removes a person's avatar. Removing a missing avatar is a
// no-op.
func (s *Service) RemoveAvatar(ctx context.Context, actor audit.Actor, personID uuid.UUID) error {
	return s.inTx(ctx, func(q *store.Queries) error {
		if _, err := q.GetPersonForUpdate(ctx, personID); err != nil {
			return notFound(err, "person")
		}
		n, err := q.DeletePersonAvatar(ctx, personID)
		if err != nil {
			return fmt.Errorf("identity: remove avatar: %w", err)
		}
		if n == 0 {
			return nil
		}
		_, err = audit.Record(ctx, q, audit.Entry{Actor: actor, Action: audit.PersonAvatarRemoved, Target: personTarget(personID)})
		return err
	})
}

// GetAvatar returns a person's avatar, or ErrNotFound.
func (s *Service) GetAvatar(ctx context.Context, personID uuid.UUID) (Avatar, error) {
	row, err := s.queries().GetPersonAvatar(ctx, personID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Avatar{}, fmt.Errorf("%w: avatar", ErrNotFound)
	} else if err != nil {
		return Avatar{}, fmt.Errorf("identity: load avatar: %w", err)
	}
	return Avatar{ContentType: row.ContentType, Data: row.Data}, nil
}

// ValidateAvatar checks an avatar image before it is stored. The content
// type must be on the allow-list and match the data; PNG and WebP are
// checked by signature. SVG must be a self-contained drawing in a small,
// safe subset: an allow-list of shape, text and gradient elements and of
// presentation attributes whose values must match fixed grammars (numbers,
// colors, keywords, font names, "url(#id)"); no scripts, event handlers,
// styles, links, foreignObject, external references, CSS escapes or
// DOCTYPE. Serving avatars additionally sets a restrictive
// Content-Security-Policy, so this is defense in depth.
func ValidateAvatar(contentType string, data []byte) error {
	if len(data) == 0 || len(data) > MaxAvatarBytes {
		return fmt.Errorf("%w: must be 1 to %d bytes", ErrUnsafeAvatar, MaxAvatarBytes)
	}
	switch contentType {
	case AvatarPNG:
		if !bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")) {
			return fmt.Errorf("%w: not a PNG image", ErrUnsafeAvatar)
		}
		return nil
	case AvatarWebP:
		if len(data) < 12 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
			return fmt.Errorf("%w: not a WebP image", ErrUnsafeAvatar)
		}
		return nil
	case AvatarSVG:
		if err := validateSVG(data); err != nil {
			return fmt.Errorf("%w: %w", ErrUnsafeAvatar, err)
		}
		return nil
	default:
		return fmt.Errorf("%w: content type %q (allowed: %s, %s, %s)", ErrUnsafeAvatar, contentType, AvatarSVG, AvatarPNG, AvatarWebP)
	}
}

const svgNamespace = "http://www.w3.org/2000/svg"

// svgElements are the SVG elements allowed in avatars.
var svgElements = []string{
	"svg", "title", "desc", "g", "defs",
	"rect", "circle", "ellipse", "line", "polyline", "polygon", "path",
	"text", "tspan",
	"linearGradient", "radialGradient", "stop",
}

// SVG attribute values are checked against small, fixed grammars rather
// than parsed as CSS: presentation attributes are CSS values, and CSS
// decodes escapes (u\72l( is url() before recognizing functions, so a
// deny-list of dangerous substrings can be bypassed. Every value grammar
// excludes backslashes, parentheses (except the exact url(#id) and
// transform forms), colons, quotes in color values and anything else a
// reference or escape needs.
var (
	svgNumber    = `[+-]?(?:[0-9]+\.?[0-9]*|\.[0-9]+)(?:[eE][+-]?[0-9]+)?`
	svgLength    = regexp.MustCompile(`^` + svgNumber + `(?:px|em|%)?$`)
	svgNumbers   = regexp.MustCompile(`^[0-9eE.,+\- \t\r\n]*$`)
	svgPathData  = regexp.MustCompile(`^[MmLlHhVvCcSsQqTtAaZz0-9eE.,+\- \t\r\n]*$`)
	svgTransform = regexp.MustCompile(`^(?:[ \t\r\n,]*(?:matrix|translate|scale|rotate|skewX|skewY)\([0-9eE.,+\- \t\r\n]*\))*[ \t\r\n]*$`)
	svgID        = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,63}$`)
	// svgColor: a hex color, a color keyword, none or currentColor.
	svgColor = regexp.MustCompile(`^(?:#[0-9A-Fa-f]{3,8}|[A-Za-z]{1,30})$`)
	// svgLocalRef is the only reference allowed: a paint server inside the
	// document.
	svgLocalRef = regexp.MustCompile(`^url\(#[A-Za-z][A-Za-z0-9_-]{0,63}\)$`)
	// svgFontFamily: family names separated by commas, no quotes or escapes.
	svgFontFamily = regexp.MustCompile(`^[A-Za-z0-9 ,-]{1,200}$`)
	svgKeyword    = regexp.MustCompile(`^[A-Za-z-]{1,40}$`)
	svgFontWeight = regexp.MustCompile(`^(?:normal|bold|bolder|lighter|[1-9]00)$`)
)

func matches(re *regexp.Regexp) func(string) bool { return re.MatchString }

func oneOf(values ...string) func(string) bool {
	return func(v string) bool { return slices.Contains(values, v) }
}

func paint(v string) bool { return svgColor.MatchString(v) || svgLocalRef.MatchString(v) }

// plainText is for attributes that are not CSS (accessible names).
func plainText(v string) bool { return len(v) <= 200 && !strings.ContainsAny(v, "<>\\") }

// svgAttributes are the attributes allowed on any allowed element, with the
// grammar their value must match: geometry, presentation and text
// attributes. No style, href, event handlers or anything namespaced except
// xmlns itself.
var svgAttributes = map[string]func(string) bool{
	"xmlns":               oneOf(svgNamespace),
	"version":             oneOf("1.0", "1.1"),
	"id":                  matches(svgID),
	"viewBox":             matches(svgNumbers),
	"preserveAspectRatio": matches(regexp.MustCompile(`^[A-Za-z ]{1,40}$`)),
	"role":                oneOf("img", "presentation"),
	"aria-label":          plainText,
	"aria-hidden":         oneOf("true", "false"),

	"width": matches(svgLength), "height": matches(svgLength),
	"x": matches(svgLength), "y": matches(svgLength),
	"x1": matches(svgLength), "y1": matches(svgLength), "x2": matches(svgLength), "y2": matches(svgLength),
	"cx": matches(svgLength), "cy": matches(svgLength), "r": matches(svgLength),
	"rx": matches(svgLength), "ry": matches(svgLength), "fx": matches(svgLength), "fy": matches(svgLength),
	"dx": matches(svgLength), "dy": matches(svgLength),
	"d":      matches(svgPathData),
	"points": matches(svgNumbers),

	"transform":         matches(svgTransform),
	"gradientTransform": matches(svgTransform),
	"gradientUnits":     oneOf("userSpaceOnUse", "objectBoundingBox"),
	"spreadMethod":      oneOf("pad", "reflect", "repeat"),
	"offset":            matches(svgLength),
	"stop-color":        matches(svgColor),
	"stop-opacity":      matches(svgLength),

	"fill":            paint,
	"fill-opacity":    matches(svgLength),
	"fill-rule":       oneOf("nonzero", "evenodd"),
	"stroke":          paint,
	"stroke-width":    matches(svgLength),
	"stroke-opacity":  matches(svgLength),
	"stroke-linecap":  oneOf("butt", "round", "square"),
	"stroke-linejoin": oneOf("miter", "round", "bevel"),
	"opacity":         matches(svgLength),

	"font-family":       matches(svgFontFamily),
	"font-size":         matches(svgLength),
	"font-weight":       matches(svgFontWeight),
	"letter-spacing":    matches(svgLength),
	"text-anchor":       oneOf("start", "middle", "end"),
	"dominant-baseline": matches(svgKeyword),
}

func validateSVG(data []byte) error {
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.Strict = true // only the predefined XML entities
	// RawToken does not resolve namespaces (so prefixed names stay visible
	// and are rejected) nor check that end tags match: the stack does.
	var open []xml.Name
	roots := 0
	for {
		tok, err := dec.RawToken()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("invalid XML: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if len(open) == 0 {
				roots++
				if roots > 1 || t.Name.Local != "svg" || t.Name.Space != "" {
					return errors.New("the document must have a single <svg> root element")
				}
				if err := checkSVGNamespace(t); err != nil {
					return err
				}
			}
			if t.Name.Space != "" || !slices.Contains(svgElements, t.Name.Local) {
				return fmt.Errorf("element <%s> is not allowed", qualified(t.Name))
			}
			for _, a := range t.Attr {
				if err := checkSVGAttr(a); err != nil {
					return err
				}
			}
			open = append(open, t.Name)
		case xml.EndElement:
			if len(open) == 0 || open[len(open)-1] != t.Name {
				return fmt.Errorf("unexpected </%s>", qualified(t.Name))
			}
			open = open[:len(open)-1]
		case xml.ProcInst:
			if t.Target != "xml" {
				return fmt.Errorf("processing instruction <?%s?> is not allowed", t.Target)
			}
		case xml.Directive:
			return errors.New("DOCTYPE and other directives are not allowed")
		case xml.CharData:
			if len(open) == 0 && len(bytes.TrimSpace(t)) > 0 {
				return errors.New("text outside the root element")
			}
		case xml.Comment:
		}
	}
	if roots != 1 || len(open) != 0 {
		return errors.New("the document must have a single <svg> root element")
	}
	return nil
}

func checkSVGNamespace(root xml.StartElement) error {
	for _, a := range root.Attr {
		if a.Name.Space == "" && a.Name.Local == "xmlns" {
			if a.Value != svgNamespace {
				return fmt.Errorf("the default namespace must be %s", svgNamespace)
			}
			return nil
		}
	}
	return fmt.Errorf("the root element must declare xmlns=%q", svgNamespace)
}

func checkSVGAttr(a xml.Attr) error {
	name := qualified(a.Name)
	valid, ok := svgAttributes[a.Name.Local]
	if a.Name.Space != "" || !ok {
		return fmt.Errorf("attribute %q is not allowed", name)
	}
	if !valid(strings.TrimSpace(a.Value)) {
		return fmt.Errorf("attribute %q has a disallowed value", name)
	}
	return nil
}

func qualified(n xml.Name) string {
	if n.Space == "" {
		return n.Local
	}
	return n.Space + ":" + n.Local
}
