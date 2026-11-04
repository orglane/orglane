package apps

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"unicode/utf8"

	"github.com/orglane/orglane/providers/provider"
)

// maxSettingLength bounds string settings.
const maxSettingLength = 500

func validateSettingSpec(s provider.SettingSpec) error {
	switch s.Type {
	case provider.SettingString:
		if s.Pattern != "" {
			if _, err := regexp.Compile(anchored(s.Pattern)); err != nil {
				return fmt.Errorf("invalid pattern: %w", err)
			}
		}
	case provider.SettingInt:
		if s.Min > s.Max {
			return errors.New("min is greater than max")
		}
	case provider.SettingBool:
	default:
		return fmt.Errorf("unknown type %q", s.Type)
	}
	if s.Default != nil {
		if _, err := settingValue(s, s.Default); err != nil {
			return fmt.Errorf("invalid default: %w", err)
		}
	}
	return nil
}

func anchored(pattern string) string { return `^(?:` + pattern + `)$` }

// settingValue validates one value and returns it in canonical form
// (string, int64 or bool).
func settingValue(s provider.SettingSpec, v any) (any, error) {
	switch s.Type {
	case provider.SettingString:
		str, ok := v.(string)
		if !ok {
			return nil, errors.New("must be a string")
		}
		if !utf8.ValidString(str) || len(str) > maxSettingLength {
			return nil, fmt.Errorf("must be valid text of at most %d bytes", maxSettingLength)
		}
		if len(s.Enum) > 0 && !slices.Contains(s.Enum, str) {
			return nil, fmt.Errorf("must be one of %v", s.Enum)
		}
		if s.Pattern != "" && !regexp.MustCompile(anchored(s.Pattern)).MatchString(str) {
			return nil, errors.New("has an invalid format")
		}
		return str, nil
	case provider.SettingInt:
		var n int64
		switch x := v.(type) {
		case int:
			n = int64(x)
		case int64:
			n = x
		case float64:
			if x != math.Trunc(x) || x < math.MinInt64 || x > math.MaxInt64 {
				return nil, errors.New("must be an integer")
			}
			n = int64(x)
		case json.Number:
			var err error
			if n, err = x.Int64(); err != nil {
				return nil, errors.New("must be an integer")
			}
		default:
			return nil, errors.New("must be an integer")
		}
		if (s.Min != 0 || s.Max != 0) && (n < s.Min || n > s.Max) {
			return nil, fmt.Errorf("must be between %d and %d", s.Min, s.Max)
		}
		return n, nil
	case provider.SettingBool:
		b, ok := v.(bool)
		if !ok {
			return nil, errors.New("must be true or false")
		}
		return b, nil
	}
	return nil, fmt.Errorf("unknown type %q", s.Type)
}

// mergeSettings validates changes against the manifest's settings and
// returns the merged settings. Unknown names are refused; immutable
// settings can only be set when the instance is created. Defaults fill in
// unset settings on creation.
func mergeSettings(specs []provider.SettingSpec, current, changes map[string]any, creating bool) (map[string]any, error) {
	out := make(map[string]any, len(specs))
	for k, v := range current {
		out[k] = v
	}
	for name, v := range changes {
		i := slices.IndexFunc(specs, func(s provider.SettingSpec) bool { return s.Name == name })
		if i < 0 {
			return nil, invalid("settings."+name, "unknown setting")
		}
		spec := specs[i]
		if !creating && !spec.Mutable {
			if old, ok := current[name]; !ok || !sameSetting(spec, old, v) {
				return nil, invalid("settings."+name, "cannot be changed after the instance is created")
			}
		}
		if v == nil {
			delete(out, name)
			continue
		}
		val, err := settingValue(spec, v)
		if err != nil {
			return nil, invalid("settings."+name, "%v", err)
		}
		out[name] = val
	}
	if creating {
		for _, s := range specs {
			if _, ok := out[s.Name]; !ok && s.Default != nil {
				v, _ := settingValue(s, s.Default)
				out[s.Name] = v
			}
		}
	}
	return out, nil
}

func sameSetting(spec provider.SettingSpec, a, b any) bool {
	x, err1 := settingValue(spec, a)
	y, err2 := settingValue(spec, b)
	return err1 == nil && err2 == nil && x == y
}

// decodeSettings reads stored settings with numbers kept exact.
func decodeSettings(raw []byte) (map[string]any, error) {
	out := map[string]any{}
	if len(raw) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("apps: decode settings: %w", err)
	}
	return out, nil
}
