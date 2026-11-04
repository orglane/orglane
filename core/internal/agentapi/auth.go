package agentapi

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/orglane/orglane/core/internal/crypto"
)

// Signature headers (docs/specs/agent.md "Transport and authentication").
const (
	HeaderTimestamp     = "X-Orglane-Timestamp"
	HeaderNonce         = "X-Orglane-Nonce"
	HeaderSignature     = "X-Orglane-Signature"
	HeaderAuthorization = "Authorization"
	AuthScheme          = "Orglane-HMAC"
)

// MaxClockSkew is how far a request timestamp may be from the agent's clock.
const MaxClockSkew = 60 * time.Second

// NonceWindow is how long the agent remembers nonces. It covers the whole
// accepted timestamp range (±MaxClockSkew), so a replay is always detected.
const NonceWindow = 2 * MaxClockSkew

// nonceBytes is the size of a request nonce.
const nonceBytes = 16

const (
	requestContext  = "orglane-agent-request/v1\n"
	responseContext = "orglane-agent-response/v1\n"
)

// MinSecretLength is the minimum length of ORGLANE_AGENT_SECRET.
const MinSecretLength = crypto.MinSecretLength

// Key is the transport key derived from ORGLANE_AGENT_SECRET.
type Key [32]byte

// ErrWeakSecret is returned for secrets shorter than MinSecretLength.
var ErrWeakSecret = errors.New("agent secret must be at least 32 characters")

// DeriveKey derives the transport key from the agent secret with the
// purpose crypto.PurposeAgentTransport. The secret is never the master
// secret (ADR 0019).
func DeriveKey(secret string) (Key, error) {
	kr, err := crypto.NewKeyring(secret)
	if err != nil {
		return Key{}, ErrWeakSecret
	}
	return Key(kr.Key(crypto.PurposeAgentTransport)), nil
}

// PathQuery returns the signed request target: the escaped path and, if
// present, "?" and the raw query.
func PathQuery(u *url.URL) string {
	p := u.EscapedPath()
	if p == "" {
		p = "/"
	}
	if u.RawQuery != "" {
		p += "?" + u.RawQuery
	}
	return p
}

func bodyHash(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// RequestMAC computes the MAC of a request.
func RequestMAC(k Key, method, pathQuery, timestamp, nonce string, body []byte) []byte {
	m := hmac.New(sha256.New, k[:])
	m.Write([]byte(requestContext + method + "\n" + pathQuery + "\n" + timestamp + "\n" + nonce + "\n" + bodyHash(body)))
	return m.Sum(nil)
}

// ResponseMAC computes the MAC of a response to the request with requestMAC.
func ResponseMAC(k Key, requestMAC []byte, status int, body []byte) []byte {
	m := hmac.New(sha256.New, k[:])
	m.Write([]byte(responseContext + encode(requestMAC) + "\n" + strconv.Itoa(status) + "\n" + bodyHash(body)))
	return m.Sum(nil)
}

func encode(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// SignRequest sets the timestamp, nonce and authorization headers of req for
// body and returns the request MAC, which the response signature covers.
func SignRequest(k Key, req *http.Request, body []byte, now time.Time) []byte {
	ts := strconv.FormatInt(now.Unix(), 10)
	nonce := encode(crypto.RandomBytes(nonceBytes))
	mac := RequestMAC(k, req.Method, PathQuery(req.URL), ts, nonce, body)
	req.Header.Set(HeaderTimestamp, ts)
	req.Header.Set(HeaderNonce, nonce)
	req.Header.Set(HeaderAuthorization, AuthScheme+" "+encode(mac))
	return mac
}

// VerifyResponse checks the signature of a response to the request with
// requestMAC. Comparison is constant time.
func VerifyResponse(k Key, requestMAC []byte, status int, header http.Header, body []byte) bool {
	got, err := base64.RawURLEncoding.DecodeString(header.Get(HeaderSignature))
	if err != nil || len(got) != sha256.Size {
		return false
	}
	return hmac.Equal(got, ResponseMAC(k, requestMAC, status, body))
}

// SignResponse returns the X-Orglane-Signature value of a response.
func SignResponse(k Key, requestMAC []byte, status int, body []byte) string {
	return encode(ResponseMAC(k, requestMAC, status, body))
}

// Verification errors. They are not shown to the client, which only sees
// "unauthenticated".
var (
	ErrMissingSignature = errors.New("missing or malformed signature headers")
	ErrStaleRequest     = errors.New("request timestamp outside the accepted window")
	ErrBadSignature     = errors.New("wrong request signature")
	ErrReplayedRequest  = errors.New("replayed request nonce")
)

// Verifier checks request signatures on the agent side and remembers nonces
// to refuse replays.
type Verifier struct {
	key   Key
	start time.Time
	now   func() time.Time

	mu     sync.Mutex
	nonces map[string]time.Time // nonce -> expiry
	pruned time.Time
}

// NewVerifier returns a verifier that refuses requests timestamped before
// start (the agent's start: nonces of an earlier process are unknown).
func NewVerifier(k Key, start time.Time, now func() time.Time) *Verifier {
	if now == nil {
		now = time.Now
	}
	return &Verifier{key: k, start: start, now: now, nonces: map[string]time.Time{}}
}

// ParseMAC returns the request MAC from the authorization header, or nil.
// The response to a request is signed over this MAC.
func ParseMAC(h http.Header) []byte {
	scheme, value, ok := strings.Cut(h.Get(HeaderAuthorization), " ")
	if !ok || scheme != AuthScheme {
		return nil
	}
	mac, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(mac) != sha256.Size {
		return nil
	}
	return mac
}

// Verify checks the signature, freshness and uniqueness of a request with
// the given body and returns its MAC. The nonce is recorded only for
// correctly signed requests, so unauthenticated clients cannot fill the
// nonce cache.
func (v *Verifier) Verify(method string, u *url.URL, h http.Header, body []byte) ([]byte, error) {
	mac := ParseMAC(h)
	ts, nonce := h.Get(HeaderTimestamp), h.Get(HeaderNonce)
	if mac == nil || !decimal(ts) || !validNonce(nonce) {
		return nil, ErrMissingSignature
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return nil, ErrMissingSignature
	}
	now := v.now()
	t := time.Unix(sec, 0)
	if t.Before(now.Add(-MaxClockSkew)) || t.After(now.Add(MaxClockSkew)) || sec < v.start.Unix() {
		return nil, ErrStaleRequest
	}
	if !hmac.Equal(mac, RequestMAC(v.key, method, PathQuery(u), ts, nonce, body)) {
		return nil, ErrBadSignature
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if now.Sub(v.pruned) > NonceWindow/4 {
		for n, exp := range v.nonces {
			if now.After(exp) {
				delete(v.nonces, n)
			}
		}
		v.pruned = now
	}
	if exp, seen := v.nonces[nonce]; seen && !now.After(exp) {
		return nil, ErrReplayedRequest
	}
	v.nonces[nonce] = now.Add(NonceWindow)
	return mac, nil
}

func decimal(s string) bool {
	if s == "" || len(s) > 19 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func validNonce(s string) bool {
	b, err := base64.RawURLEncoding.DecodeString(s)
	return err == nil && len(b) == nonceBytes && encode(b) == s
}
