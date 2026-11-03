// Package demomode runs an instance in demo mode (`orglane server --demo`,
// or demo.enabled in the configuration): it loads a demo dataset (package
// demo) into an empty instance and remembers that the instance is a demo.
//
// The dataset is loaded through the identity service, so every person, role,
// avatar, team and team membership is created like any other, with its
// audit entry; the actor is
// the system with the user agent "orglane-demo". Demo mode never touches a
// database that holds anything else: Prepare refuses a database that has
// people but was not loaded as a demo.
//
// The instance setting "demo" marks a demo database. It is written in two
// steps, "loading" before the first person and "loaded" with an
// instance.demo_loaded audit entry at the end, so a load interrupted by a
// crash is completed on the next start instead of being refused. The marker
// records the dataset version: a database loaded from an older version of
// the dataset gets what the newer version adds (for example teams) on the
// next start, the same way an interrupted load is completed.
package demomode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/authz"
	"github.com/orglane/orglane/core/internal/identity"
	"github.com/orglane/orglane/core/internal/store"
	"github.com/orglane/orglane/demo"
)

// SettingKey is the instance setting marking a demo database.
const SettingKey = "demo"

// Marker states.
const (
	stateLoading = "loading"
	stateLoaded  = "loaded"
)

// lockID serializes demo preparation between instances starting together.
const lockID int64 = 0x6f72676c64656d6f // "orgldemo"

// Actor is the audit actor of the dataset load.
var Actor = audit.Actor{Kind: audit.ActorSystem, UserAgent: "orglane-demo"}

// ErrNotEmpty is returned when demo mode is enabled on a database that
// already holds data that is not from the demo dataset.
var ErrNotEmpty = errors.New("demo mode needs an empty database: this one already has people that were not loaded as a demo. Use a new database for demo mode")

// ErrOtherDataset is returned when the database holds another demo dataset.
var ErrOtherDataset = errors.New("the database holds a different demo dataset")

// Marker is the value of the "demo" instance setting.
type Marker struct {
	Dataset string `json:"dataset"`
	Version int    `json:"version"`
	State   string `json:"state"`
}

// Result describes what Prepare did.
type Result struct {
	// Loaded is true when this call loaded (or finished loading) the dataset;
	// false when it was loaded on an earlier start.
	Loaded bool
	// People is how many people the dataset has.
	People int
	// Teams is how many teams the dataset has.
	Teams int
}

// Prepare makes the database a demo of dataset d: on an empty database it
// loads the dataset; on a database loaded earlier it does nothing (finishing
// an interrupted load, or completing a load from an older dataset version,
// if needed); on any other database it returns ErrNotEmpty. The built-in
// roles must be seeded first.
func Prepare(ctx context.Context, pool *pgxpool.Pool, svc *identity.Service, d *demo.Dataset) (Result, error) {
	marker, err := claim(ctx, pool, d)
	if err != nil {
		return Result{}, err
	}
	res := Result{People: len(d.People), Teams: len(d.Teams)}
	if current(marker, d) {
		return res, nil
	}
	if err := load(ctx, svc, d); err != nil {
		return Result{}, fmt.Errorf("demo: load dataset %q: %w", d.Name, err)
	}
	if err := finish(ctx, pool, d); err != nil {
		return Result{}, err
	}
	res.Loaded = true
	return res, nil
}

// IsDemo reports whether the database was loaded as a demo, and with which
// dataset.
func IsDemo(ctx context.Context, pool *pgxpool.Pool) (Marker, bool, error) {
	return readMarker(ctx, store.New(pool))
}

func readMarker(ctx context.Context, q *store.Queries) (Marker, bool, error) {
	raw, err := q.GetInstanceSetting(ctx, SettingKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return Marker{}, false, nil
	} else if err != nil {
		return Marker{}, false, fmt.Errorf("demo: load marker: %w", err)
	}
	var m Marker
	if err := json.Unmarshal(raw, &m); err != nil {
		return Marker{}, false, fmt.Errorf("demo: decode marker: %w", err)
	}
	return m, true, nil
}

func putMarker(ctx context.Context, q *store.Queries, m Marker) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if err := q.PutInstanceSetting(ctx, store.PutInstanceSettingParams{Key: SettingKey, Value: raw}); err != nil {
		return fmt.Errorf("demo: store marker: %w", err)
	}
	return nil
}

// claim returns the marker of a demo database, or marks an empty database
// as loading. Other databases are refused.
func claim(ctx context.Context, pool *pgxpool.Pool, d *demo.Dataset) (Marker, error) {
	var marker Marker
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		if err := q.AcquireXactLock(ctx, lockID); err != nil {
			return fmt.Errorf("demo: lock: %w", err)
		}
		m, ok, err := readMarker(ctx, q)
		if err != nil {
			return err
		}
		if ok {
			if m.Dataset != d.Name {
				return fmt.Errorf("%w: %q, not %q", ErrOtherDataset, m.Dataset, d.Name)
			}
			marker = m
			return nil
		}
		n, err := q.CountPeople(ctx)
		if err != nil {
			return fmt.Errorf("demo: count people: %w", err)
		}
		if n > 0 {
			return ErrNotEmpty
		}
		marker = Marker{Dataset: d.Name, Version: d.Version, State: stateLoading}
		return putMarker(ctx, q, marker)
	})
	return marker, err
}

func finish(ctx context.Context, pool *pgxpool.Pool, d *demo.Dataset) error {
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		if err := q.AcquireXactLock(ctx, lockID); err != nil {
			return fmt.Errorf("demo: lock: %w", err)
		}
		m, ok, err := readMarker(ctx, q)
		if err != nil {
			return err
		}
		if ok && current(m, d) {
			return nil // finished by a concurrent start
		}
		if err := putMarker(ctx, q, Marker{Dataset: d.Name, Version: d.Version, State: stateLoaded}); err != nil {
			return err
		}
		_, err = audit.Record(ctx, q, audit.Entry{
			Actor:    Actor,
			Action:   audit.InstanceDemoLoaded,
			Metadata: map[string]any{"dataset": d.Name, "version": d.Version, "people": len(d.People), "teams": len(d.Teams)},
		})
		return err
	})
}

// current reports whether the marker says dataset d is fully loaded.
func current(m Marker, d *demo.Dataset) bool {
	return m.State == stateLoaded && m.Version >= d.Version
}

// load creates what is missing of the dataset, so it can resume an
// interrupted load or complete a load from an older dataset version: people
// and teams that exist are kept, roles and team memberships are set
// idempotently and avatars are set only where none exists. Archived teams
// are left alone.
func load(ctx context.Context, svc *identity.Service, d *demo.Dataset) error {
	ids := make(map[string]uuid.UUID, len(d.People))
	for _, p := range d.People {
		person, err := svc.GetPersonByUsername(ctx, p.Username)
		if errors.Is(err, identity.ErrNotFound) {
			person, err = svc.CreatePerson(ctx, Actor, identity.NewPerson{
				Username:    p.Username,
				WorkEmail:   p.WorkEmail,
				DisplayName: p.DisplayName,
				Title:       p.Title,
				Status:      identity.StatusActive,
			})
			if errors.Is(err, identity.ErrUsernameTaken) {
				// Created by a concurrent start in the meantime.
				person, err = svc.GetPersonByUsername(ctx, p.Username)
			}
		}
		if err != nil {
			return fmt.Errorf("person %s: %w", p.Username, err)
		}
		ids[p.Username] = person.ID
		if err := svc.AssignRole(ctx, Actor, person.ID, authz.RoleKey(p.Role)); err != nil {
			return fmt.Errorf("person %s: role %s: %w", p.Username, p.Role, err)
		}
		data, contentType, ok, err := d.Avatar(p)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if _, err := svc.GetAvatar(ctx, person.ID); err == nil {
			continue
		} else if !errors.Is(err, identity.ErrNotFound) {
			return err
		}
		if err := svc.SetAvatar(ctx, Actor, person.ID, identity.Avatar{ContentType: contentType, Data: data}); err != nil {
			return fmt.Errorf("person %s: avatar: %w", p.Username, err)
		}
	}
	for _, t := range d.Teams {
		if err := loadTeam(ctx, svc, t, ids); err != nil {
			return fmt.Errorf("team %s: %w", t.Slug, err)
		}
	}
	return nil
}

func loadTeam(ctx context.Context, svc *identity.Service, t demo.Team, ids map[string]uuid.UUID) error {
	team, err := svc.GetTeamBySlug(ctx, t.Slug)
	if errors.Is(err, identity.ErrNotFound) {
		team, err = svc.CreateTeam(ctx, Actor, identity.NewTeam{Slug: t.Slug, Name: t.Name, Description: t.Description})
		if errors.Is(err, identity.ErrSlugTaken) {
			// Created by a concurrent start in the meantime.
			team, err = svc.GetTeamBySlug(ctx, t.Slug)
		}
	}
	if err != nil {
		return err
	}
	if team.ArchivedAt != nil {
		return nil
	}
	set := func(usernames []string, role identity.TeamRole) error {
		for _, u := range usernames {
			_, err := svc.SetTeamMember(ctx, Actor, team.ID, ids[u], role)
			if errors.Is(err, identity.ErrPersonOffboarded) {
				continue // offboarded in the demo in the meantime
			}
			if err != nil {
				return fmt.Errorf("%s %s: %w", role, u, err)
			}
		}
		return nil
	}
	if err := set(t.Leads, identity.TeamRoleLead); err != nil {
		return err
	}
	return set(t.Members, identity.TeamRoleMember)
}
