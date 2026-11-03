package crypto

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// PasswordParams are argon2id parameters. They are stored with every hash,
// so they can be raised later: hashes made with older parameters still
// verify, and NeedsRehash tells when to upgrade them.
type PasswordParams struct {
	// Memory in KiB.
	Memory uint32
	// Iterations (time cost).
	Iterations uint32
	// Parallelism (lanes).
	Parallelism uint8
	// SaltLength and KeyLength in bytes.
	SaltLength uint32
	KeyLength  uint32
}

// DefaultPasswordParams follow the OWASP Password Storage Cheat Sheet
// (argon2id, m=19 MiB, t=2, p=1) with a 128-bit salt and a 256-bit hash.
var DefaultPasswordParams = PasswordParams{
	Memory:      19 * 1024,
	Iterations:  2,
	Parallelism: 1,
	SaltLength:  16,
	KeyLength:   32,
}

// Limits on stored parameters, so a corrupted or hostile hash cannot make
// verification allocate unbounded memory or run forever.
const (
	maxMemory     = 1 << 20 // 1 GiB in KiB
	maxIterations = 64
	maxKeyLength  = 128
)

// MaxPasswordLength bounds password input in bytes. Long passphrases are
// fine; megabytes of input are not.
const MaxPasswordLength = 1024

// ErrMalformedHash is returned for stored hashes that cannot be parsed.
var ErrMalformedHash = errors.New("crypto: malformed password hash")

// PasswordHasher hashes and verifies passwords with argon2id. It bounds the
// number of concurrent hash computations, since each one holds Memory KiB:
// a burst of sign-in attempts queues instead of exhausting memory.
type PasswordHasher struct {
	params PasswordParams
	slots  chan struct{}
}

// NewPasswordHasher returns a hasher using params for new hashes and
// allowing at most concurrency computations at a time.
func NewPasswordHasher(params PasswordParams, concurrency int) *PasswordHasher {
	if concurrency < 1 {
		concurrency = 1
	}
	return &PasswordHasher{params: params, slots: make(chan struct{}, concurrency)}
}

func (h *PasswordHasher) acquire(ctx context.Context) error {
	select {
	case h.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *PasswordHasher) release() { <-h.slots }

// Hash returns the PHC string of password with a fresh random salt:
// $argon2id$v=19$m=19456,t=2,p=1$<salt>$<hash>.
func (h *PasswordHasher) Hash(ctx context.Context, password string) (string, error) {
	if len(password) > MaxPasswordLength {
		return "", fmt.Errorf("crypto: password longer than %d bytes", MaxPasswordLength)
	}
	if err := h.acquire(ctx); err != nil {
		return "", err
	}
	defer h.release()
	p := h.params
	salt := RandomBytes(int(p.SaltLength))
	key := argon2.IDKey([]byte(password), salt, p.Iterations, p.Memory, p.Parallelism, p.KeyLength)
	return encodePHC(p, salt, key), nil
}

// Verify reports whether password matches the PHC string encoded. It also
// reports whether the hash should be replaced because it was made with
// other parameters than the hasher's.
func (h *PasswordHasher) Verify(ctx context.Context, password, encoded string) (ok, needsRehash bool, err error) {
	p, salt, want, err := decodePHC(encoded)
	if err != nil {
		return false, false, err
	}
	if len(password) > MaxPasswordLength {
		return false, false, nil
	}
	if err := h.acquire(ctx); err != nil {
		return false, false, err
	}
	defer h.release()
	got := argon2.IDKey([]byte(password), salt, p.Iterations, p.Memory, p.Parallelism, uint32(len(want))) //nolint:gosec // bounded by decodePHC
	ok = subtle.ConstantTimeCompare(got, want) == 1
	return ok, ok && p != h.params, nil
}

func encodePHC(p PasswordParams, salt, key []byte) string {
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.Memory, p.Iterations, p.Parallelism, b64.EncodeToString(salt), b64.EncodeToString(key))
}

func decodePHC(encoded string) (PasswordParams, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return PasswordParams{}, nil, nil, ErrMalformedHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return PasswordParams{}, nil, nil, ErrMalformedHash
	}
	var p PasswordParams
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.Memory, &p.Iterations, &p.Parallelism); err != nil {
		return PasswordParams{}, nil, nil, ErrMalformedHash
	}
	b64 := base64.RawStdEncoding
	salt, err := b64.DecodeString(parts[4])
	if err != nil {
		return PasswordParams{}, nil, nil, ErrMalformedHash
	}
	key, err := b64.DecodeString(parts[5])
	if err != nil {
		return PasswordParams{}, nil, nil, ErrMalformedHash
	}
	if p.Memory < 8*uint32(p.Parallelism) || p.Memory > maxMemory || p.Iterations < 1 || p.Iterations > maxIterations ||
		p.Parallelism < 1 || len(salt) < 8 || len(key) < 16 || len(key) > maxKeyLength {
		return PasswordParams{}, nil, nil, ErrMalformedHash
	}
	p.SaltLength = uint32(len(salt)) //nolint:gosec // checked above
	p.KeyLength = uint32(len(key))   //nolint:gosec // checked above
	return p, salt, key, nil
}
