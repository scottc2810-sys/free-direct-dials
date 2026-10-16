// Package filter parses, validates and canonicalises visitor filters. The
// canonical string is the cache key, the CDN cache key and the shareable URL.
package filter

import (
	"errors"
	"net/url"
	"sort"
	"strings"
	"unicode"

	"markemind/dials/internal/normalize"
)

// Dim is a filterable dimension backed by an integer column of contacts.
type Dim struct {
	ID    int
	Key   string // query parameter and column name
	Label string
}

var Dims = []Dim{
	{1, "country", "Country"},
	{2, "state", "State / Region"},
	{3, "city", "City"},
	{4, "industry", "Industry"},
	{5, "sector", "Sector"},
	{6, "ctype", "Company type"},
	{7, "seniority", "Seniority"},
	{8, "division", "Department"},
	{9, "decision", "Decision power"},
	{10, "csize", "Company size"},
	{11, "revenue", "Revenue"},
	{12, "econf", "Email confidence"},
}

var dimByKey = func() map[string]Dim {
	m := map[string]Dim{}
	for _, d := range Dims {
		m[d.Key] = d
	}
	return m
}()

func DimByKey(k string) (Dim, bool) { d, ok := dimByKey[k]; return d, ok }

const (
	Blank        = "__blank__"
	MaxValues    = 50
	MaxKeyword   = 200
	MaxTokens    = 10
	MaxTech      = 10
	MaxTechLen   = 50
	MaxQueryLen  = 4096
	minTokenRune = 2
)

var (
	ErrTooManyValues = errors.New("too many values in one filter (max 50)")
	ErrTooLong       = errors.New("filter is too long")
)

// Filter is always held in canonical form: folded, sorted, de-duplicated.
type Filter struct {
	Values   map[string][]string
	Keyword  string   // sanitised tokens joined by a single space
	Tech     []string // folded prefixes
	HasEmail bool
	HasPhone bool
}

// Parse builds a canonical Filter from query parameters. Unknown parameters
// are ignored; garbage values never cause errors, only size limits do.
func Parse(q url.Values) (Filter, error) {
	f := Filter{Values: map[string][]string{}}
	size := 0
	for k, vs := range q {
		size += len(k)
		for _, v := range vs {
			size += len(v)
		}
	}
	if size > MaxQueryLen {
		return f, ErrTooLong
	}
	for _, d := range Dims {
		raw := q[d.Key]
		if len(raw) == 0 {
			continue
		}
		if len(raw) > MaxValues {
			return f, ErrTooManyValues
		}
		seen := map[string]bool{}
		var out []string
		for _, v := range raw {
			var n string
			if strings.EqualFold(strings.TrimSpace(v), Blank) {
				n = Blank
			} else {
				n = normalize.Dim(d.Key, v)
			}
			if n == "" || seen[n] {
				continue
			}
			seen[n] = true
			out = append(out, n)
		}
		if len(out) > 0 {
			sort.Strings(out)
			f.Values[d.Key] = out
		}
	}
	f.Keyword = strings.Join(Tokens(q.Get("kw")), " ")
	f.Tech = TechTerms(strings.Join(q["tech"], ","))
	f.HasEmail = truthy(q.Get("email"))
	f.HasPhone = truthy(q.Get("phone"))
	return f, nil
}

func truthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// Tokens splits keyword input into searchable word tokens. Everything that is
// not a letter or digit (FTS operators, quotes, %, _, emoji, SQL) is a
// separator, so the result can never be an FTS syntax error.
func Tokens(kw string) []string {
	kw = normalize.Fold(normalize.Truncate(kw, MaxKeyword))
	var toks []string
	seen := map[string]bool{}
	for _, t := range strings.FieldsFunc(kw, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if len([]rune(t)) < minTokenRune || seen[t] {
			continue
		}
		seen[t] = true
		toks = append(toks, t)
		if len(toks) == MaxTokens {
			break
		}
	}
	return toks
}

// FTSQuery renders tokens as an FTS5 query: every word must prefix-match.
func FTSQuery(keyword string) string {
	if keyword == "" {
		return ""
	}
	parts := strings.Split(keyword, " ")
	for i, p := range parts {
		parts[i] = `"` + p + `"*` // tokens contain only letters/digits: no quote escaping needed
	}
	return strings.Join(parts, " ")
}

// TechFTSQuery renders technology terms for the tech_fts index: a contact
// matches when, for at least one term, every word of that term prefix-matches
// one of its technologies' words ("google tag" → Google Tag Manager).
func TechFTSQuery(terms []string) string {
	var ors []string
	for _, t := range terms {
		var ands []string
		for _, w := range strings.Fields(t) {
			if !strings.ContainsFunc(w, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) {
				continue
			}
			if len([]rune(w)) < 2 && !strings.ContainsAny(w, "+#") {
				continue // one-letter prefixes would expand to most of the index
			}
			ands = append(ands, `"`+w+`"*`) // CleanTech leaves no quotes to escape
		}
		if len(ands) > 0 {
			ors = append(ors, "("+strings.Join(ands, " ")+")")
		}
	}
	return strings.Join(ors, " OR ")
}

// TechTerms parses a comma list of technology names into folded prefixes.
func TechTerms(s string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range strings.Split(s, ",") {
		t := CleanTech(p)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
		if len(out) == MaxTech {
			break
		}
	}
	sort.Strings(out)
	return out
}

// CleanTech folds a technology name, keeping letters, digits and " .+#-"
// (C++, C#, .NET, Node.js). Shared with the importer so both sides agree.
func CleanTech(s string) string {
	s = normalize.Fold(s)
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune(" .+#-", r) {
			b.WriteRune(r)
		}
	}
	return normalize.Truncate(strings.TrimSpace(strings.Join(strings.Fields(b.String()), " ")), MaxTechLen)
}

// Canonical encodes the filter as a sorted query string.
func (f Filter) Canonical() string {
	v := url.Values{}
	for _, d := range Dims {
		for _, x := range f.Values[d.Key] {
			v.Add(d.Key, x)
		}
	}
	if f.Keyword != "" {
		v.Set("kw", f.Keyword)
	}
	if len(f.Tech) > 0 {
		v.Set("tech", strings.Join(f.Tech, ","))
	}
	if f.HasEmail {
		v.Set("email", "1")
	}
	if f.HasPhone {
		v.Set("phone", "1")
	}
	return v.Encode()
}

// Without returns a copy with one dimension removed (for faceting).
func (f Filter) Without(dim string) Filter {
	g := f
	g.Values = make(map[string][]string, len(f.Values))
	for k, v := range f.Values {
		if k != dim {
			g.Values[k] = v
		}
	}
	return g
}

// IsEmpty reports whether no constraint is active.
func (f Filter) IsEmpty() bool {
	return len(f.Values) == 0 && f.Keyword == "" && len(f.Tech) == 0 && !f.HasEmail && !f.HasPhone
}

// OnlyDim reports whether the filter constrains exactly one dimension and
// nothing else (eligible for the dims.cnt fast path).
func (f Filter) OnlyDim() (string, bool) {
	if len(f.Values) != 1 || f.Keyword != "" || len(f.Tech) > 0 || f.HasEmail || f.HasPhone {
		return "", false
	}
	for k := range f.Values {
		return k, true
	}
	return "", false
}
