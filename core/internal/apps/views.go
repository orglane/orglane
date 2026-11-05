package apps

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/orglane/orglane/core/internal/store"
	"github.com/orglane/orglane/providers/provider"
)

func providerID(s string) provider.ID { return provider.ID(s) }

// InstanceDetail is what the instance page shows besides the instance.
type InstanceDetail struct {
	Bindings            []store.AppBinding
	Operations          []Operation
	Credentials         []Credential
	CredentialRotations []store.ListAppCredentialRotationsRow
	CredentialDrift     []store.ListAppCredentialDriftRow
	UnmanagedCount      int64
	StatusCounts        map[string]int64
}

// Detail returns an instance's bindings, last operations, credential
// versions (without values), unmanaged account count and result counts.
func (s *Service) Detail(ctx context.Context, id uuid.UUID) (InstanceDetail, error) {
	q := store.New(s.pool)
	var d InstanceDetail
	var err error
	if d.Bindings, err = q.ListAppInstanceBindings(ctx, id); err != nil {
		return d, fmt.Errorf("apps: list bindings: %w", err)
	}
	if d.Operations, err = s.Operations(ctx, id, 10, 0); err != nil {
		return d, err
	}
	if d.Credentials, err = q.ListAppCredentials(ctx, id); err != nil {
		return d, fmt.Errorf("apps: list credentials: %w", err)
	}
	if d.CredentialRotations, err = q.ListAppCredentialRotations(ctx, store.ListAppCredentialRotationsParams{InstanceID: id, TargetID: ptr(id.String())}); err != nil {
		return d, fmt.Errorf("apps: list rotation outcomes: %w", err)
	}
	if d.CredentialDrift, err = q.ListAppCredentialDrift(ctx, id); err != nil {
		return d, fmt.Errorf("apps: list credential drift: %w", err)
	}
	if d.UnmanagedCount, err = q.CountUnmanagedAccounts(ctx, id); err != nil {
		return d, fmt.Errorf("apps: count unmanaged accounts: %w", err)
	}
	counts, err := q.CountProvisioningResults(ctx, id)
	if err != nil {
		return d, fmt.Errorf("apps: count results: %w", err)
	}
	d.StatusCounts = map[string]int64{}
	for _, c := range counts {
		d.StatusCounts[c.Status] = c.N
	}
	return d, nil
}

// Operation is an agent operation with the reason of its desired spec
// generation, if it pushed one.
type Operation = store.ListAppOperationHistoryRow

// Operations returns a page of an instance's operations, newest first.
func (s *Service) Operations(ctx context.Context, instanceID uuid.UUID, limit, offset int32) ([]Operation, error) {
	rows, err := store.New(s.pool).ListAppOperationHistory(ctx, store.ListAppOperationHistoryParams{InstanceID: instanceID, RowLimit: limit, RowOffset: offset})
	if err != nil {
		return nil, fmt.Errorf("apps: list operations: %w", err)
	}
	return rows, nil
}

// Results returns a page of an instance's provisioning results.
func (s *Service) Results(ctx context.Context, instanceID uuid.UUID, limit, offset int32) ([]store.ListProvisioningResultsRow, error) {
	q := store.New(s.pool)
	if _, err := s.loadInstance(ctx, q, instanceID, false); err != nil {
		return nil, err
	}
	rows, err := q.ListProvisioningResults(ctx, store.ListProvisioningResultsParams{InstanceID: instanceID, RowLimit: limit, RowOffset: offset})
	if err != nil {
		return nil, fmt.Errorf("apps: list results: %w", err)
	}
	return rows, nil
}

// UnmanagedAccounts returns an instance's unmanaged accounts report.
func (s *Service) UnmanagedAccounts(ctx context.Context, instanceID uuid.UUID) ([]store.AppUnmanagedAccount, error) {
	rows, err := store.New(s.pool).ListUnmanagedAccounts(ctx, instanceID)
	if err != nil {
		return nil, fmt.Errorf("apps: list unmanaged accounts: %w", err)
	}
	return rows, nil
}

// PersonApps is a person's provisioning results and links.
type PersonApps struct {
	Results []store.ListPersonProvisioningResultsRow
	Links   []store.AccountLink
	Pending []store.ListPersonPendingAccountLinksWithConflictRow
}

// ForPerson returns a person's results in every instance, their verified
// links and their unverified import evidence.
func (s *Service) ForPerson(ctx context.Context, personID uuid.UUID) (PersonApps, error) {
	q := store.New(s.pool)
	var out PersonApps
	var err error
	if out.Results, err = q.ListPersonProvisioningResults(ctx, personID); err != nil {
		return out, fmt.Errorf("apps: list results: %w", err)
	}
	if out.Links, err = q.ListPersonAccountLinks(ctx, personID); err != nil {
		return out, fmt.Errorf("apps: list links: %w", err)
	}
	if out.Pending, err = q.ListPersonPendingAccountLinksWithConflict(ctx, personID); err != nil {
		return out, fmt.Errorf("apps: list pending links: %w", err)
	}
	return out, nil
}

// MyApp is an instance a person has access to.
type MyApp struct {
	Instance Instance
	Status   string
	Step     string
}

// MyApps returns the provisionable instances in which the person has
// desired access, with their result status.
func (s *Service) MyApps(ctx context.Context, personID uuid.UUID) ([]MyApp, error) {
	q := store.New(s.pool)
	rows, err := q.ListPersonProvisioningResults(ctx, personID)
	if err != nil {
		return nil, fmt.Errorf("apps: list results: %w", err)
	}
	out := []MyApp{}
	for _, r := range rows {
		if !r.Access {
			continue
		}
		inst, err := s.loadInstance(ctx, q, r.InstanceID, false)
		if err != nil {
			return nil, err
		}
		if !inst.Provisionable() {
			continue
		}
		out = append(out, MyApp{Instance: inst, Status: r.Status, Step: derefString(r.EmployeeStep)})
	}
	return out, nil
}
