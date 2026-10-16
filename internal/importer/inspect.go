package importer

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"

	"markemind/dials/internal/normalize"
)

// ColumnReport profiles one source column (PII-safe: samples are masked).
type ColumnReport struct {
	Header   string   `json:"header"`
	Field    string   `json:"field"`
	Filled   int64    `json:"filled"`
	Distinct int      `json:"distinct"` // capped at distinctCap
	MaxLen   int      `json:"max_len"`
	Samples  []string `json:"samples"`
}

type InspectReport struct {
	Sniffed Sniffed        `json:"sniffed"`
	Rows    int64          `json:"rows"`
	Columns []ColumnReport `json:"columns"`
}

const distinctCap = 5000

var (
	emailish = regexp.MustCompile(`[^\s@]+@[^\s@]+`)
	digitRun = regexp.MustCompile(`\d`)
)

// MaskSample hides personal values so profiles can be shared safely.
func MaskSample(field, v string) string {
	v = normalize.Truncate(v, 60)
	switch {
	case emailish.MatchString(v):
		return emailish.ReplaceAllStringFunc(v, func(e string) string {
			at := strings.LastIndexByte(e, '@')
			return e[:1] + "***" + e[at:]
		})
	case field == FName || field == FFirst || field == FLast:
		if r, _ := utf8.DecodeRuneInString(v); r != utf8.RuneError {
			return string(r) + "***"
		}
	case field == FLinkedIn || strings.Contains(strings.ToLower(v), "linkedin.com/in/"):
		return "linkedin.com/in/***"
	}
	if d := normalize.PhoneDigits(v); len(d) >= 6 && len(d)*2 >= len(v) {
		n := 0
		return digitRun.ReplaceAllStringFunc(v, func(s string) string {
			n++
			if n <= len(d)-2 {
				return "#"
			}
			return s
		})
	}
	return v
}

// Inspect profiles a whole file: row count, fill rate, distinct values and
// masked samples per column, with the automatic mapping.
func Inspect(r io.Reader) (InspectReport, error) {
	var rep InspectReport
	cr, sn, err := OpenCSV(r)
	if err != nil {
		return rep, err
	}
	rep.Sniffed = sn
	head, err := cr.Read()
	if err != nil {
		return rep, fmt.Errorf("reading header: %w", err)
	}
	headers := CleanHeaders(append([]string(nil), head...))
	m := AutoMap(headers)
	cols := make([]ColumnReport, len(headers))
	seen := make([]map[string]struct{}, len(headers))
	for i, hd := range headers {
		cols[i] = ColumnReport{Header: hd, Field: m[i].Field, Samples: []string{}}
		seen[i] = map[string]struct{}{}
	}
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			var pe *csv.ParseError
			if errors.As(err, &pe) {
				continue
			}
			return rep, err
		}
		rep.Rows++
		for i := 0; i < len(rec) && i < len(cols); i++ {
			v := normalize.Value(rec[i])
			if v == "" {
				continue
			}
			c := &cols[i]
			c.Filled++
			if l := utf8.RuneCountInString(v); l > c.MaxLen {
				c.MaxLen = l
			}
			if len(seen[i]) < distinctCap {
				seen[i][v] = struct{}{}
			}
			if len(c.Samples) < 3 {
				s := MaskSample(c.Field, v)
				dup := false
				for _, x := range c.Samples {
					dup = dup || x == s
				}
				if !dup {
					c.Samples = append(c.Samples, s)
				}
			}
		}
	}
	for i := range cols {
		cols[i].Distinct = len(seen[i])
	}
	rep.Columns = cols
	return rep, nil
}
