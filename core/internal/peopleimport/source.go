// Package peopleimport implements the staged people import pipeline (ADR 0015).
// Sources return bounded untrusted records; no source may mutate identity state.
package peopleimport

import (
	"bytes"
	"encoding/csv"
	"errors"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"
	textunicode "golang.org/x/text/encoding/unicode"
)

// Limits bound uploads before parsing, and both sparse and expanded workbooks.
const (
	MaxUploadBytes   = 5 << 20
	MaxExpandedBytes = 32 << 20
	MaxZIPEntries    = 100
	MaxRows          = 1000
	MaxColumns       = 100
	MaxCellBytes     = 4096
)

// ErrFile intentionally never embeds parser errors or source values.
var ErrFile = errors.New("import: unsupported, malformed or oversized file")

// Source is the adapter contract for CSV/XLSX and future directory sources.
// Implementations must honor the limits and return fixed row error codes.
type Source interface {
	Key() string
	Read(io.Reader) (Dataset, error)
}

// Dataset contains the first row's headings and records, all still untrusted.
type Dataset struct {
	Headers   []string
	Rows      []Record
	Encoding  string
	Delimiter string
}

// Record holds one source row; Number is its original CSV start line or XLSX row.
type Record struct {
	Number int
	Cells  []string
	Error  string
}

// FileSource detects XLSX by ZIP magic, otherwise CSV.
type FileSource struct{}

// Key is the stable namespace for file external IDs across CSV/XLSX reimports.
func (FileSource) Key() string { return "file" }

// Read bounds the raw bytes before decoding either format.
func (FileSource) Read(r io.Reader) (Dataset, error) {
	b, err := io.ReadAll(io.LimitReader(r, MaxUploadBytes+1))
	if err != nil || len(b) == 0 || len(b) > MaxUploadBytes {
		return Dataset{}, ErrFile
	}
	if bytes.HasPrefix(b, []byte{'P', 'K', 3, 4}) {
		return readXLSX(b)
	}
	return readCSV(b)
}

func readCSV(b []byte) (Dataset, error) {
	enc := "UTF-8"
	var err error
	switch {
	case bytes.HasPrefix(b, []byte{0xff, 0xfe}):
		b, err = textunicode.UTF16(textunicode.LittleEndian, textunicode.ExpectBOM).NewDecoder().Bytes(b)
		enc = "UTF-16LE"
	case bytes.HasPrefix(b, []byte{0xfe, 0xff}):
		b, err = textunicode.UTF16(textunicode.BigEndian, textunicode.ExpectBOM).NewDecoder().Bytes(b)
		enc = "UTF-16BE"
	case bytes.HasPrefix(b, []byte{0xef, 0xbb, 0xbf}):
		b = b[3:]
	case !utf8.Valid(b):
		b, err = charmap.Windows1252.NewDecoder().Bytes(b)
		enc = "Windows-1252"
	}
	if err != nil || !utf8.Valid(b) || bytes.IndexByte(b, 0) >= 0 {
		return Dataset{}, ErrFile
	}
	// Parse candidate records rather than counting separator characters inside quotes.
	delim, score := ',', -1
	for _, candidate := range []rune{',', ';', '\t', '|'} {
		r := csv.NewReader(bytes.NewReader(b))
		r.Comma = candidate
		r.FieldsPerRecord = -1
		header, e := r.Read()
		if len(header) > MaxColumns {
			return Dataset{}, ErrFile
		}
		if e != nil {
			continue
		}
		n := len(header) * 10
		for i := 0; i < 10; i++ {
			row, e := r.Read()
			if e == io.EOF {
				break
			}
			if e != nil {
				n = -1
				break
			}
			if len(row) == len(header) {
				n++
			}
		}
		if n > score {
			delim, score = candidate, n
		}
	}
	if score < 0 {
		return Dataset{}, ErrFile
	}
	r := csv.NewReader(bytes.NewReader(b))
	r.Comma = delim
	r.FieldsPerRecord = -1
	headers, err := r.Read()
	if err != nil || !validCells(headers) || len(headers) == 0 {
		return Dataset{}, ErrFile
	}
	out := Dataset{Headers: headers, Encoding: enc, Delimiter: string(delim)}
	for {
		row, e := r.Read()
		if e == io.EOF {
			break
		}
		if e != nil {
			return Dataset{}, ErrFile
		}
		if len(out.Rows) >= MaxRows || len(row) > MaxColumns {
			return Dataset{}, ErrFile
		}
		line, _ := r.FieldPos(0)
		if line > 1048576 {
			return Dataset{}, ErrFile
		}
		rec := Record{Number: line, Cells: row}
		if len(row) != len(headers) {
			rec.Error = "column_count"
		}
		if !validCells(row) {
			rec.Error = "cell_limit"
			rec.Cells = nil
		}
		out.Rows = append(out.Rows, rec)
	}
	if len(out.Rows) == 0 {
		return Dataset{}, ErrFile
	}
	return out, nil
}

func validCells(cells []string) bool {
	if len(cells) > MaxColumns {
		return false
	}
	for _, c := range cells {
		if len(c) > MaxCellBytes || !utf8.ValidString(c) || strings.ContainsRune(c, 0) {
			return false
		}
	}
	return true
}

// SafeCSVCell prevents spreadsheet execution if this helper is later used for
// value-bearing reports. Current error exports contain only fixed codes/numbers.
func SafeCSVCell(s string) string {
	v := strings.TrimLeftFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) })
	if strings.ContainsAny(s, "\t\r\n") || (len(v) > 0 && strings.ContainsRune("=+-@", rune(v[0]))) {
		return "'" + s
	}
	return s
}
