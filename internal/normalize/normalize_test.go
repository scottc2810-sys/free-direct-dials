package normalize

import (
	"testing"
	"unicode/utf8"
)

func TestDimVariantsCollapse(t *testing.T) {
	// Acceptance #1: USA / usa / " USA " are one option (and aliases join them).
	for _, in := range []string{"USA", "usa", " USA ", "U.S.A.", "United States", "united  states of america", "\u00a0USA\u200b"} {
		if got := Dim("country", in); got != "united states" {
			t.Errorf("Dim(country,%q) = %q", in, got)
		}
	}
	if Dim("industry", "Information Technology & Services") != Dim("industry", "information technology and services") {
		t.Error("& and 'and' should fold together")
	}
	if Dim("city", "São Paulo") != "sao paulo" {
		t.Errorf("diacritics: %q", Dim("city", "São Paulo"))
	}
	if Dim("state", "N/A") != "" || Dim("state", " null ") != "" || Dim("state", "-") != "" {
		t.Error("blank markers must fold to empty")
	}
	if Dim("city", "O\xe2\x80\x99Fallon") != Dim("city", "O'Fallon") || Dim("city", "O'Fallon") != "ofallon" {
		t.Errorf("curly and straight apostrophes must fold together: %q %q", Dim("city", "O\xe2\x80\x99Fallon"), Dim("city", "O'Fallon"))
	}
	if Dim("industry", "us") != "us" {
		t.Error("aliases apply to country only")
	}
}

func TestFoldIdempotent(t *testing.T) {
	for _, s := range []string{"ÉCOLE  Ünïcode", "ﬁnance", "İstanbul", "  a\tb\nc ", "日本 東京", "🚀 rocket"} {
		if Fold(Fold(s)) != Fold(s) {
			t.Errorf("not idempotent for %q", s)
		}
	}
	if Fold("ﬁnance") != "finance" {
		t.Errorf("NFKD ligature: %q", Fold("ﬁnance"))
	}
}

func FuzzFold(f *testing.F) {
	for _, s := range []string{"USA", "São Paulo", "\xff\xfe", "á", "  x  "} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		a := Fold(s)
		if Fold(a) != a {
			t.Fatalf("Fold not idempotent: %q -> %q -> %q", s, a, Fold(a))
		}
		if !utf8.ValidString(a) {
			t.Fatalf("invalid utf8 out of %q", s)
		}
		_ = Dim("country", s)
		_ = Slug(s)
	})
}

func TestPhone(t *testing.T) {
	cases := map[string]bool{
		"+1 (555) 010-2030": true, "12345": false, "123456": true, "9.19876E+11": false,
		"ext": false, "+91 98765 43210": true, "123456789012345678901": false,
	}
	for in, want := range cases {
		if ValidPhone(in) != want {
			t.Errorf("ValidPhone(%q) = %v", in, !want)
		}
	}
}

func TestLinkedInKey(t *testing.T) {
	cases := map[string]string{
		"https://www.linkedin.com/in/John-Doe-123/": "in/john-doe-123",
		"linkedin.com/in/jane?trk=x":                "in/jane",
		"http://in.linkedin.com/in/r%C3%A9mi":       "in/rémi",
		"https://www.linkedin.com/company/acme":     "",
		"https://evil.com/in/john":                  "",
		"https://notlinkedin.com.evil.io/in/john":   "",
		"": "",
	}
	for in, want := range cases {
		if got := LinkedInKey(in); got != want {
			t.Errorf("LinkedInKey(%q) = %q want %q", in, got, want)
		}
	}
}

func TestSlug(t *testing.T) {
	cases := map[string]string{
		"Information Technology & Services": "information-technology-and-services",
		"  São Paulo ":                      "sao-paulo",
		"C++ / .NET":                        "c-net",
		"日本":                                "",
	}
	for in, want := range cases {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q want %q", in, got, want)
		}
	}
}

func TestHashKeyStable(t *testing.T) {
	a := HashKey("John  Smith", "ACME", "CEO", "+1 555 0100")
	b := HashKey("john smith", "acme", "ceo", "15550100")
	if a == "" || a != b {
		t.Fatalf("hash key unstable: %q %q", a, b)
	}
	if HashKey("", "acme", "", "") != "" {
		t.Fatal("no name → no hash identity")
	}
}

func TestDimAliases(t *testing.T) {
	for dim, m := range dimAliases {
		for from, to := range m {
			if got := Dim(dim, to); got != to {
				t.Errorf("%s alias target %q is not a fixed point (→ %q)", dim, to, got)
			}
			if got := Dim(dim, from); got != to {
				t.Errorf("Dim(%s, %q) = %q, want %q", dim, from, got, to)
			}
		}
	}
	for raw, want := range map[string]string{"Vice President": "vp", "VP-Level": "vp", "C-Suite": "c-level", "C Level": "c-level",
		"Directors": "director", "Entry Level": "entry", "Co-Founder": "founder"} {
		if got := Dim("seniority", raw); got != want {
			t.Errorf("seniority %q = %q, want %q", raw, got, want)
		}
	}
	for raw, want := range map[string]string{"HR": "human resources", "R&D": "research and development", "IT": "information technology",
		"Biz Dev": "business development", "Human Resources": "human resources"} {
		if got := Dim("division", raw); got != want {
			t.Errorf("division %q = %q, want %q", raw, got, want)
		}
	}
	for raw, want := range map[string]string{"CXO, VP": "c-level", "Owner, Director": "owner", "Director; Manager": "director",
		" , VP": "vp", "37214": "", "80234": ""} {
		if got := Dim("seniority", raw); got != want {
			t.Errorf("seniority %q = %q, want %q", raw, got, want)
		}
	}
	if Dim("city", "90291") != "" || Dim("csize", "10-19") != "10-19" || Dim("revenue", "$1-10") == "" || Dim("econf", "95") != "95" {
		t.Error("letterless values: text columns blank, bands and scores kept")
	}
	if Dim("industry", "Food, Beverages") != "food beverages" {
		t.Error("only single-choice dimensions take the first listed value")
	}
	if Dim("city", "VP") != "vp" || Dim("industry", "IT") != "it" {
		t.Error("aliases must only apply to their own dimension")
	}
}
