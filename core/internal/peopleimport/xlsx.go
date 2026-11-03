package peopleimport

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"path"
	"strconv"
	"strings"
)

// XML is streamed, including cells and rich text. A small raw document must
// not allocate an unbounded slice of empty rows/cells/runs before limits apply.
type safeXML struct {
	decoder       *xml.Decoder
	depth, tokens int
}

func newXML(b []byte) *safeXML { return &safeXML{decoder: xml.NewDecoder(bytes.NewReader(b))} }
func (d *safeXML) next() (xml.Token, error) {
	tok, err := d.decoder.Token()
	if err != nil {
		return nil, err
	}
	d.tokens++
	if d.tokens > 2_100_000 {
		return nil, ErrFile
	}
	switch t := tok.(type) {
	case xml.StartElement:
		d.depth++
		if d.depth > 64 || len(t.Attr) > 100 {
			return nil, ErrFile
		}
	case xml.EndElement:
		d.depth--
		if d.depth < 0 {
			return nil, ErrFile
		}
	case xml.Directive:
		return nil, ErrFile // DTDs and custom entities are unnecessary.
	}
	return tok, nil
}
func attr(start xml.StartElement, name string) string {
	for _, a := range start.Attr {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}
func (d *safeXML) skip() error {
	end := d.depth - 1
	for d.depth > end {
		if _, err := d.next(); err != nil {
			return err
		}
	}
	return nil
}

// text collects only <t> or <v> text within an already opened element.
func (d *safeXML) text() (string, error) {
	end := d.depth - 1
	capture := false
	var out strings.Builder
	for d.depth > end {
		tok, err := d.next()
		if err != nil {
			return "", err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "t" || t.Name.Local == "v" {
				capture = true
			}
		case xml.EndElement:
			if t.Name.Local == "t" || t.Name.Local == "v" {
				capture = false
			}
		case xml.CharData:
			if capture {
				if out.Len()+len(t) > MaxCellBytes {
					return "", ErrFile
				}
				out.Write(t)
			}
		}
	}
	return out.String(), nil
}
func firstSheet(files map[string][]byte) (string, error) {
	d := newXML(files["xl/workbook.xml"])
	id := ""
	count := 0
	for {
		tok, err := d.next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", ErrFile
		}
		if start, ok := tok.(xml.StartElement); ok && start.Name.Local == "sheet" {
			count++
			if count > MaxZIPEntries {
				return "", ErrFile
			}
			if id == "" {
				id = attr(start, "id")
			}
		}
	}
	if id == "" {
		return "", ErrFile
	}
	d = newXML(files["xl/_rels/workbook.xml.rels"])
	sheet := ""
	count = 0
	for {
		tok, err := d.next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", ErrFile
		}
		if start, ok := tok.(xml.StartElement); ok && start.Name.Local == "Relationship" {
			count++
			if count > MaxZIPEntries {
				return "", ErrFile
			}
			if attr(start, "Id") == id {
				if sheet != "" || attr(start, "TargetMode") == "External" || !strings.HasSuffix(attr(start, "Type"), "/worksheet") {
					return "", ErrFile
				}
				target := attr(start, "Target")
				if strings.Contains(target, "\\") || strings.Contains(target, "..") {
					return "", ErrFile
				}
				if strings.HasPrefix(target, "/xl/") {
					sheet = strings.TrimPrefix(target, "/")
				} else {
					sheet = path.Join("xl", target)
				}
			}
		}
	}
	if !strings.HasPrefix(sheet, "xl/worksheets/") || files[sheet] == nil {
		return "", ErrFile
	}
	return sheet, nil
}

func readXLSX(b []byte) (Dataset, error) {
	z, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil || len(z.File) > MaxZIPEntries {
		return Dataset{}, ErrFile
	}
	files := make(map[string][]byte, len(z.File))
	total := 0
	for _, f := range z.File {
		if f.Flags&1 != 0 || f.UncompressedSize64 > MaxExpandedBytes || strings.Contains(f.Name, "\\") || path.Clean(f.Name) != strings.TrimSuffix(f.Name, "/") || strings.HasPrefix(f.Name, "/") || strings.HasPrefix(f.Name, "../") {
			return Dataset{}, ErrFile
		}
		if f.FileInfo().IsDir() {
			continue
		}
		if _, exists := files[f.Name]; exists {
			return Dataset{}, ErrFile
		}
		r, e := f.Open()
		if e != nil {
			return Dataset{}, ErrFile
		}
		data, e := io.ReadAll(io.LimitReader(r, int64(MaxExpandedBytes-total+1)))
		closeErr := r.Close()
		total += len(data)
		if e != nil || closeErr != nil || total > MaxExpandedBytes {
			return Dataset{}, ErrFile
		}
		files[f.Name] = data
	}
	sheet, err := firstSheet(files)
	if err != nil {
		return Dataset{}, ErrFile
	}
	var shared []string
	if data, ok := files["xl/sharedStrings.xml"]; ok {
		d := newXML(data)
		for {
			tok, e := d.next()
			if errors.Is(e, io.EOF) {
				break
			}
			if e != nil {
				return Dataset{}, ErrFile
			}
			if start, ok := tok.(xml.StartElement); ok && start.Name.Local == "si" {
				if len(shared) >= MaxRows*MaxColumns+MaxColumns {
					return Dataset{}, ErrFile
				}
				value, e := d.text()
				if e != nil || !validCells([]string{value}) {
					return Dataset{}, ErrFile
				}
				shared = append(shared, value)
			}
		}
	}
	out := Dataset{Encoding: "UTF-8", Delimiter: ""}
	d := newXML(files[sheet])
	previous, count := 0, 0
	for {
		tok, e := d.next()
		if errors.Is(e, io.EOF) {
			break
		}
		if e != nil {
			return Dataset{}, ErrFile
		}
		start, ok := tok.(xml.StartElement)
		if !ok || start.Name.Local != "row" {
			continue
		}
		count++
		if count > MaxRows+1 {
			return Dataset{}, ErrFile
		}
		number := previous + 1
		if raw := attr(start, "r"); raw != "" {
			number, e = strconv.Atoi(raw)
			if e != nil {
				return Dataset{}, ErrFile
			}
		}
		if number <= previous || number > 1048576 {
			return Dataset{}, ErrFile
		}
		previous = number
		cells, code, e := readSheetRow(d, number, shared)
		if e != nil {
			return Dataset{}, ErrFile
		}
		if len(cells) == 0 {
			continue
		}
		if !validCells(cells) {
			code = "cell_limit"
			cells = nil
		}
		if out.Headers == nil {
			if code != "" {
				return Dataset{}, ErrFile
			}
			out.Headers = cells
			continue
		}
		if len(cells) > len(out.Headers) {
			code = "column_count"
		}
		for len(cells) < len(out.Headers) {
			cells = append(cells, "")
		}
		out.Rows = append(out.Rows, Record{Number: number, Cells: cells, Error: code})
	}
	if len(out.Rows) == 0 {
		return Dataset{}, ErrFile
	}
	return out, nil
}
func readSheetRow(d *safeXML, number int, shared []string) ([]string, string, error) {
	end := d.depth - 1
	cells := []string{}
	last, count := -1, 0
	code := ""
	for d.depth > end {
		tok, err := d.next()
		if err != nil {
			return nil, "", err
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		if start.Name.Local != "c" {
			if err = d.skip(); err != nil {
				return nil, "", err
			}
			continue
		}
		count++
		if count > MaxColumns {
			return nil, "", ErrFile
		}
		col := last + 1
		if ref := attr(start, "r"); ref != "" {
			n, i := 0, 0
			for i < len(ref) && ref[i] >= 'A' && ref[i] <= 'Z' {
				n = n*26 + int(ref[i]-'A'+1)
				if n > MaxColumns {
					return nil, "", ErrFile
				}
				i++
			}
			row, e := strconv.Atoi(ref[i:])
			if e != nil || row != number || n == 0 {
				return nil, "", ErrFile
			}
			col = n - 1
		}
		if col <= last || col >= MaxColumns {
			return nil, "", ErrFile
		}
		last = col
		for len(cells) <= col {
			cells = append(cells, "")
		}
		value, cellCode, err := readCell(d, attr(start, "t"), shared)
		if err != nil {
			return nil, "", err
		}
		if cellCode != "" {
			code = cellCode
		}
		cells[col] = value
	}
	return cells, code, nil
}
func readCell(d *safeXML, kind string, shared []string) (string, string, error) {
	end := d.depth - 1
	value, code := "", ""
	for d.depth > end {
		tok, err := d.next()
		if err != nil {
			return "", "", err
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch start.Name.Local {
		case "f":
			code = "formula"
			if err = d.skip(); err != nil {
				return "", "", err
			}
		case "v":
			// text expects the capture element to be nested; read the value's
			// character data directly while bounding it before allocation.
			var b strings.Builder
			level := d.depth - 1
			for d.depth > level {
				t, e := d.next()
				if e != nil {
					return "", "", e
				}
				if c, ok := t.(xml.CharData); ok {
					if b.Len()+len(c) > MaxCellBytes {
						return "", "", ErrFile
					}
					b.Write(c)
				}
			}
			value = b.String()
		case "is":
			v, e := d.text()
			if e != nil {
				return "", "", e
			}
			value = v
		default:
			if err = d.skip(); err != nil {
				return "", "", err
			}
		}
	}
	if code == "formula" {
		return "", code, nil
	}
	switch kind {
	case "s":
		index, err := strconv.Atoi(value)
		if err != nil || index < 0 || index >= len(shared) {
			return "", "", ErrFile
		}
		value = shared[index]
	case "e":
		return "", "cell_error", nil
	case "", "n", "str", "inlineStr", "b", "d":
	default:
		return "", "", ErrFile
	}
	return value, code, nil
}
