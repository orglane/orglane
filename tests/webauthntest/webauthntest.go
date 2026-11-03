// Package webauthntest is a software WebAuthn authenticator for tests: it
// answers navigator.credentials.create and .get the way a browser with a
// platform authenticator would, with ES256 keys and "none" attestation, so
// passkey registration and sign-in can be tested end to end against the
// real verification code.
package webauthntest

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/go-webauthn/webauthn/protocol/webauthncbor"
)

// Flags of the authenticator data.
const (
	flagUserPresent  = 0x01
	flagUserVerified = 0x04
	flagAttested     = 0x40
)

var b64 = base64.RawURLEncoding

// Authenticator holds discoverable credentials and signs ceremonies for one
// origin.
type Authenticator struct {
	// Origin is the web origin the "browser" reports, e.g.
	// https://work.acme.test.
	Origin string

	mu    sync.Mutex
	creds []*credential
}

type credential struct {
	id         []byte
	rpID       string
	userHandle []byte
	key        *ecdsa.PrivateKey
	signCount  uint32
}

// New returns an authenticator for origin.
func New(origin string) *Authenticator {
	return &Authenticator{Origin: origin}
}

// Credentials returns how many credentials the authenticator holds.
func (a *Authenticator) Credentials() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.creds)
}

type creationOptions struct {
	Challenge string `json:"challenge"`
	RP        struct {
		ID string `json:"id"`
	} `json:"rp"`
	User struct {
		ID string `json:"id"`
	} `json:"user"`
}

// Create answers PublicKeyCredentialCreationOptions (the publicKey member,
// JSON) with a registration response (PublicKeyCredential.toJSON()).
func (a *Authenticator) Create(publicKey map[string]any) (map[string]any, error) {
	var opts creationOptions
	if err := remarshal(publicKey, &opts); err != nil {
		return nil, err
	}
	userHandle, err := b64.DecodeString(opts.User.ID)
	if err != nil {
		return nil, fmt.Errorf("user.id: %w", err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	c := &credential{id: randomBytes(32), rpID: opts.RP.ID, userHandle: userHandle, key: key}

	clientData, err := a.clientData("webauthn.create", opts.Challenge)
	if err != nil {
		return nil, err
	}
	point, err := key.PublicKey.Bytes() // 0x04 || X || Y
	if err != nil {
		return nil, err
	}
	coseKey, err := webauthncbor.Marshal(map[int]any{
		1:  2,  // kty: EC2
		3:  -7, // alg: ES256
		-1: 1,  // crv: P-256
		-2: point[1:33],
		-3: point[33:65],
	})
	if err != nil {
		return nil, err
	}
	var authData bytes.Buffer
	authData.Write(rpIDHash(c.rpID))
	authData.WriteByte(flagUserPresent | flagUserVerified | flagAttested)
	_ = binary.Write(&authData, binary.BigEndian, c.signCount)
	authData.Write(make([]byte, 16))                                 // AAGUID
	_ = binary.Write(&authData, binary.BigEndian, uint16(len(c.id))) //nolint:gosec // 32 bytes
	authData.Write(c.id)
	authData.Write(coseKey)

	attestation, err := webauthncbor.Marshal(map[string]any{
		"fmt":      "none",
		"attStmt":  map[string]any{},
		"authData": authData.Bytes(),
	})
	if err != nil {
		return nil, err
	}

	a.mu.Lock()
	a.creds = append(a.creds, c)
	a.mu.Unlock()
	return map[string]any{
		"id":    b64.EncodeToString(c.id),
		"rawId": b64.EncodeToString(c.id),
		"type":  "public-key",
		"response": map[string]any{
			"clientDataJSON":    b64.EncodeToString(clientData),
			"attestationObject": b64.EncodeToString(attestation),
			"transports":        []string{"internal"},
		},
		"authenticatorAttachment": "platform",
		"clientExtensionResults":  map[string]any{},
	}, nil
}

type requestOptions struct {
	Challenge string `json:"challenge"`
	RPID      string `json:"rpId"`
}

// ErrNoCredential is returned by Get without a credential for the RP.
var ErrNoCredential = errors.New("webauthntest: no credential for this relying party")

// Get answers PublicKeyCredentialRequestOptions with an assertion from the
// most recently created credential for the relying party.
func (a *Authenticator) Get(publicKey map[string]any) (map[string]any, error) {
	var opts requestOptions
	if err := remarshal(publicKey, &opts); err != nil {
		return nil, err
	}
	a.mu.Lock()
	var c *credential
	for i := len(a.creds) - 1; i >= 0; i-- {
		if a.creds[i].rpID == opts.RPID {
			c = a.creds[i]
			break
		}
	}
	if c == nil {
		a.mu.Unlock()
		return nil, ErrNoCredential
	}
	c.signCount++
	count := c.signCount
	a.mu.Unlock()

	clientData, err := a.clientData("webauthn.get", opts.Challenge)
	if err != nil {
		return nil, err
	}
	var authData bytes.Buffer
	authData.Write(rpIDHash(c.rpID))
	authData.WriteByte(flagUserPresent | flagUserVerified)
	_ = binary.Write(&authData, binary.BigEndian, count)

	clientHash := sha256.Sum256(clientData)
	digest := sha256.Sum256(append(authData.Bytes(), clientHash[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, c.key, digest[:])
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"id":    b64.EncodeToString(c.id),
		"rawId": b64.EncodeToString(c.id),
		"type":  "public-key",
		"response": map[string]any{
			"clientDataJSON":    b64.EncodeToString(clientData),
			"authenticatorData": b64.EncodeToString(authData.Bytes()),
			"signature":         b64.EncodeToString(sig),
			"userHandle":        b64.EncodeToString(c.userHandle),
		},
		"authenticatorAttachment": "platform",
		"clientExtensionResults":  map[string]any{},
	}, nil
}

func (a *Authenticator) clientData(typ, challenge string) ([]byte, error) {
	return json.Marshal(map[string]any{
		"type":        typ,
		"challenge":   challenge,
		"origin":      a.Origin,
		"crossOrigin": false,
	})
}

func rpIDHash(rpID string) []byte {
	h := sha256.Sum256([]byte(rpID))
	return h[:]
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

func remarshal(in, out any) error {
	raw, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}
