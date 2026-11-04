package apps

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"

	"github.com/orglane/orglane/core/internal/store"
)

// DirectoryChanged queues provisioning for everyone a directory change
// affects, in every provisionable instance, in the transaction of the
// change (the transactional outbox). q must be bound to that transaction.
// Every member of the given teams is affected (an archived team, for
// example). It implements identity.DirectoryObserver.
func (s *Service) DirectoryChanged(ctx context.Context, q *store.Queries, changedPeople, teams []uuid.UUID) error {
	rows, err := q.ListAppInstancesInState(ctx, provisionable)
	if err != nil {
		return fmt.Errorf("apps: list instances: %w", err)
	}
	var instances []uuid.UUID
	for _, row := range rows {
		if a, ok := s.reg.Adapter(providerID(row.Provider)); ok {
			if _, ok := a.Provisioner(); ok {
				instances = append(instances, row.ID)
			}
		}
	}
	if len(instances) == 0 {
		return nil
	}
	tx, ok := q.Tx()
	if !ok {
		return errors.New("apps: directory changes must be reported inside their transaction")
	}
	people := slices.Clone(changedPeople)
	for _, team := range teams {
		members, err := q.ListTeamMemberIDs(ctx, team)
		if err != nil {
			return fmt.Errorf("apps: list team members: %w", err)
		}
		people = append(people, members...)
	}
	slices.SortFunc(people, compareUUID)
	people = slices.Compact(people)
	for _, inst := range instances {
		for _, p := range people {
			if err := s.enqueuePerson(ctx, tx, q, inst, p, false); err != nil {
				return err
			}
		}
	}
	return nil
}

func compareUUID(a, b uuid.UUID) int {
	for i := range a {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}
