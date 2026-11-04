//go:build linux

package agent

import (
	"bufio"
	"bytes"
	"os"
	"strconv"
	"strings"
)

// memory returns the host's total and available memory from /proc/meminfo.
func memory() (total, available uint64) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	s := bufio.NewScanner(bytes.NewReader(data))
	for s.Scan() {
		name, rest, ok := strings.Cut(s.Text(), ":")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		kib, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		switch name {
		case "MemTotal":
			total = kib * 1024
		case "MemAvailable":
			available = kib * 1024
		}
	}
	return total, available
}
