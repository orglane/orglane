package forgejo

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestFoldLabelCaseFolding checks full case folding for every Unicode
// scalar value against the common and full mappings in the Unicode 17.0.0
// CaseFolding.txt, the data the pinned golang.org/x/text tables are built
// from. The file is an independent oracle: a code point folds to its
// listed mapping, or to itself when it is not listed. The S and T statuses
// are simple and Turkic mappings, which full case folding does not use.
func TestFoldLabelCaseFolding(t *testing.T) {
	folds := readCaseFolding(t, "testdata/CaseFolding-17.0.0.txt")
	mismatches := 0
	for r := rune(0); r <= utf8.MaxRune; r++ {
		if r >= 0xD800 && r <= 0xDFFF {
			continue // not Unicode scalar values
		}
		want, ok := folds[r]
		if !ok {
			want = string(r)
		}
		if got := foldLabel(string(r)); got != want {
			mismatches++
			if mismatches <= 20 {
				t.Errorf("foldLabel(%U) = %q, want %q", r, got, want)
			}
		}
	}
	if mismatches > 20 {
		t.Errorf("%d code points do not fold as CaseFolding.txt says (first 20 shown)", mismatches)
	}
}

func readCaseFolding(t *testing.T, path string) map[rune]string {
	t.Helper()
	f, err := os.Open(path) //nolint:gosec // fixed test data
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	folds := make(map[rune]string, 1600)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Split(line, ";")
		if len(fields) != 4 {
			t.Fatalf("CaseFolding.txt: %q: %d fields, want 4", scanner.Text(), len(fields))
		}
		status := strings.TrimSpace(fields[1])
		if status != "C" && status != "F" {
			continue
		}
		code, err := strconv.ParseUint(strings.TrimSpace(fields[0]), 16, 32)
		if err != nil {
			t.Fatalf("CaseFolding.txt: %q: %v", scanner.Text(), err)
		}
		if code > utf8.MaxRune {
			t.Fatalf("CaseFolding.txt: %q: %U is not a Unicode code point", scanner.Text(), code)
		}
		r := rune(code) //nolint:gosec // code is at most utf8.MaxRune
		if _, ok := folds[r]; ok {
			t.Fatalf("CaseFolding.txt: %U has more than one common or full mapping", r)
		}
		var b strings.Builder
		for _, field := range strings.Fields(fields[2]) {
			mapped, err := strconv.ParseUint(field, 16, 32)
			if err != nil {
				t.Fatalf("CaseFolding.txt: %q: %v", scanner.Text(), err)
			}
			if mapped > utf8.MaxRune {
				t.Fatalf("CaseFolding.txt: %q: %U is not a Unicode code point", scanner.Text(), mapped)
			}
			b.WriteRune(rune(mapped)) //nolint:gosec // mapped is at most utf8.MaxRune
		}
		folds[r] = b.String()
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return folds
}
