package packages

import (
	"cmp"
	"fmt"
	"regexp"
	"strconv"
)

// Version is a package version "<upstream>-<revision>": the upstream
// version as MAJOR.MINOR.PATCH and the package revision, which changes when
// only the package changes (docs/specs/agent.md "Manifest").
type Version struct {
	Major, Minor, Patch, Revision int
}

var versionPattern = regexp.MustCompile(`^(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})-([1-9][0-9]{0,8})$`)

// ParseVersion parses a package version.
func ParseVersion(s string) (Version, error) {
	m := versionPattern.FindStringSubmatch(s)
	if m == nil {
		return Version{}, fmt.Errorf("invalid package version %q: want <major>.<minor>.<patch>-<revision>", s)
	}
	var v Version
	for i, p := range []*int{&v.Major, &v.Minor, &v.Patch, &v.Revision} {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return Version{}, fmt.Errorf("invalid package version %q", s)
		}
		*p = n
	}
	return v, nil
}

func (v Version) String() string {
	return fmt.Sprintf("%d.%d.%d-%d", v.Major, v.Minor, v.Patch, v.Revision)
}

// Compare compares by upstream version, then revision.
func (v Version) Compare(o Version) int {
	return cmp.Or(
		cmp.Compare(v.Major, o.Major),
		cmp.Compare(v.Minor, o.Minor),
		cmp.Compare(v.Patch, o.Patch),
		cmp.Compare(v.Revision, o.Revision),
	)
}
