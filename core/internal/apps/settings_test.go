package apps

import (
	"errors"
	"strings"
	"testing"

	"github.com/orglane/orglane/providers/provider"
)

// Required settings: refused when omitted or null on creation (unless a
// default fills them) and never removed on update, for mutable and
// immutable ones alike.
func TestMergeSettingsRequired(t *testing.T) {
	specs := []provider.SettingSpec{
		{Name: "org", Type: provider.SettingString, Required: true},
		{Name: "team", Type: provider.SettingString, Required: true, Mutable: true},
		{Name: "port", Type: provider.SettingInt, Min: 1, Max: 9, Required: true, Default: 2},
		{Name: "note", Type: provider.SettingString, Mutable: true},
	}
	refused := func(t *testing.T, err error, name string) {
		t.Helper()
		if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "settings."+name) {
			t.Fatalf("error = %v, want settings.%s refused", err, name)
		}
	}
	for name, changes := range map[string]map[string]any{
		"org omitted":  {"team": "a"},
		"org null":     {"org": nil, "team": "a"},
		"team omitted": {"org": "acme"},
		"team null":    {"org": "acme", "team": nil},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := mergeSettings(specs, nil, changes, true)
			refused(t, err, strings.Fields(name)[0])
		})
	}
	got, err := mergeSettings(specs, nil, map[string]any{"org": "acme", "team": "a"}, true)
	if err != nil || got["port"] != int64(2) {
		t.Fatalf("create = %v, %v (the default fills a required setting)", got, err)
	}
	// On creation null is unset: the default fills it too.
	if withNull, err := mergeSettings(specs, nil, map[string]any{"org": "acme", "team": "a", "port": nil}, true); err != nil || withNull["port"] != int64(2) {
		t.Fatalf("create with a null defaulted setting = %v, %v", withNull, err)
	}
	// Update: a required setting cannot be removed; others can.
	_, err = mergeSettings(specs, got, map[string]any{"team": nil}, false)
	refused(t, err, "team")
	_, err = mergeSettings(specs, got, map[string]any{"port": nil}, false)
	refused(t, err, "port")
	_, err = mergeSettings(specs, got, map[string]any{"org": nil}, false)
	refused(t, err, "org")
	if _, err := mergeSettings(specs, got, map[string]any{"note": nil, "team": "b"}, false); err != nil {
		t.Fatalf("update: %v", err)
	}
}
