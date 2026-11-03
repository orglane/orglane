package crypto

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

const testSecret = "0123456789abcdef0123456789abcdef"

func TestKeyring(t *testing.T) {
	if _, err := NewKeyring("short"); !errors.Is(err, ErrWeakSecret) {
		t.Fatalf("NewKeyring(short) error = %v, want ErrWeakSecret", err)
	}
	k1, err := NewKeyring(testSecret)
	if err != nil {
		t.Fatal(err)
	}
	k2, _ := NewKeyring(testSecret)
	other, _ := NewKeyring(testSecret + "x")
	if k1.Key(PurposeCSRF) != k2.Key(PurposeCSRF) {
		t.Error("the same secret and purpose derive different keys")
	}
	if k1.Key(PurposeCSRF) == k1.Key(PurposeSigningKeys) {
		t.Error("two purposes derive the same key")
	}
	if k1.Key(PurposeCSRF) == other.Key(PurposeCSRF) {
		t.Error("two secrets derive the same key")
	}
}

func TestSealer(t *testing.T) {
	k, _ := NewKeyring(testSecret)
	s := k.Sealer(PurposeSigningKeys)
	ct := s.Seal([]byte("private key"), []byte("row-1"))
	if bytes.Contains(ct, []byte("private key")) {
		t.Fatal("ciphertext contains the plaintext")
	}
	if ct2 := s.Seal([]byte("private key"), []byte("row-1")); bytes.Equal(ct, ct2) {
		t.Error("two seals of the same plaintext are equal (nonce reuse)")
	}
	pt, err := s.Open(ct, []byte("row-1"))
	if err != nil || string(pt) != "private key" {
		t.Fatalf("Open() = %q, %v", pt, err)
	}
	for name, tc := range map[string]struct {
		ct, ad []byte
		s      *Sealer
	}{
		"other associated data": {ct, []byte("row-2"), s},
		"other purpose":         {ct, []byte("row-1"), k.Sealer(PurposeCSRF)},
		"truncated":             {ct[:10], []byte("row-1"), s},
		"tampered":              {append(append([]byte{}, ct[:len(ct)-1]...), ct[len(ct)-1]^1), []byte("row-1"), s},
		"unknown version":       {append([]byte{9}, ct[1:]...), []byte("row-1"), s},
	} {
		if _, err := tc.s.Open(tc.ct, tc.ad); !errors.Is(err, ErrDecrypt) {
			t.Errorf("%s: Open() error = %v, want ErrDecrypt", name, err)
		}
	}
}

func TestRandomToken(t *testing.T) {
	a, b := RandomToken("ols_"), RandomToken("ols_")
	if a == b || !strings.HasPrefix(a, "ols_") || len(a) != 4+43 {
		t.Fatalf("RandomToken() = %q, %q", a, b)
	}
	if !Equal(HashToken(a), HashToken(a)) || Equal(HashToken(a), HashToken(b)) {
		t.Error("HashToken is not a function of its input")
	}
}

// fastParams keep the unit tests quick; production uses DefaultPasswordParams.
var fastParams = PasswordParams{Memory: 64, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32}

func TestPasswordHash(t *testing.T) {
	ctx := context.Background()
	h := NewPasswordHasher(fastParams, 2)
	enc, err := h.Hash(ctx, "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(enc, "$argon2id$v=19$m=64,t=1,p=1$") {
		t.Fatalf("hash = %q, want a PHC string with its parameters", enc)
	}
	if enc2, _ := h.Hash(ctx, "correct horse battery staple"); enc2 == enc {
		t.Error("two hashes of one password are equal (salt reuse)")
	}
	if ok, rehash, err := h.Verify(ctx, "correct horse battery staple", enc); !ok || rehash || err != nil {
		t.Errorf("Verify(right) = %v, %v, %v", ok, rehash, err)
	}
	if ok, _, err := h.Verify(ctx, "wrong", enc); ok || err != nil {
		t.Errorf("Verify(wrong) = %v, %v", ok, err)
	}
	stronger := NewPasswordHasher(PasswordParams{Memory: 128, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32}, 1)
	if ok, rehash, err := stronger.Verify(ctx, "correct horse battery staple", enc); !ok || !rehash || err != nil {
		t.Errorf("Verify with upgraded params = %v, %v, %v; want ok and needsRehash", ok, rehash, err)
	}
	if _, err := h.Hash(ctx, strings.Repeat("a", MaxPasswordLength+1)); err == nil {
		t.Error("Hash accepted an oversized password")
	}
}

func TestDefaultPasswordParams(t *testing.T) {
	enc, err := NewPasswordHasher(DefaultPasswordParams, 1).Hash(context.Background(), "pw")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(enc, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("default hash = %q, want OWASP parameters m=19456,t=2,p=1", enc)
	}
}

func TestMalformedHashes(t *testing.T) {
	h := NewPasswordHasher(fastParams, 1)
	for _, enc := range []string{
		"",
		"plaintext",
		"$argon2i$v=19$m=64,t=1,p=1$c2FsdHNhbHRzYWx0$aGFzaGhhc2hoYXNoaGFzaA",
		"$argon2id$v=16$m=64,t=1,p=1$c2FsdHNhbHRzYWx0$aGFzaGhhc2hoYXNoaGFzaA",
		"$argon2id$v=19$m=99999999,t=1,p=1$c2FsdHNhbHRzYWx0$aGFzaGhhc2hoYXNoaGFzaA",
		"$argon2id$v=19$m=64,t=1000,p=1$c2FsdHNhbHRzYWx0$aGFzaGhhc2hoYXNoaGFzaA",
		"$argon2id$v=19$m=64,t=1,p=1$!!$aGFzaGhhc2hoYXNoaGFzaA",
	} {
		if _, _, err := h.Verify(context.Background(), "pw", enc); !errors.Is(err, ErrMalformedHash) {
			t.Errorf("Verify(%q) error = %v, want ErrMalformedHash", enc, err)
		}
	}
}

func TestHasherHonorsContext(t *testing.T) {
	h := NewPasswordHasher(fastParams, 1)
	h.slots <- struct{}{} // occupy the only slot
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := h.Hash(ctx, "pw"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Hash() with a full hasher and canceled context = %v", err)
	}
}
