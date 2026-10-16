package filter

import (
	"strings"
	"testing"
)

func TestTitleCanonicalForms(t *testing.T) {
	cases := map[string]string{
		"CEO":                                "ceo",
		"C.E.O.":                             "ceo",
		"ceos":                               "ceo",
		"Chief Executive Officer":            "ceo",
		"chief executive officers":           "ceo",
		"CEO, Chief Executive Officer":       "ceo",
		"VP":                                 "vp",
		"VPs":                                "vp",
		"V.P. Sales":                         "vp sale",
		"Vice President":                     "vp",
		"vice presidents of sales":           "vp sale",
		"Senior Vice President":              "svp",
		"head of sales":                      "head sale",
		"Human Resources Manager":            "hr mgr",
		"HR managers":                        "hr mgr",
		"Chief Human Resources Officer":      "chro",
		"account executive":                  "ae",
		"the of and":                         "the of and", // only stop words: kept as typed
		"node.js www.acme.com":               "node js www acme com",
		"companies":                          "compan",
		"boxes":                              "box",
		"business":                           "business",
		"status analysis":                    "status analysis",
		"x vice x president":                 "vp", // dedupe brings the phrase together; "x" is one letter
		"ceo vice chief executive president": "ceo vp",
	}
	for in, want := range cases {
		if got := strings.Join(Tokens(in), " "); got != want {
			t.Errorf("Tokens(%q) = %q, want %q", in, got, want)
		}
		// canonical form is a fixed point
		if again := strings.Join(Tokens(strings.Join(Tokens(in), " ")), " "); again != strings.Join(Tokens(in), " ") {
			t.Errorf("Tokens not idempotent for %q: %q → %q", in, strings.Join(Tokens(in), " "), again)
		}
	}
}

func TestKeywordExpansion(t *testing.T) {
	if q := FTSQuery("ceo sale"); q != `("ceo"* OR ("chief"* AND "executive"*)) AND "sale"*` {
		t.Fatalf("FTSQuery ceo = %s", q)
	}
	q := FTSQuery("vp")
	for _, part := range []string{`"vp"*`, `"svp"*`, `"evp"*`, `"avp"*`, `("vice"* AND "president"*)`} {
		if !strings.Contains(q, part) {
			t.Fatalf("FTSQuery vp = %s, missing %s", q, part)
		}
	}
	if FTSQuery("") != "" || len(KeywordTerms("")) != 0 {
		t.Fatal("empty keyword")
	}
	for _, e := range titleTable { // every abbreviation is a stable token on its own
		if got := strings.Join(Tokens(e.abbr), " "); got != e.abbr {
			t.Errorf("abbreviation %q canonicalises to %q", e.abbr, got)
		}
		for _, p := range e.phrases {
			if got := strings.Join(Tokens(p), " "); got != e.abbr {
				t.Errorf("phrase %q canonicalises to %q, want %q", p, got, e.abbr)
			}
		}
	}
}
