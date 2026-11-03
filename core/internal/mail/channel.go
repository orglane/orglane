package mail

import (
	"slices"

	"github.com/orglane/orglane/core/internal/authz"
)

// InvitationChannel records where an activation capability was issued.
// Its permission ceiling remains binding after later SMTP or role changes.
type InvitationChannel string

// Persisted channel provenance; legacy means no trusted envelope was stored.
const (
	ChannelLegacy        InvitationChannel = "legacy"
	ChannelLink          InvitationChannel = "link"
	ChannelSettingsEmail InvitationChannel = "settings_email"
	ChannelConfigEmail   InvitationChannel = "config_email"
)

// EditableSMTPAllowsGrant uses the minimum permission held by EVERY possible
// SMTP configurator, including custom bundles. Built-in Admin permissions
// cannot be assumed for an arbitrary holder of instance.manage.
func EditableSMTPAllowsGrant(permissions []string) bool {
	for _, permission := range permissions {
		if permission != string(authz.InstanceManage) {
			return false
		}
	}
	return true
}

// InvitationChannelFor chooses a channel without granting control of a
// capability to a mail administrator who lacks its permissions.
func InvitationChannelFor(source Source, permissions []string) InvitationChannel {
	if source == SourceConfig {
		return ChannelConfigEmail
	}
	if source == SourceSettings && EditableSMTPAllowsGrant(permissions) {
		return ChannelSettingsEmail
	}
	return ChannelLink
}

// AllowsGrant rejects later privilege expansion and conservatively limits
// legacy emailed tokens. Copy-links remain usable with editable SMTP active.
func (c InvitationChannel) AllowsGrant(ceiling, current []string) bool {
	if c == ChannelLegacy {
		return EditableSMTPAllowsGrant(current)
	}
	if c != ChannelLink && c != ChannelConfigEmail && c != ChannelSettingsEmail {
		return false
	}
	if c == ChannelSettingsEmail && (!EditableSMTPAllowsGrant(ceiling) || !EditableSMTPAllowsGrant(current)) {
		return false
	}
	for _, permission := range current {
		if !slices.Contains(ceiling, permission) {
			return false
		}
	}
	return true
}

// AllowsSMTP validates the full capability authority, not just today's
// assignment: a temporarily removed permission can return within its ceiling.
func (c InvitationChannel) AllowsSMTP(source Source, ceiling, current []string) bool {
	if c == ChannelLink || !c.AllowsGrant(ceiling, current) {
		return false
	}
	if source == SourceConfig {
		return true
	}
	return source == SourceSettings && EditableSMTPAllowsGrant(ceiling) && EditableSMTPAllowsGrant(current)
}
