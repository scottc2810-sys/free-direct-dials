package filter

import (
	"regexp"
	"sort"
	"strings"
)

// Job titles are written many ways: "CEO", "C.E.O.", "CEOs", "Chief Executive
// Officer". Keyword search treats an abbreviation and its spelled-out forms as
// one thing, in both directions. The index is untouched: the query is rewritten
// (ceo → "ceo"* OR ("chief"* "executive"* "officer"*)), so this works the same
// on every store, on data imported before a row was added here, and costs a
// few extra index lookups per search.
//
// Phrases are written in their singular, folded form; each word prefix-matches.
var titleTable = []struct {
	abbr    string
	phrases []string // spelled-out forms
	alts    []string // narrower abbreviations that also count (an SVP is a VP)
}{
	{"ceo", []string{"chief executive officer", "chief executive"}, nil},
	{"cfo", []string{"chief financial officer", "chief finance officer"}, nil},
	{"coo", []string{"chief operating officer", "chief operation officer"}, nil},
	{"cto", []string{"chief technology officer", "chief technical officer"}, nil},
	{"cio", []string{"chief information officer"}, nil},
	{"ciso", []string{"chief information security officer"}, nil},
	{"cmo", []string{"chief marketing officer"}, nil},
	{"cro", []string{"chief revenue officer", "chief risk officer"}, nil},
	{"cso", []string{"chief sale officer", "chief security officer", "chief strategy officer"}, nil},
	{"cpo", []string{"chief product officer", "chief people officer", "chief procurement officer"}, nil},
	{"chro", []string{"chief human resource officer"}, nil},
	{"cdo", []string{"chief data officer", "chief digital officer"}, nil},
	{"cco", []string{"chief commercial officer", "chief compliance officer", "chief customer officer"}, nil},
	{"cbo", []string{"chief business officer"}, nil},
	{"vp", []string{"vice president"}, []string{"svp", "evp", "avp"}},
	{"svp", []string{"senior vice president"}, nil},
	{"evp", []string{"executive vice president"}, nil},
	{"avp", []string{"assistant vice president", "associate vice president"}, nil},
	{"md", []string{"managing director"}, nil},
	{"gm", []string{"general manager"}, nil},
	{"hr", []string{"human resource"}, nil},
	{"it", []string{"information technology"}, nil},
	{"bd", []string{"business development", "biz dev"}, nil},
	{"bdr", []string{"business development representative"}, nil},
	{"bdm", []string{"business development manager"}, nil},
	{"sdr", []string{"sale development representative"}, nil},
	{"ae", []string{"account executive"}, nil},
	{"csm", []string{"customer success manager"}, nil},
	{"pm", []string{"project manager", "product manager"}, nil},
	{"qa", []string{"quality assurance"}, nil},
	{"swe", []string{"software engineer"}, nil},
	{"sde", []string{"software development engineer"}, nil},
	{"sr", []string{"senior"}, nil},
	{"jr", []string{"junior"}, nil},
	{"mgr", []string{"manager"}, nil},
	{"mktg", []string{"marketing"}, nil},
	{"ops", []string{"operation"}, nil},
	{"admin", []string{"administrator", "administration"}, nil},
	{"asst", []string{"assistant"}, nil},
	{"assoc", []string{"associate"}, nil},
	{"acct", []string{"accountant", "accounting", "account"}, nil},
}

// stopWords are dropped from keyword queries ("head of sales" finds "Sales
// Head"); a query made only of them is kept as it is.
var stopWords = map[string]bool{"of": true, "the": true, "and": true, "for": true, "at": true, "in": true, "to": true,
	"an": true, "on": true, "by": true, "with": true, "or": true, "from": true}

var (
	abbrAlts   = map[string][][]string{} // abbreviation → alternatives (each an AND of prefix words)
	phraseAbbr = map[string]string{}     // "chief executive officer" → "ceo"
	maxPhrase  = 1
	dotted     = regexp.MustCompile(`(^|[^\pL\pN.])((?:\pL\.)+\pL)\.?($|[^\pL\pN.])`)
)

func init() {
	for _, e := range titleTable {
		alts := [][]string{{e.abbr}}
		for _, a := range e.alts {
			alts = append(alts, []string{a})
		}
		var phrases [][]string
		for _, p := range e.phrases {
			words := strings.Fields(p)
			phrases = append(phrases, words)
			phraseAbbr[p] = e.abbr
			maxPhrase = max(maxPhrase, len(words))
		}
		// keep only minimal phrases: "chief executive" already covers "chief executive officer"
		sort.Slice(phrases, func(i, j int) bool { return len(phrases[i]) < len(phrases[j]) })
		var minimal [][]string
		for _, p := range phrases {
			covered := false
			for _, m := range minimal {
				covered = covered || subset(m, p)
			}
			if !covered {
				minimal = append(minimal, p)
			}
		}
		abbrAlts[e.abbr] = append(alts, minimal...)
	}
}

func subset(small, big []string) bool {
	for _, w := range small {
		found := false
		for _, b := range big {
			found = found || b == w
		}
		if !found {
			return false
		}
	}
	return true
}

// CollapseDots turns dotted single-letter abbreviations into words ("C.E.O." →
// "CEO", "V.P." → "VP") and leaves domains and versions alone. The importer
// applies it to indexed text too, so "C.E.O." titles are found by "ceo".
func CollapseDots(s string) string { return collapseDots(s) }

func collapseDots(s string) string {
	if !strings.Contains(s, ".") {
		return s
	}
	return dotted.ReplaceAllStringFunc(s, func(m string) string {
		sm := dotted.FindStringSubmatch(m)
		return sm[1] + strings.ReplaceAll(sm[2], ".", "") + sm[3]
	})
}

// singular reduces common English plurals. Every result is a prefix of the word
// it came from, so with prefix matching the word as typed still matches and
// the singular form now matches too ("presidents" → "president", "boxes" →
// "box", "companies" → "compan"). Known abbreviations lose a plural s ("VPs").
func singular(t string) string {
	for {
		n := len(t)
		var next string
		switch {
		case n >= 3 && t[n-1] == 's' && abbrAlts[t[:n-1]] != nil:
			next = t[:n-1]
		case n >= 7 && strings.HasSuffix(t, "ies"):
			next = t[:n-3]
		case n >= 5 && (strings.HasSuffix(t, "sses") || strings.HasSuffix(t, "xes") || strings.HasSuffix(t, "ches") ||
			strings.HasSuffix(t, "shes") || strings.HasSuffix(t, "zes")):
			next = t[:n-2]
		case n >= 4 && t[n-1] == 's' && !strings.HasSuffix(t, "ss") && !strings.HasSuffix(t, "us") && !strings.HasSuffix(t, "is"):
			next = t[:n-1]
		default:
			return t
		}
		if len([]rune(next)) < minTokenRune {
			return t
		}
		t = next
	}
}

// canonicalTitles drops stop words (unless nothing else is left) and replaces
// spelled-out titles with their abbreviation, longest phrase first.
func canonicalTitles(toks []string) []string {
	kept := make([]string, 0, len(toks))
	for _, t := range toks {
		if !stopWords[t] {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		kept = toks
	}
	out := make([]string, 0, len(kept))
	for i := 0; i < len(kept); {
		matched := false
		for n := min(maxPhrase, len(kept)-i); n >= 2; n-- {
			if a, ok := phraseAbbr[strings.Join(kept[i:i+n], " ")]; ok {
				out = append(out, a)
				i += n
				matched = true
				break
			}
		}
		if !matched {
			if a, ok := phraseAbbr[kept[i]]; ok && abbrAlts[kept[i]] == nil {
				out = append(out, a) // one-word forms: "senior" → sr, "manager" → mgr
			} else {
				out = append(out, kept[i])
			}
			i++
		}
	}
	return out
}

// KeywordTerms expands a canonical keyword for search: every term must match;
// a term matches when any of its alternatives matches; an alternative matches
// when each of its words prefix-matches a word of the contact.
func KeywordTerms(keyword string) [][][]string {
	if keyword == "" {
		return nil
	}
	var terms [][][]string
	for _, t := range strings.Split(keyword, " ") {
		if alts, ok := abbrAlts[t]; ok {
			terms = append(terms, alts)
		} else {
			terms = append(terms, [][]string{{t}})
		}
	}
	return terms
}
