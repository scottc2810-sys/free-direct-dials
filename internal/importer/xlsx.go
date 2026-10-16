package importer

import (
	"archive/zip"
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/csv"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strconv"
	"strings"
	"time"
)

// Excel support: a streaming .xlsx reader (stdlib only). Memory stays flat on
// million-row sheets: rows are streamed straight from the zip and shared
// strings spill to a temp file once they outgrow a small in-memory budget.

var (
	ErrLegacyXLS = errors.New("old .xls (Excel 97-2003) files aren't supported: open it in Excel and Save As .xlsx or CSV")
	ErrNotXLSX   = errors.New("not a valid .xlsx workbook")
)

func IsXLSX(head []byte) bool { return bytes.HasPrefix(head, []byte("PK\x03\x04")) }
func IsLegacyXLS(head []byte) bool {
	return bytes.HasPrefix(head, []byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1})
}

type Sheet struct {
	Name string `json:"name"`
	path string
}

type XLSX struct {
	zr       *zip.Reader
	sheets   []Sheet
	strs     *stringStore
	tmpDir   string
	maxSize  uint64
	dateKind []byte // per cellXfs index: 0 not a date, 1 date, 2 month-year, 3 time
	date1904 bool
}

const (
	maxXMLPart   = 8 << 30 // refuse absurd decompressed parts (zip bombs)
	memStrBudget = 24 << 20
	maxColumns   = 512
)

// OpenXLSX indexes the workbook and loads (or spills) the shared strings.
func OpenXLSX(ra io.ReaderAt, size int64, tmpDir string) (*XLSX, error) {
	zr, err := zip.NewReader(ra, size)
	if err != nil {
		return nil, ErrNotXLSX
	}
	x := &XLSX{zr: zr, tmpDir: tmpDir, maxSize: maxXMLPart}
	if err := x.readWorkbook(); err != nil {
		return nil, err
	}
	if len(x.sheets) == 0 {
		return nil, errors.New("the workbook has no worksheets")
	}
	if f := x.file("xl/styles.xml"); f != nil {
		x.readStyles(f) // best effort: without it dates stay as serial numbers
	}
	x.strs = &stringStore{dir: tmpDir}
	if f := x.file("xl/sharedStrings.xml"); f != nil {
		if err := x.loadStrings(f); err != nil {
			x.Close()
			return nil, fmt.Errorf("shared strings: %w", err)
		}
	}
	return x, nil
}

func (x *XLSX) Sheets() []Sheet { return x.sheets }

func (x *XLSX) Close() error {
	if x.strs != nil {
		return x.strs.close()
	}
	return nil
}

func (x *XLSX) file(name string) *zip.File {
	for _, f := range x.zr.File {
		if strings.EqualFold(f.Name, name) {
			return f
		}
	}
	return nil
}

func (x *XLSX) open(f *zip.File) (io.ReadCloser, error) {
	if f.UncompressedSize64 > x.maxSize {
		return nil, errors.New("workbook part is too large")
	}
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	return struct {
		io.Reader
		io.Closer
	}{io.LimitReader(rc, int64(x.maxSize)), rc}, nil
}

func (x *XLSX) readWorkbook() error {
	rels := map[string]string{}
	if f := x.file("xl/_rels/workbook.xml.rels"); f != nil {
		rc, err := x.open(f)
		if err != nil {
			return err
		}
		var r struct {
			Rel []struct {
				ID     string `xml:"Id,attr"`
				Target string `xml:"Target,attr"`
			} `xml:"Relationship"`
		}
		err = xml.NewDecoder(rc).Decode(&r)
		rc.Close()
		if err != nil {
			return ErrNotXLSX
		}
		for _, rel := range r.Rel {
			t := rel.Target
			if strings.HasPrefix(t, "/") {
				t = strings.TrimPrefix(t, "/")
			} else {
				t = path.Join("xl", t)
			}
			rels[rel.ID] = t
		}
	}
	f := x.file("xl/workbook.xml")
	if f == nil {
		return ErrNotXLSX
	}
	rc, err := x.open(f)
	if err != nil {
		return err
	}
	defer rc.Close()
	var wb struct {
		Pr struct {
			Date1904 string `xml:"date1904,attr"`
		} `xml:"workbookPr"`
		Sheets []struct {
			Name string     `xml:"name,attr"`
			Attr []xml.Attr `xml:",any,attr"`
		} `xml:"sheets>sheet"`
	}
	if err := xml.NewDecoder(rc).Decode(&wb); err != nil {
		return ErrNotXLSX
	}
	x.date1904 = wb.Pr.Date1904 == "1" || wb.Pr.Date1904 == "true"
	for i, s := range wb.Sheets {
		p := ""
		for _, a := range s.Attr {
			if a.Name.Local == "id" {
				p = rels[a.Value]
			}
		}
		if p == "" {
			p = fmt.Sprintf("xl/worksheets/sheet%d.xml", i+1)
		}
		x.sheets = append(x.sheets, Sheet{Name: s.Name, path: p})
	}
	return nil
}

// readStyles records which cell styles are dates. Excel silently turns typed
// ranges like "10-19" (employees) into the date Oct-2019; knowing the style lets
// us give the visitor back "10-19" instead of the serial number 43739.
func (x *XLSX) readStyles(f *zip.File) {
	rc, err := x.open(f)
	if err != nil {
		return
	}
	defer rc.Close()
	var st struct {
		NumFmts []struct {
			ID   int    `xml:"numFmtId,attr"`
			Code string `xml:"formatCode,attr"`
		} `xml:"numFmts>numFmt"`
		Xfs []struct {
			ID int `xml:"numFmtId,attr"`
		} `xml:"cellXfs>xf"`
	}
	if xml.NewDecoder(rc).Decode(&st) != nil {
		return
	}
	custom := map[int]string{}
	for _, n := range st.NumFmts {
		custom[n.ID] = n.Code
	}
	x.dateKind = make([]byte, len(st.Xfs))
	for i, xf := range st.Xfs {
		x.dateKind[i] = dateKindOf(xf.ID, custom[xf.ID])
	}
}

func dateKindOf(id int, code string) byte {
	switch {
	case id == 17: // mmm-yy
		return 2
	case id >= 14 && id <= 16, id == 22, id >= 27 && id <= 36, id >= 50 && id <= 58:
		return 1
	case id >= 18 && id <= 21, id >= 45 && id <= 47:
		return 3
	case code == "":
		return 0
	}
	// strip quoted literals, escapes and [colour]/[locale] sections before looking for tokens
	var b strings.Builder
	inQ, inB := false, false
	for i := 0; i < len(code); i++ {
		c := code[i]
		switch {
		case c == '"':
			inQ = !inQ
		case inQ:
		case c == '[':
			inB = true
		case c == ']':
			inB = false
		case inB:
		case c == '\\' || c == '_' || c == '*':
			i++
		default:
			b.WriteByte(c | 0x20)
		}
	}
	f := b.String()
	hasY, hasD, hasH := strings.Contains(f, "y"), strings.Contains(f, "d"), strings.Contains(f, "h") || strings.Contains(f, "s")
	switch {
	case hasY && !hasD:
		return 2
	case hasY || hasD:
		return 1
	case hasH:
		return 3
	}
	return 0
}

func (x *XLSX) renderDate(serial float64, kind byte) string {
	base := time.Date(1899, 12, 30, 0, 0, 0, 0, time.UTC)
	if x.date1904 {
		base = time.Date(1904, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	t := base.Add(time.Duration(serial * float64(24*time.Hour)))
	switch kind {
	case 2:
		return fmt.Sprintf("%d-%02d", int(t.Month()), t.Year()%100) // what the person typed before Excel converted it
	case 3:
		return t.Format("15:04:05")
	}
	if t.Hour() == 0 && t.Minute() == 0 && t.Second() == 0 {
		return t.Format("2006-01-02")
	}
	return t.Format("2006-01-02 15:04")
}

// loadStrings reads <si> items: the text of every <t>, excluding phonetic <rPh> runs.
func (x *XLSX) loadStrings(f *zip.File) error {
	rc, err := x.open(f)
	if err != nil {
		return err
	}
	defer rc.Close()
	dec := xml.NewDecoder(bufio.NewReaderSize(rc, 64<<10))
	var cur strings.Builder
	inSI, inT, inPh := false, false, 0
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "si":
				inSI = true
				cur.Reset()
			case "t":
				inT = inSI
			case "rPh":
				inPh++
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "si":
				inSI = false
				if err := x.strs.add(cur.String()); err != nil {
					return err
				}
			case "t":
				inT = false
			case "rPh":
				inPh--
			}
		case xml.CharData:
			if inT && inPh == 0 && cur.Len() < 32768 {
				cur.Write(t)
			}
		}
	}
}

// CSV streams sheet i as UTF-8 CSV (for the regular importer pipeline).
func (x *XLSX) CSV(i int) (io.ReadCloser, error) {
	if i < 0 || i >= len(x.sheets) {
		return nil, fmt.Errorf("sheet %d does not exist", i+1)
	}
	f := x.file(x.sheets[i].path)
	if f == nil {
		return nil, fmt.Errorf("sheet %q is missing from the workbook", x.sheets[i].Name)
	}
	rc, err := x.open(f)
	if err != nil {
		return nil, err
	}
	pr, pw := io.Pipe()
	go func() {
		defer rc.Close()
		cw := csv.NewWriter(pw)
		err := x.streamRows(rc, func(rec []string) error { return cw.Write(rec) })
		cw.Flush()
		if err == nil {
			err = cw.Error()
		}
		pw.CloseWithError(err)
	}()
	return pr, nil
}

func (x *XLSX) streamRows(r io.Reader, emit func([]string) error) error {
	dec := xml.NewDecoder(bufio.NewReaderSize(r, 64<<10))
	var row []string
	var col, maxCol int
	var typ string
	style := -1
	var val strings.Builder
	inV, inRow, inIS := false, false, false
	headerWidth := 0
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("sheet XML: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "row":
				inRow, row, col, maxCol = true, row[:0], 0, -1
			case "c":
				typ, style = "", -1
				ref := ""
				for _, a := range t.Attr {
					switch a.Name.Local {
					case "t":
						typ = a.Value
					case "r":
						ref = a.Value
					case "s":
						if n, err := strconv.Atoi(a.Value); err == nil {
							style = n
						}
					}
				}
				if ref != "" {
					if c := colIndex(ref); c >= 0 {
						col = c
					}
				}
				val.Reset()
			case "v":
				inV = true
			case "is":
				inIS = true
			case "t":
				if inIS {
					inV = true
				}
			}
		case xml.CharData:
			if inV && val.Len() < 32768 {
				val.Write(t)
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "v":
				inV = false
			case "t":
				if inIS {
					inV = false
				}
			case "is":
				inIS = false
			case "c":
				if inRow && col < maxColumns {
					for len(row) <= col {
						row = append(row, "")
					}
					row[col] = x.cellValue(typ, val.String(), style)
					if row[col] != "" && col > maxCol {
						maxCol = col
					}
				}
				col++
			case "row":
				inRow = false
				if maxCol < 0 {
					continue // empty row
				}
				width := maxCol + 1
				if headerWidth == 0 {
					headerWidth = width
				}
				if width < headerWidth {
					width = headerWidth
				}
				for len(row) < width {
					row = append(row, "")
				}
				if err := emit(row[:width]); err != nil {
					return err
				}
			}
		}
	}
}

func (x *XLSX) cellValue(typ, v string, style int) string {
	switch typ {
	case "s":
		i, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return ""
		}
		s, _ := x.strs.get(i)
		return s
	case "b":
		if strings.TrimSpace(v) == "1" {
			return "TRUE"
		}
		return "FALSE"
	case "e":
		return ""
	case "str", "inlineStr":
		return v
	}
	v = strings.TrimSpace(v)
	if f, err := strconv.ParseFloat(v, 64); err == nil {
		if style >= 0 && style < len(x.dateKind) && x.dateKind[style] != 0 && f > 0 && f < 2958466 {
			return x.renderDate(f, x.dateKind[style])
		}
		// phone numbers typed as numbers come back as 9.19876543210E+11: print digits
		if f == float64(int64(f)) && f < 1e15 && f > -1e15 {
			return strconv.FormatInt(int64(f), 10)
		}
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return v
}

// colIndex converts "AB12" → 27.
func colIndex(ref string) int {
	n := 0
	seen := false
	for _, c := range ref {
		if c >= 'A' && c <= 'Z' {
			n = n*26 + int(c-'A'+1)
			seen = true
		} else if c >= 'a' && c <= 'z' {
			n = n*26 + int(c-'a'+1)
			seen = true
		} else {
			break
		}
	}
	if !seen {
		return -1
	}
	return n - 1
}

// stringStore keeps shared strings in memory until a budget, then on disk.
type stringStore struct {
	dir   string
	mem   []string
	bytes int
	file  *os.File
	w     *bufio.Writer
	offs  []uint64
	pos   uint64
	buf   []byte
}

func (s *stringStore) add(v string) error {
	if s.file == nil {
		s.mem = append(s.mem, v)
		s.bytes += len(v) + 16
		if s.bytes < memStrBudget {
			return nil
		}
		f, err := os.CreateTemp(s.dir, "upload-strings-*")
		if err != nil {
			return err
		}
		s.file, s.w = f, bufio.NewWriterSize(f, 256<<10)
		for _, m := range s.mem {
			if err := s.write(m); err != nil {
				return err
			}
		}
		s.mem = nil
		return nil
	}
	return s.write(v)
}

func (s *stringStore) write(v string) error {
	s.offs = append(s.offs, s.pos)
	var hdr [4]byte
	binary.LittleEndian.PutUint32(hdr[:], uint32(len(v)))
	if _, err := s.w.Write(hdr[:]); err != nil {
		return err
	}
	if _, err := s.w.WriteString(v); err != nil {
		return err
	}
	s.pos += uint64(4 + len(v))
	return nil
}

func (s *stringStore) get(i int) (string, error) {
	if s.file == nil {
		if i < 0 || i >= len(s.mem) {
			return "", nil
		}
		return s.mem[i], nil
	}
	if i < 0 || i >= len(s.offs) {
		return "", nil
	}
	if s.w != nil {
		if err := s.w.Flush(); err != nil {
			return "", err
		}
		s.w = nil
	}
	var hdr [4]byte
	if _, err := s.file.ReadAt(hdr[:], int64(s.offs[i])); err != nil {
		return "", err
	}
	n := binary.LittleEndian.Uint32(hdr[:])
	if cap(s.buf) < int(n) {
		s.buf = make([]byte, n)
	}
	b := s.buf[:n]
	if _, err := s.file.ReadAt(b, int64(s.offs[i])+4); err != nil {
		return "", err
	}
	return string(b), nil
}

func (s *stringStore) close() error {
	if s.file != nil {
		name := s.file.Name()
		s.file.Close()
		return os.Remove(name)
	}
	return nil
}

// Source opens an uploaded file of any supported kind as a CSV stream.
type Source struct {
	Kind   string  `json:"kind"` // csv | xlsx
	Sheets []Sheet `json:"sheets,omitempty"`
	Sheet  int     `json:"sheet"`
	r      io.ReadCloser
	closer []io.Closer
}

func (s *Source) Read(p []byte) (int, error) { return s.r.Read(p) }
func (s *Source) Close() error {
	err := s.r.Close()
	for _, c := range s.closer {
		c.Close()
	}
	return err
}

// OpenSource detects CSV (optionally gzipped) or XLSX by content, not extension.
func OpenSource(p, tmpDir string, sheet int) (*Source, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	head := make([]byte, 8)
	n, _ := io.ReadFull(f, head)
	head = head[:n]
	if IsLegacyXLS(head) {
		f.Close()
		return nil, ErrLegacyXLS
	}
	if IsXLSX(head) {
		st, err := f.Stat()
		if err != nil {
			f.Close()
			return nil, err
		}
		x, err := OpenXLSX(f, st.Size(), tmpDir)
		if err != nil {
			f.Close()
			return nil, err
		}
		rc, err := x.CSV(sheet)
		if err != nil {
			x.Close()
			f.Close()
			return nil, err
		}
		return &Source{Kind: "xlsx", Sheets: x.Sheets(), Sheet: sheet, r: rc, closer: []io.Closer{x, f}}, nil
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	return &Source{Kind: "csv", r: f}, nil
}
