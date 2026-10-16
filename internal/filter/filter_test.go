package filter

import (
	"net/url"
	"reflect"
	"strings"
	"testing"
)

func mustParse(t *testing.T, raw string) Filter {
	t.Helper()
	q, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatal(err)
	}
	f, err := Parse(q)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestCanonicalSortedFolded(t *testing.T) {
	a := mustParse(t, "country=USA&country=india&industry=Software&kw=Acme++Corp&tech=HubSpot,salesforce&email=true&junk=1")
	b := mustParse(t, "industry=software&email=1&country=India&country=u.s.a.&kw=acme%20corp&tech=Salesforce&tech=hubspot")
	if a.Canonical() != b.Canonical() {
		t.Fatalf("canonical mismatch:\n%s\n%s", a.Canonical(), b.Canonical())
	}
	want := "country=india&country=united+states&email=1&industry=software&kw=acme+corp&tech=hubspot%2Csalesforce"
	if a.Canonical() != want {
		t.Fatalf("canonical = %s", a.Canonical())
	}
}

func TestBlankAndGarbage(t *testing.T) {
	f := mustParse(t, "state=__BLANK__&state=&state=n/a&city=%F0%9F%9A%80")
	if !reflect.DeepEqual(f.Values["state"], []string{Blank}) {
		t.Fatalf("state = %v", f.Values["state"])
	}
	if _, ok := f.Values["city"]; ok && len(f.Values["city"]) != 1 {
		t.Fatalf("emoji city handled oddly: %v", f.Values["city"])
	}
}

func TestKeywordSanitiser(t *testing.T) {
	// Acceptance #4: injection / wildcard / emoji input never errors and is literal.
	cases := map[string]string{
		`"; DROP --`:               "drop",
		`%`:                        "",
		`_`:                        "",
		`🚀🚀`:                       "",
		`ac`:                       "ac",
		`AC  me`:                   "ac me",
		`NEAR(a b) AND title:ceo*`: "near and title ceo",
		`José`:                     "jose",
		`a b c`:                    "",
	}
	for in, want := range cases {
		if got := strings.Join(Tokens(in), " "); got != want {
			t.Errorf("Tokens(%q) = %q want %q", in, got, want)
		}
	}
	if q := FTSQuery("ac me"); q != `"ac"* "me"*` {
		t.Fatalf("FTSQuery = %s", q)
	}
}

func TestLimits(t *testing.T) {
	q := url.Values{}
	for i := 0; i < MaxValues+1; i++ {
		q.Add("country", "c"+strings.Repeat("x", i))
	}
	if _, err := Parse(q); err != ErrTooManyValues {
		t.Fatalf("want ErrTooManyValues, got %v", err)
	}
	if _, err := Parse(url.Values{"kw": {strings.Repeat("a", MaxQueryLen+1)}}); err != ErrTooLong {
		t.Fatalf("want ErrTooLong, got %v", err)
	}
	f := mustParse(t, "kw="+strings.Repeat("word ", 50))
	if len(strings.Fields(f.Keyword)) > MaxTokens {
		t.Fatal("token cap not applied")
	}
}

func TestTech(t *testing.T) {
	got := TechTerms(" C++ , c#, .NET, node.js, , NODE.JS, <script>")
	want := []string{".net", "c#", "c++", "node.js", "script"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("TechTerms = %v", got)
	}
}

func TestWithoutAndOnlyDim(t *testing.T) {
	f := mustParse(t, "country=usa&industry=software")
	g := f.Without("country")
	if _, ok := g.Values["country"]; ok || len(f.Values) != 2 {
		t.Fatal("Without must not mutate original")
	}
	if d, ok := g.OnlyDim(); !ok || d != "industry" {
		t.Fatal("OnlyDim")
	}
	if !mustParse(t, "").IsEmpty() {
		t.Fatal("empty")
	}
}

func FuzzCanonicalRoundTrip(f *testing.F) {
	f.Add("country=USA&kw=acme&tech=a,b&email=1")
	f.Add("state=__blank__&city=S%C3%A3o+Paulo&phone=yes")
	f.Fuzz(func(t *testing.T, raw string) {
		q, err := url.ParseQuery(raw)
		if err != nil {
			return
		}
		a, err := Parse(q)
		if err != nil {
			return
		}
		q2, err := url.ParseQuery(a.Canonical())
		if err != nil {
			t.Fatalf("canonical not parseable: %v", err)
		}
		b, err := Parse(q2)
		if err != nil {
			t.Fatalf("canonical rejected: %v", err)
		}
		if a.Canonical() != b.Canonical() {
			t.Fatalf("not a fixed point:\n%s\n%s", a.Canonical(), b.Canonical())
		}
		_ = FTSQuery(a.Keyword)
	})
}
