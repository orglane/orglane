package mail

import (
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ErrUnsafeGrant is terminal: a new safe-channel invitation must be issued.
var ErrUnsafeGrant = errors.New("mail: invitation grant exceeds channel authority")

// smtpFailure carries only locally selected categories, never relay text or
// a wrapped cause. Relay rejection text may echo the entire message/token.
type smtpFailure struct{ stage, address string }

func (e *smtpFailure) Error() string {
	return fmt.Sprintf("SMTP %s failed at %s. Check the email settings.", e.stage, e.address)
}

var invitationSecretPattern = regexp.MustCompile(`oli_[A-Za-z0-9_-]+`)

// safeDeliveryFailure is the boundary before outbox, logs, API and River.
// Unknown diagnostics are discarded; even recognized categories redact
// configured credentials (including SMTP authentication encodings).
func safeDeliveryFailure(err error, smtp SMTP) error {
	message := "Email preparation failed. Check the system email settings."
	var failure *smtpFailure
	switch {
	case errors.Is(err, ErrUnsafeGrant):
		message = "This invitation requires a new link. Resend it and share the link directly."
	case errors.Is(err, ErrNotConfigured):
		message = "System email is not configured."
	case errors.As(err, &failure):
		message = failure.Error()
	}
	secrets := []string{smtp.Password, smtp.Username}
	if smtp.Username != "" || smtp.Password != "" {
		secrets = append(secrets, base64.StdEncoding.EncodeToString([]byte("\x00"+smtp.Username+"\x00"+smtp.Password)))
	}
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		message = strings.ReplaceAll(message, secret, "[redacted]")
		message = strings.ReplaceAll(message, base64.StdEncoding.EncodeToString([]byte(secret)), "[redacted]")
	}
	return errors.New(truncate(invitationSecretPattern.ReplaceAllString(message, "[redacted]")))
}
