package apps

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/orglane/orglane/core/internal/agentapi"
	"github.com/orglane/orglane/core/internal/agentclient"
)

func TestClassifyAgentError(t *testing.T) {
	problem := func(status int, code string) error {
		return &agentclient.ProblemError{Problem: agentapi.Problem{Status: status, Code: code}}
	}
	for _, c := range []struct {
		err  error
		code string
		kind agentErrorKind
	}{
		{fmt.Errorf("%w: dial", agentclient.ErrUnavailable), CodeAgentUnavailable, agentWaiting},
		{agentclient.ErrUntrusted, CodeAgentUntrusted, agentWaiting},
		{fmt.Errorf("%w: core 1, agent 2", agentclient.ErrVersionMismatch), CodeAgentVersionMismatch, agentWaiting},
		{agentclient.ErrProtocol, CodeAgentVersionMismatch, agentWaiting},
		{problem(http.StatusInternalServerError, agentapi.CodeInternal), agentapi.CodeInternal, agentRetry},
		{problem(http.StatusConflict, agentapi.CodeAgentBusy), agentapi.CodeAgentBusy, agentRetry},
		{problem(http.StatusConflict, agentapi.CodeInstanceMismatch), agentapi.CodeInstanceMismatch, agentRefused},
		{problem(http.StatusUnprocessableEntity, agentapi.CodeInvalidInput), agentapi.CodeInvalidInput, agentRefused},
		{problem(http.StatusConflict, agentapi.CodeStaleGeneration), agentapi.CodeStaleGeneration, agentRefused},
		{errors.New("context deadline exceeded"), "", agentRetry},
	} {
		code, kind := classifyAgentError(c.err)
		if code != c.code || kind != c.kind {
			t.Errorf("%v: got %q %d, want %q %d", c.err, code, kind, c.code, c.kind)
		}
	}
}

func TestAgentBackoff(t *testing.T) {
	s := &Service{cfg: Config{AgentRetryMax: 30 * time.Second}}
	want := map[int]time.Duration{0: time.Second, 1: time.Second, 2: 2 * time.Second, 5: 16 * time.Second, 6: 30 * time.Second, 25: 30 * time.Second, 10000: 30 * time.Second}
	for attempt, d := range want {
		if got := s.AgentBackoff(attempt); got != d {
			t.Errorf("attempt %d: %v, want %v", attempt, got, d)
		}
	}
}

func TestContainerHealth(t *testing.T) {
	running := func(svcs ...agentapi.ServiceStatus) *agentapi.InstanceStatus {
		return &agentapi.InstanceStatus{Phase: agentapi.PhaseRunning, Services: svcs}
	}
	for name, c := range map[string]struct {
		st           *agentapi.InstanceStatus
		health, code string
	}{
		"missing":    {nil, HealthUnknown, CodeAgentInstanceMissing},
		"healthy":    {running(agentapi.ServiceStatus{Name: "web", State: "running", Health: "healthy"}, agentapi.ServiceStatus{Name: "worker", State: "running"}), HealthHealthy, ""},
		"unhealthy":  {running(agentapi.ServiceStatus{Name: "web", State: "running", Health: "unhealthy"}), HealthUnhealthy, CodeAgentUnhealthy},
		"exited":     {running(agentapi.ServiceStatus{Name: "web", State: "exited"}), HealthUnhealthy, CodeAgentUnhealthy},
		"degraded":   {&agentapi.InstanceStatus{Phase: agentapi.PhaseDegraded}, HealthUnhealthy, CodeAgentUnhealthy},
		"failed":     {&agentapi.InstanceStatus{Phase: agentapi.PhaseFailed}, HealthUnhealthy, CodeAgentUnhealthy},
		"installing": {&agentapi.InstanceStatus{Phase: agentapi.PhaseInstalling}, HealthUnknown, ""},
		"stopped":    {&agentapi.InstanceStatus{Phase: agentapi.PhaseStopped}, HealthUnknown, ""},
	} {
		health, code := containerHealth(c.st)
		if health != c.health || code != c.code {
			t.Errorf("%s: %s %q, want %s %q", name, health, code, c.health, c.code)
		}
	}
}

func TestNoteBoot(t *testing.T) {
	s := &Service{}
	a, b := [16]byte{1}, [16]byte{2}
	if s.noteBoot(a) {
		t.Fatal("the first boot ID counts as a change")
	}
	if s.noteBoot(a) {
		t.Fatal("the same boot ID counts as a change")
	}
	if !s.noteBoot(b) {
		t.Fatal("a new boot ID does not count as a change")
	}
}
