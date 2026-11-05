// Package crypto holds the core's cryptographic building blocks: keys derived
// from the instance master secret, authenticated encryption of data at rest,
// password hashing and random tokens.
//
// It wraps well-reviewed primitives (HKDF-SHA256, AES-256-GCM, argon2id from
// the Go standard library and golang.org/x/crypto) and implements no
// cryptography of its own (ADR 0003: "don't own the cryptography").
package crypto

import (
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
)

// Purpose names what a derived key is used for. Every purpose gets an
// independent key; never reuse one for two purposes.
type Purpose string

// Key purposes. The strings are part of the derivation and never change,
// or data encrypted under the old key becomes unreadable.
const (
	// PurposeSigningKeys encrypts OIDC signing keys stored in the database.
	PurposeSigningKeys Purpose = "orglane/v1/signing-keys"
	// PurposeOIDCTokens encrypts opaque OIDC access tokens and codes.
	PurposeOIDCTokens Purpose = "orglane/v1/oidc-tokens" //nolint:gosec // a derivation label, not a credential
	// PurposeCSRF signs CSRF tokens bound to a session.
	PurposeCSRF Purpose = "orglane/v1/csrf"
	// PurposeProfileFields encrypts values of sensitive profile fields
	// (ADR 0015).
	PurposeProfileFields Purpose = "orglane/v1/profile-fields"
	// PurposeImportStaging encrypts all uploaded import cells and preview snapshots.
	PurposeImportStaging Purpose = "orglane/v1/import-staging"
	// PurposeImportIdentifiers hashes low-entropy external identifiers without dictionary disclosure.
	PurposeImportIdentifiers Purpose = "orglane/v1/import-external-identifiers"
	// PurposeEmailSettings encrypts the SMTP password in the email settings.
	PurposeEmailSettings Purpose = "orglane/v1/email-settings"
	// PurposeEmailOutbox encrypts queued emails (they carry invitation links).
	PurposeEmailOutbox Purpose = "orglane/v1/email-outbox"
	// PurposeAgentTransport signs requests and responses between the core and
	// the agent (docs/specs/agent.md). It is derived from ORGLANE_AGENT_SECRET,
	// never from the master secret (ADR 0019).
	PurposeAgentTransport Purpose = "orglane/v1/agent-transport"
	// PurposeAppCredentials encrypts credentials of app instances (provider
	// tokens, keys, generated passwords and webhook secrets).
	PurposeAppCredentials Purpose = "orglane/v1/app-credentials" //nolint:gosec // a derivation label, not a credential
	// PurposeWebhookDeliveries encrypts stored webhook delivery bodies.
	PurposeWebhookDeliveries Purpose = "orglane/v1/webhook-deliveries"
	// PurposeAppOAuthState encrypts PKCE verifiers of provider round trips.
	PurposeAppOAuthState Purpose = "orglane/v1/app-oauth-state"
)

// keySize is the size of every derived key (256 bits).
const keySize = 32

// Keyring derives keys from the instance master secret.
type Keyring struct {
	secret []byte
}

// ErrWeakSecret is returned for master secrets shorter than MinSecretLength.
var ErrWeakSecret = errors.New("crypto: master secret is too short")

// MinSecretLength is the minimum master secret length in bytes.
const MinSecretLength = 32

// NewKeyring returns a keyring for the master secret.
func NewKeyring(masterSecret string) (*Keyring, error) {
	if len(masterSecret) < MinSecretLength {
		return nil, ErrWeakSecret
	}
	return &Keyring{secret: []byte(masterSecret)}, nil
}

// Key derives the 256-bit key for a purpose with HKDF-SHA256.
func (k *Keyring) Key(p Purpose) [keySize]byte {
	b, err := hkdf.Key(sha256.New, k.secret, nil, string(p), keySize)
	if err != nil {
		// Only possible for invalid lengths, which are constant here.
		panic(fmt.Sprintf("crypto: derive key: %v", err))
	}
	return [keySize]byte(b)
}

// Sealer derives the key for a purpose and returns a sealer using it.
func (k *Keyring) Sealer(p Purpose) *Sealer {
	key := k.Key(p)
	s, err := NewSealer(key[:])
	if err != nil {
		panic(fmt.Sprintf("crypto: sealer: %v", err)) // the key size is fixed
	}
	return s
}

// RandomBytes returns n bytes from the system CSPRNG.
func RandomBytes(n int) []byte {
	b := make([]byte, n)
	// crypto/rand.Read never returns an error (Go 1.24+); it crashes the
	// program if the system CSPRNG fails.
	_, _ = rand.Read(b)
	return b
}

// RandomToken returns prefix followed by 32 random bytes in unpadded
// base64url (256 bits of entropy). Prefixes make leaked tokens easy to
// recognize.
func RandomToken(prefix string) string {
	return prefix + base64.RawURLEncoding.EncodeToString(RandomBytes(32))
}

// HashToken returns the SHA-256 hash of a high-entropy token for storage. It
// must only be used for random tokens (256 bits), never for passwords.
func HashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// Equal compares two byte slices in constant time.
func Equal(a, b []byte) bool {
	return subtle.ConstantTimeCompare(a, b) == 1
}
