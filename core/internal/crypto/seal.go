package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"fmt"
)

// sealVersion prefixes every ciphertext so the format can change later.
const sealVersion byte = 1

// ErrDecrypt is returned for ciphertexts that cannot be opened: tampered,
// truncated, sealed with another key or with other associated data.
var ErrDecrypt = errors.New("crypto: cannot decrypt")

// Sealer encrypts and authenticates data with AES-256-GCM and random nonces.
type Sealer struct {
	aead cipher.AEAD
}

// NewSealer returns a sealer for a 256-bit key.
func NewSealer(key []byte) (*Sealer, error) {
	if len(key) != keySize {
		return nil, fmt.Errorf("crypto: key must be %d bytes", keySize)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Sealer{aead: aead}, nil
}

// Seal encrypts plaintext. additionalData is authenticated but not
// encrypted; Open must be given the same value. Bind ciphertexts to their
// context (for example a row ID) so they cannot be swapped between rows.
func (s *Sealer) Seal(plaintext, additionalData []byte) []byte {
	nonce := RandomBytes(s.aead.NonceSize())
	out := make([]byte, 0, 1+len(nonce)+len(plaintext)+s.aead.Overhead())
	out = append(out, sealVersion)
	out = append(out, nonce...)
	return s.aead.Seal(out, nonce, plaintext, additionalData)
}

// Open decrypts a ciphertext produced by Seal.
func (s *Sealer) Open(ciphertext, additionalData []byte) ([]byte, error) {
	n := s.aead.NonceSize()
	if len(ciphertext) < 1+n+s.aead.Overhead() || ciphertext[0] != sealVersion {
		return nil, ErrDecrypt
	}
	plaintext, err := s.aead.Open(nil, ciphertext[1:1+n], ciphertext[1+n:], additionalData)
	if err != nil {
		return nil, ErrDecrypt
	}
	return plaintext, nil
}
