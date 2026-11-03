package peopleimport

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestFixtureFiles(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		rows                      int
		encoding, delimiter, code string
		bad                       bool
	}{
		{name: "good.csv", rows: 3, encoding: "UTF-8", delimiter: ","},
		{name: "messy.csv", rows: 2, encoding: "Windows-1252", delimiter: ";", code: "column_count"},
		{name: "good.xlsx", rows: 3, encoding: "UTF-8"},
		{name: "formula.xlsx", rows: 3, encoding: "UTF-8", code: "formula"},
		{name: "fifty.csv", rows: 50, encoding: "UTF-8", delimiter: ","},
		{name: "malformed.csv", bad: true}, {name: "zip-bomb.xlsx", bad: true}, {name: "zip-traversal.xlsx", bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, err := os.Open("testdata/" + tc.name)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = f.Close() }()
			out, err := (FileSource{}).Read(f)
			if tc.bad {
				if !errors.Is(err, ErrFile) {
					t.Fatalf("error=%v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(out.Rows) != tc.rows || out.Encoding != tc.encoding || out.Delimiter != tc.delimiter {
				t.Fatalf("metadata=%+v", out)
			}
			if tc.code != "" {
				found := false
				for _, r := range out.Rows {
					if r.Error == tc.code {
						found = true
					}
				}
				if !found {
					t.Fatalf("missing %s", tc.code)
				}
			}
			if tc.name == "formula.xlsx" && out.Rows[0].Cells[0] != "" {
				t.Fatal("formula cached value was trusted")
			}
		})
	}
}

func TestSourceLimitsAndEncodings(t *testing.T) {
	utf := []byte{0xff, 0xfe}
	for _, r := range utf16.Encode([]rune("username\twork_email\nzoe\tzoe@example.test\n")) {
		utf = binary.LittleEndian.AppendUint16(utf, r)
	}
	out, err := (FileSource{}).Read(bytes.NewReader(utf))
	if err != nil || out.Encoding != "UTF-16LE" || out.Delimiter != "\t" {
		t.Fatalf("UTF16 metadata: %+v, %v", out, err)
	}
	for _, input := range []string{strings.Repeat("x", MaxUploadBytes+1), "a\n" + strings.Repeat("x\n", MaxRows+1), strings.Repeat("a,", MaxColumns) + "a\nx\n", "a\n\x00\n"} {
		if _, err := (FileSource{}).Read(strings.NewReader(input)); !errors.Is(err, ErrFile) {
			t.Fatalf("oversized/malformed input: %v", err)
		}
	}
	out, err = (FileSource{}).Read(strings.NewReader("title,name\n\"comma, value\",Ada\n"))
	if err != nil || out.Rows[0].Cells[0] != "comma, value" {
		t.Fatalf("quoted delimiter: %v", err)
	}
}

func TestSafeCSVAndValueFreeExport(t *testing.T) {
	for _, s := range []string{"=CMD()", " +123", "-1", "@SUM(1)", "\tformula", "line\nbreak"} {
		if got := SafeCSVCell(s); !strings.HasPrefix(got, "'") {
			t.Errorf("unsafe %q", got)
		}
	}
	if SafeCSVCell("ordinary") != "ordinary" {
		t.Fatal("ordinary value changed")
	}
	got := ErrorsCSV(Run{Rows: []Row{{Row: 2, Decision: "conflict", Error: "invalid_fields", Result: "failed"}, {Row: 3, Decision: "create", Result: "created"}}})
	if got != "row,decision,error,result\n2,conflict,invalid_fields,failed\n" {
		t.Fatalf("export=%q", got)
	}
}

func TestCSVPhysicalStartLines(t *testing.T) {
	fixture, err := os.ReadFile("testdata/physical-lines.csv")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, text string }{
		{name: "LF", text: string(fixture)},
		{name: "CRLF", text: strings.ReplaceAll(string(fixture), "\n", "\r\n")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := (FileSource{}).Read(strings.NewReader(tc.text))
			if err != nil {
				t.Fatal(err)
			}
			if len(data.Rows) != 3 {
				t.Fatalf("records=%d", len(data.Rows))
			}
			for i, line := range []int{3, 6, 8} {
				if data.Rows[i].Number != line {
					t.Fatalf("record%d starts at%d,want physical line%d", i, data.Rows[i].Number, line)
				}
			}
			if data.Rows[0].Cells[2] != "Alpha\nExample" || data.Rows[1].Error != "column_count" {
				t.Fatalf("multiline/error parsing changed: %+v", data.Rows)
			}
		})
	}
}

func TestCSVPhysicalLineLimit(t *testing.T) {
	input := "username\n" + strings.Repeat("\n", 1048576) + "someone\n"
	if _, err := (FileSource{}).Read(strings.NewReader(input)); !errors.Is(err, ErrFile) {
		t.Fatalf("oversized source line number: %v", err)
	}
}
