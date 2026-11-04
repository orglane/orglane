package apps

import (
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// MaxResponseBytes bounds every provider response body read through the
// adapters' HTTP client.
const MaxResponseBytes = 10 << 20

// NewHTTPClient returns the client handed to adapters: timeouts, a response
// size limit, no cookies and no redirects to another host.
func NewHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 20 * time.Second
	return &http.Client{
		Timeout:   30 * time.Second,
		Transport: limitedTransport{next: transport},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("apps: too many redirects")
			}
			if req.URL.Host != via[0].URL.Host || req.URL.Scheme != via[0].URL.Scheme {
				return errors.New("apps: redirect to another host refused")
			}
			return nil
		},
	}
}

type limitedTransport struct{ next http.RoundTripper }

func (t limitedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	res, err := t.next.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	res.Body = &limitedBody{ReadCloser: res.Body, left: MaxResponseBytes}
	return res, nil
}

// ErrResponseTooLarge is returned when a provider response body exceeds
// MaxResponseBytes, so an adapter never decodes a silently cut body.
var ErrResponseTooLarge = errors.New("apps: provider response too large")

// limitedBody reads at most MaxResponseBytes and fails, rather than ending
// early, when the body is longer.
type limitedBody struct {
	io.ReadCloser
	left int64
}

func (b *limitedBody) Read(p []byte) (int, error) {
	if b.left <= 0 {
		// At the limit: one more byte means the body is too large.
		var one [1]byte
		n, err := b.ReadCloser.Read(one[:])
		if n > 0 {
			return 0, ErrResponseTooLarge
		}
		return 0, err
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.ReadCloser.Read(p)
	b.left -= int64(n)
	return n, err
}

// rateLimitedLog lets one message through per interval.
type rateLimitedLog struct {
	mu    sync.Mutex
	every time.Duration
	last  time.Time
}

func (l *rateLimitedLog) allow(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.last) < l.every {
		return false
	}
	l.last = now
	return true
}

func isUnique(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && (constraint == "" || pgErr.ConstraintName == constraint)
}
