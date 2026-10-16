package importer

import (
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	"markemind/dials/internal/emailcheck"
	"markemind/dials/internal/filter"
)

// DryRow shows exactly what one source row becomes after mapping + cleaning.
type DryRow struct {
	Line   int64             `json:"line"`
	Fields map[string]string `json:"fields"`
	Extra  map[string]string `json:"extra,omitempty"`
	Skip   string            `json:"skip,omitempty"`
	Notes  []string          `json:"notes,omitempty"`
}

// DryRunResult summarises a mapping before anything is written.
type DryRunResult struct {
	Rows     []DryRow `json:"rows"`
	Stats    Stats    `json:"stats"`
	Warnings []string `json:"warnings"`
}

var cleanedNotes = map[string]string{
	"email_invalid":    "email removed: not a valid address",
	"email_disposable": "email removed: disposable domain",
	"phone_invalid":    "phone removed: fewer than 6 digits",
	"linkedin_invalid": "LinkedIn removed: not a profile URL",
}

// DryRun maps and cleans up to n rows without touching the database.
func DryRun(r io.Reader, mapping Mapping, n int, disp *emailcheck.Disposable) (DryRunResult, error) {
	res := DryRunResult{Rows: []DryRow{}, Warnings: []string{}}
	cr, sn, err := OpenCSV(r)
	if err != nil {
		return res, err
	}
	head, err := cr.Read()
	if err != nil {
		return res, fmt.Errorf("reading header: %w", err)
	}
	headers := CleanHeaders(append([]string(nil), head...))
	if mapping == nil {
		mapping = AutoMap(headers)
	}
	if len(mapping) != len(headers) {
		return res, fmt.Errorf("mapping has %d columns, file has %d", len(mapping), len(headers))
	}
	if err := mapping.Validate(); err != nil {
		return res, err
	}
	res.Warnings = mapping.Warnings()
	st := newStats()
	st.Sniffed = sn
	rs := &runState{im: &Importer{Disposable: disp}, idx: mapping.index(), headers: headers, stats: &st}
	var line int64 = 1
	for len(res.Rows) < n {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		line++
		if err != nil {
			var pe *csv.ParseError
			if errors.As(err, &pe) {
				st.Rows++
				st.Skipped++
				st.Reasons["malformed"]++
				res.Rows = append(res.Rows, DryRow{Line: line, Skip: "malformed"})
				continue
			}
			return res, err
		}
		st.Rows++
		before := map[string]int64{}
		for k, v := range st.Cleaned {
			before[k] = v
		}
		r, reason := rs.build(rec)
		row := DryRow{Line: line, Fields: map[string]string{}, Skip: reason}
		for k, v := range st.Cleaned {
			if v > before[k] {
				row.Notes = append(row.Notes, cleanedNotes[k])
			}
		}
		set := func(k, v string) {
			if v != "" {
				row.Fields[k] = v
			}
		}
		set("name", r.name)
		set("title", r.title)
		set("company", r.company)
		set("website", r.website)
		set("linkedin", r.linkedin)
		set("email", r.email)
		set("phone", r.phone)
		for i, d := range filter.Dims {
			set(d.Key, r.dimRaw[i])
		}
		set("tech", strings.Join(r.tech, ", "))
		if len(r.extra) > 0 {
			row.Extra = r.extra
		}
		if reason != "" {
			st.Skipped++
			st.Reasons[reason]++
		} else {
			st.Added++ // "would import" (new vs. merge is only known against the DB)
		}
		res.Rows = append(res.Rows, row)
	}
	res.Stats = st
	return res, nil
}

// Warnings flags mappings that will import poorly.
func (m Mapping) Warnings() []string {
	has := map[string]int{}
	for _, c := range m {
		has[c.Field]++
	}
	var w []string
	if has[FEmail] == 0 {
		w = append(w, "No column is mapped to Email: contacts can't be matched by email and 'Has email' will be empty.")
	}
	if has[FName] == 0 && (has[FFirst] == 0 || has[FLast] == 0) {
		w = append(w, "No Name column (or First + Last name) is mapped: rows without email or LinkedIn can't be de-duplicated.")
	}
	if has[FDirect]+has[FMobile]+has[FPhone]+has[FWorkPhone]+has[FCoPhone] == 0 {
		w = append(w, "No phone column is mapped: 'Has phone' and direct dials will be empty.")
	}
	if has["country"] == 0 {
		w = append(w, "No Country column is mapped: the most used filter will show only (Blank).")
	}
	// location fields are left out on purpose: company_city/state/country filling the person's blanks is the design
	for _, f := range []string{FName, FTitle, FCompany, "industry"} {
		if has[f] > 1 {
			w = append(w, fmt.Sprintf("%d columns are mapped to %q: the first non-empty value in each row wins.", has[f], f))
		}
	}
	return w
}

// Signature identifies a header layout so a mapping can be remembered.
func Signature(headers []string) string {
	h := sha256.New()
	for _, x := range headers {
		h.Write([]byte(strings.ToLower(strings.TrimSpace(x))))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:24]
}
