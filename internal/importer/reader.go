// Package importer streams CSV files into the contact store: it sniffs
// encoding and delimiter, maps headers to canonical fields, cleans every value,
// and upserts with the identity rules of ARCHITECTURE §4.4.
package importer

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"

	"markemind/dials/internal/normalize"
)

// Sniffed describes how a file was decoded.
type Sniffed struct {
	Gzip      bool   `json:"gzip"`
	Encoding  string `json:"encoding"` // utf-8 | utf-8-bom | utf-16le | utf-16be | windows-1252
	Delimiter string `json:"delimiter"`
}

const sniffBytes = 64 << 10

// OpenCSV wraps r with decompression + decoding and returns a configured
// csv.Reader positioned at the header row.
func OpenCSV(r io.Reader) (*csv.Reader, Sniffed, error) {
	var s Sniffed
	br := bufio.NewReaderSize(r, sniffBytes)
	head, _ := br.Peek(2)
	var src io.Reader = br
	if len(head) == 2 && head[0] == 0x1f && head[1] == 0x8b {
		gz, err := gzip.NewReader(br)
		if err != nil {
			return nil, s, fmt.Errorf("gzip: %w", err)
		}
		s.Gzip = true
		src = gz
	}
	br = bufio.NewReaderSize(src, sniffBytes)
	sample, err := br.Peek(sniffBytes)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, bufio.ErrBufferFull) {
		return nil, s, err
	}
	var dec io.Reader
	switch {
	case bytes.HasPrefix(sample, []byte{0xEF, 0xBB, 0xBF}):
		br.Discard(3)
		s.Encoding, dec = "utf-8-bom", br
	case bytes.HasPrefix(sample, []byte{0xFF, 0xFE}):
		s.Encoding = "utf-16le"
		dec = transform.NewReader(br, unicode.UTF16(unicode.LittleEndian, unicode.UseBOM).NewDecoder())
	case bytes.HasPrefix(sample, []byte{0xFE, 0xFF}):
		s.Encoding = "utf-16be"
		dec = transform.NewReader(br, unicode.UTF16(unicode.BigEndian, unicode.UseBOM).NewDecoder())
	case validUTF8Prefix(sample):
		s.Encoding, dec = "utf-8", br
	default:
		s.Encoding = "windows-1252" // Excel "CSV" on Windows
		dec = transform.NewReader(br, charmap.Windows1252.NewDecoder())
	}
	db := bufio.NewReaderSize(dec, sniffBytes)
	decoded, _ := db.Peek(sniffBytes)
	s.Delimiter = string(sniffDelimiter(decoded))
	cr := csv.NewReader(db)
	cr.Comma = rune(s.Delimiter[0])
	cr.LazyQuotes = true
	cr.FieldsPerRecord = -1
	cr.ReuseRecord = true
	return cr, s, nil
}

// validUTF8Prefix ignores a rune cut off at the end of the sample.
func validUTF8Prefix(b []byte) bool {
	for i := 0; i < 3 && len(b) > 0 && !utf8.Valid(b); i++ {
		b = b[:len(b)-1]
	}
	return utf8.Valid(b)
}

func sniffDelimiter(sample []byte) byte {
	line := sample
	if i := bytes.IndexByte(sample, '\n'); i >= 0 {
		line = sample[:i]
	}
	counts := map[byte]int{}
	inQ := false
	for _, c := range line {
		if c == '"' {
			inQ = !inQ
			continue
		}
		if !inQ && (c == ',' || c == ';' || c == '\t' || c == '|') {
			counts[c]++
		}
	}
	best, bestN := byte(','), 0
	for _, c := range []byte{',', ';', '\t', '|'} {
		if counts[c] > bestN {
			best, bestN = c, counts[c]
		}
	}
	return best
}

// CleanHeaders trims headers, names empty ones and de-duplicates.
func CleanHeaders(raw []string) []string {
	out := make([]string, len(raw))
	seen := map[string]int{}
	for i, h := range raw {
		h = normalize.Truncate(normalize.Clean(strings.TrimPrefix(h, "\xef\xbb\xbf")), 80)
		if h == "" {
			h = fmt.Sprintf("column_%d", i+1)
		}
		key := strings.ToLower(h)
		seen[key]++
		if n := seen[key]; n > 1 {
			h = fmt.Sprintf("%s (%d)", h, n)
		}
		out[i] = h
	}
	return out
}
