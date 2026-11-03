package mail

import (
	"testing"

	"github.com/orglane/orglane/core/internal/authz"
)

func TestEditableSMTPGrantBoundary(t *testing.T) {
	for _, tc := range []struct {
		name        string
		permissions []string
		channel     InvitationChannel
	}{
		{"no permissions", []string{}, ChannelSettingsEmail},
		{"configurator only", []string{string(authz.InstanceManage)}, ChannelSettingsEmail},
		{"custom sensitive grant", []string{string(authz.PeopleSensitiveRead)}, ChannelLink},
		{"configurator with extra grant", []string{string(authz.InstanceManage), string(authz.BillingManage)}, ChannelLink},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := InvitationChannelFor(SourceSettings, tc.permissions); got != tc.channel {
				t.Fatalf("channel %q, want %q", got, tc.channel)
			}
			if got := InvitationChannelFor(SourceConfig, tc.permissions); got != ChannelConfigEmail {
				t.Fatalf("immutable configuration channel = %q", got)
			}
			if got := InvitationChannelFor(SourceNone, tc.permissions); got != ChannelLink {
				t.Fatalf("unconfigured channel = %q", got)
			}
		})
	}
}

func TestInvitationGrantEnvelope(t *testing.T) {
	high := []string{string(authz.PeopleSensitiveRead)}
	for _, channel := range []InvitationChannel{ChannelLink, ChannelConfigEmail} {
		if !channel.AllowsGrant(high, high) {
			t.Fatalf("trusted %q capability rejected", channel)
		}
		if channel.AllowsGrant([]string{}, high) {
			t.Fatalf("%q capability gained permissions after issue", channel)
		}
	}
	if ChannelSettingsEmail.AllowsGrant(high, high) || ChannelLegacy.AllowsGrant(nil, high) {
		t.Fatal("editable or legacy email can grant sensitive access")
	}
	if ChannelLegacy.AllowsGrant(nil, high) {
		t.Fatal("unknown legacy ceiling authorized a privileged direct link")
	}
	if !ChannelSettingsEmail.AllowsGrant([]string{}, []string{}) {
		t.Fatal("ordinary invitations stopped working")
	}
}
