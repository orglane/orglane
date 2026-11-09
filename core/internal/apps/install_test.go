package apps

import (
	"errors"
	"testing"

	"github.com/orglane/orglane/core/internal/packages"
)

func TestCheckResources(t *testing.T) {
	pkg := &packages.Package{Manifest: &packages.Manifest{}}
	pkg.Manifest.Resources.Memory.Min = 256 << 20
	pkg.Manifest.Resources.Disk.Min = 2 << 30
	for name, c := range map[string]struct {
		st   AgentStatus
		code string
	}{
		"fits":         {AgentStatus{MemTotal: 2 << 30, MemoryReserved: 1 << 30, DiskFree: 10 << 30}, ""},
		"exactly fits": {AgentStatus{MemTotal: 1<<30 + 256<<20, MemoryReserved: 1 << 30, DiskFree: 2 << 30}, ""},
		"memory":       {AgentStatus{MemTotal: 1<<30 + 255<<20, MemoryReserved: 1 << 30, DiskFree: 10 << 30}, CodeInsufficientMemory},
		"disk":         {AgentStatus{MemTotal: 8 << 30, MemoryReserved: 1 << 30, DiskFree: 2<<30 - 1}, CodeInsufficientDisk},
	} {
		err := checkResources(c.st, pkg)
		var coded *CodedError
		switch {
		case c.code == "" && err != nil:
			t.Errorf("%s: %v", name, err)
		case c.code != "" && (!errors.As(err, &coded) || coded.Code != c.code || Code(err) != c.code):
			t.Errorf("%s: %v, want %s", name, err, c.code)
		}
	}
}
