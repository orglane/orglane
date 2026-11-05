package provider

import "time"

// Feature is a row of the capability matrix.
type Feature string

// Support says how far a provider implements a feature.
type Support string

// Support levels. The matrix is honest: what is not implemented is
// unsupported or planned, never omitted.
const (
	Supported   Support = "supported"
	Partial     Support = "partial"
	Planned     Support = "planned"
	Unsupported Support = "unsupported"
)

// FeatureSupport is one cell of the capability matrix.
type FeatureSupport struct {
	Feature Feature
	Support Support
	// Note is an i18n key, e.g.
	// "providers.github.features.accounts_invite_only".
	Note string
	// Milestone is set for Planned, e.g. "M5".
	Milestone string
}

// Feature keys.
const (
	FeatureSSO                     Feature = "sso"
	FeatureDeepLinks               Feature = "deep_links"
	FeatureAccountsCreate          Feature = "accounts.create"
	FeatureAccountsInvite          Feature = "accounts.invite"
	FeatureAccountsLink            Feature = "accounts.link"
	FeatureTeamsSync               Feature = "teams.sync"
	FeatureDeprovisionTeamRemoval  Feature = "deprovision.team_removal"
	FeatureDeprovisionOrgRemoval   Feature = "deprovision.org_removal"
	FeatureDeprovisionSuspend      Feature = "deprovision.suspend"
	FeatureDeprovisionRevokeTokens Feature = "deprovision.revoke_tokens" //nolint:gosec // a feature key, not a credential
	FeatureEventsWebhooks          Feature = "events.webhooks"
	FeatureLifecycleInstall        Feature = "lifecycle.install"
	FeatureLifecycleUpgrade        Feature = "lifecycle.upgrade"
	FeatureLifecycleSnapshot       Feature = "lifecycle.snapshot"
	FeatureLifecycleBackup         Feature = "lifecycle.backup"
)

// Features lists every feature key in matrix order. A manifest must declare
// each of them exactly once.
var Features = []Feature{
	FeatureSSO,
	FeatureDeepLinks,
	FeatureAccountsCreate,
	FeatureAccountsInvite,
	FeatureAccountsLink,
	FeatureTeamsSync,
	FeatureDeprovisionTeamRemoval,
	FeatureDeprovisionOrgRemoval,
	FeatureDeprovisionSuspend,
	FeatureDeprovisionRevokeTokens,
	FeatureEventsWebhooks,
	FeatureLifecycleInstall,
	FeatureLifecycleUpgrade,
	FeatureLifecycleSnapshot,
	FeatureLifecycleBackup,
}

// Event is a normalized event (docs/specs/providers.md "Normalized events").
type Event struct {
	Type          string // e.g. "code.pull_request.opened"
	SchemaVersion int    // 1
	// DedupeKey is derived from signed body fields only, per type; never
	// from headers.
	DedupeKey  string
	OccurredAt time.Time
	Actor      ExternalRef
	Subject    Subject
	Targets    []Target
	// Data is type-specific, documented per type, small.
	Data map[string]any
}

// ExternalRef is an account referenced by an event.
type ExternalRef struct {
	ExternalID string `json:"external_id,omitempty"` // omitted when only a login is known (mentions)
	Login      string `json:"login"`
}

// Subject is what an event is about.
type Subject struct {
	Kind       string `json:"kind"`                 // "pull_request" | "issue" | "repository" | "branch" | "organization" | "team"
	ExternalID string `json:"external_id"`          // provider's stable ID
	Repository string `json:"repository,omitempty"` // "owner/name"
	Number     int    `json:"number,omitempty"`
	Title      string `json:"title,omitempty"`
	URL        string `json:"url,omitempty"` // validated against the instance base URL
}

// Target is an account an event is addressed to.
type Target struct {
	ExternalRef
	Reason string `json:"reason"` // "review_requested" | "mentioned" | "assigned" | "member_added" | ...
}
