package identity

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/store"
)

// Platform is the kind of client a device is.
type Platform string

// Device platforms.
const (
	PlatformWeb     Platform = "web"
	PlatformIOS     Platform = "ios"
	PlatformAndroid Platform = "android"
)

const maxDeviceNameLength = 100

// NewDevice describes a device to register.
type NewDevice struct {
	// Name is shown to the person and admins, e.g. "Firefox on macOS".
	Name     string
	Platform Platform
}

// RegisterDevice registers a device of a person. Suspended and offboarded
// people cannot register devices.
func (s *Service) RegisterDevice(ctx context.Context, actor audit.Actor, personID uuid.UUID, in NewDevice) (store.Device, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" || utf8.RuneCountInString(name) > maxDeviceNameLength || !utf8.ValidString(name) || strings.ContainsRune(name, 0) {
		return store.Device{}, invalid("name", "must be 1-%d characters", maxDeviceNameLength)
	}
	switch in.Platform {
	case PlatformWeb, PlatformIOS, PlatformAndroid:
	default:
		return store.Device{}, invalid("platform", "unknown platform %q", in.Platform)
	}

	var device store.Device
	err := s.inTx(ctx, func(q *store.Queries) error {
		person, err := q.GetPersonForUpdate(ctx, personID)
		if err != nil {
			return notFound(err, "person")
		}
		if st := Status(person.Status); st != StatusActive && st != StatusInvited {
			return fmt.Errorf("%w: %s", ErrInactive, st)
		}
		if device, err = q.CreateDevice(ctx, store.CreateDeviceParams{PersonID: personID, Name: name, Platform: string(in.Platform)}); err != nil {
			return fmt.Errorf("identity: register device: %w", err)
		}
		_, err = audit.Record(ctx, q, audit.Entry{
			Actor:    actor,
			Action:   audit.DeviceRegistered,
			Target:   deviceTarget(device.ID),
			Metadata: map[string]any{"person_id": personID, "platform": device.Platform},
		})
		return err
	})
	if err != nil {
		return store.Device{}, err
	}
	return device, nil
}

// RevokeDevice revokes a device and all its sessions. Clients wipe their app
// data when they learn about it (ADR 0012). Revoking a revoked device is a
// no-op.
func (s *Service) RevokeDevice(ctx context.Context, actor audit.Actor, id uuid.UUID) (store.Device, error) {
	var device store.Device
	err := s.inTx(ctx, func(q *store.Queries) error {
		current, err := q.GetDevice(ctx, id)
		if err != nil {
			return notFound(err, "device")
		}
		// Lock order: person, device, sessions (see Service).
		if _, err := q.GetPersonForUpdate(ctx, current.PersonID); err != nil {
			return notFound(err, "person")
		}
		if current, err = q.GetDeviceForUpdate(ctx, id); err != nil {
			return notFound(err, "device")
		}
		if current.RevokedAt != nil {
			device = current
			return nil
		}
		if device, err = q.RevokeDevice(ctx, id); err != nil {
			return fmt.Errorf("identity: revoke device: %w", err)
		}
		if _, err := audit.Record(ctx, q, audit.Entry{
			Actor:    actor,
			Action:   audit.DeviceRevoked,
			Target:   deviceTarget(id),
			Metadata: map[string]any{"person_id": device.PersonID},
		}); err != nil {
			return err
		}
		sessions, err := q.RevokeDeviceSessions(ctx, id)
		if err != nil {
			return fmt.Errorf("identity: revoke device sessions: %w", err)
		}
		return recordSessionsRevoked(ctx, q, actor, sessions, "device_revoked")
	})
	if err != nil {
		return store.Device{}, err
	}
	return device, nil
}

// GetDevice returns a device by ID.
func (s *Service) GetDevice(ctx context.Context, id uuid.UUID) (store.Device, error) {
	d, err := s.queries().GetDevice(ctx, id)
	if err != nil {
		return store.Device{}, notFound(err, "device")
	}
	return d, nil
}

// PersonDevices returns the devices of a person, oldest first.
func (s *Service) PersonDevices(ctx context.Context, personID uuid.UUID) ([]store.Device, error) {
	return s.queries().ListPersonDevices(ctx, personID)
}

func deviceTarget(id uuid.UUID) audit.Target {
	return audit.Target{Kind: audit.TargetDevice, ID: id.String()}
}
