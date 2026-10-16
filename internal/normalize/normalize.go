// Package normalize implements the text-normalisation contract used by the
// importer and the filter engine (ARCHITECTURE §5). Every function is pure and
// safe for concurrent use.
package normalize

import (
	"crypto/sha1"
	"encoding/base64"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// Clean trims, removes control / zero-width characters and collapses any run
// of whitespace into a single space. It keeps case and accents (display form).
func Clean(s string) string {
	if s == "" {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	space := false
	for _, r := range s {
		switch {
		case r == utf8.RuneError:
			continue
		case r == 0x200b || r == 0x200c || r == 0x200d || r == 0x2060 || r == 0xfeff:
			continue // zero-width & BOM
		case unicode.IsSpace(r) || r == 0x00a0:
			space = true
			continue
		case unicode.IsControl(r):
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	return b.String()
}

// Fold is the comparison form: Clean → NFKD → strip combining marks → lower.
// Fold(Fold(x)) == Fold(x).
func Fold(s string) string {
	s = Clean(s)
	if s == "" {
		return s
	}
	if !isASCII(s) {
		d := norm.NFKD.String(s)
		var b strings.Builder
		b.Grow(len(d))
		for _, r := range d {
			if unicode.Is(unicode.Mn, r) {
				continue
			}
			b.WriteRune(r)
		}
		s = Clean(b.String()) // NFKD may introduce spaces (e.g. ideographic space)
	}
	return strings.ToLower(s)
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

var blanks = map[string]bool{
	"": true, "n/a": true, "na": true, "n.a.": true, "n.a": true, "null": true, "none": true,
	"nil": true, "-": true, "--": true, "---": true, "unknown": true, "not available": true,
	"not applicable": true, "#n/a": true, "undefined": true, "?": true, "nan": true,
	"(blank)": true, "blank": true, "tbd": true, ".": true, "n\\a": true, "#value!": true,
	"#ref!": true, "#name?": true,
}

// Value returns the cleaned display value, or "" if the cell is a blank marker.
// A leading apostrophe before a number ('+1 555…, '90291) is Excel's
// "store as text" marker, not data, and is removed.
func Value(s string) string {
	c := Clean(s)
	if len(c) > 1 && (c[0] == '\'' || c[0] == '`') && (c[1] == '+' || c[1] >= '0' && c[1] <= '9') {
		c = c[1:]
	}
	if blanks[Fold(c)] {
		return ""
	}
	return c
}

var rangeDash = strings.NewReplacer(" - ", "-", " -", "-", "- ", "-", "\xe2\x80\x93", "-", "\xe2\x80\x94", "-")

var dimPunct = strings.NewReplacer("&", " and ", ".", "", "'", "", "\xe2\x80\x99", "", ",", " ", ";", " ", "/", " ", "|", " ", "_", " ", "(", " ", ")", " ")

// Version identifies the alias tables below. Raise it whenever they change:
// every store then re-normalises its dictionary once (importer.RenormalizeDims),
// merging values that now mean the same ("Vice President" and "VP").
const Version = 4

// Dim returns the normalised value used for dimension dictionaries. It returns
// "" for blank markers. dim is the dimension key (country, seniority and
// department have alias tables).
func Dim(dim, raw string) string {
	if multiValued[dim] && strings.ContainsAny(raw, ",;|") {
		// vendors sometimes list several ("CXO, VP"): the first real one counts
		for _, part := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ';' || r == '|' }) {
			if v := Dim(dim, part); v != "" {
				return v
			}
		}
		return ""
	}
	f := Fold(raw)
	if blanks[f] {
		return ""
	}
	f = strings.Join(strings.Fields(dimPunct.Replace(f)), " ")
	f = rangeDash.Replace(f) // "0 - 9" == "0-9", "$1m – $5m" == "$1m-$5m"
	if blanks[f] {           // e.g. "'blank" or "(n/a)" only become a blank marker after punctuation is stripped
		return ""
	}
	if !numericOK[dim] && !strings.ContainsFunc(f, unicode.IsLetter) {
		return "" // a ZIP code or id in a text column is noise, not a value
	}
	if m := dimAliases[dim]; m != nil {
		if a, ok := m[f]; ok {
			return a
		}
	}
	return f
}

// multiValued dimensions are single-choice even when a file lists several.
var multiValued = map[string]bool{"seniority": true, "division": true, "decision": true}

// numericOK dimensions may legitimately contain no letters (bands and scores).
var numericOK = map[string]bool{"csize": true, "revenue": true, "econf": true}

var dimAliases = map[string]map[string]string{"country": countryAliases, "seniority": seniorityAliases, "division": divisionAliases}

// seniorityAliases fold the many ways exports write a level onto one value.
var seniorityAliases = map[string]string{
	"c level": "c-level", "c-suite": "c-level", "c suite": "c-level", "csuite": "c-level", "cxo": "c-level", "c-level executive": "c-level",
	"c level executive": "c-level", "chief": "c-level", "chief officer": "c-level", "executive level": "c-level",
	"vice president": "vp", "vice-president": "vp", "vice presidents": "vp", "vps": "vp", "vp level": "vp", "vp-level": "vp",
	"svp": "vp", "evp": "vp", "avp": "vp", "senior vice president": "vp", "executive vice president": "vp",
	"directors": "director", "dir": "director", "director level": "director", "director-level": "director",
	"managers": "manager", "mgr": "manager", "manager level": "manager", "manager-level": "manager",
	"sr": "senior", "senior level": "senior", "senior-level": "senior",
	"junior": "entry", "jr": "entry", "entry level": "entry", "entry-level": "entry",
	"owners": "owner", "partners": "partner", "founders": "founder", "co-founder": "founder", "co founder": "founder", "cofounder": "founder",
	"heads": "head", "head of": "head", "interns": "intern", "internship": "intern", "trainee": "intern",
	"non manager": "non-manager", "nonmanager": "non-manager", "non-managers": "non-manager",
}

// divisionAliases fold department spellings (dimPunct already turned & into "and").
var divisionAliases = map[string]string{
	"hr": "human resources", "human resource": "human resources", "people and culture": "human resources", "people operations": "human resources",
	"it": "information technology", "information technology it": "information technology", "i t": "information technology",
	"information technology systems": "information technology", "it systems": "information technology", "information systems": "information technology",
	"eng": "engineering", "engineering and technical": "engineering", "engineering and technology": "engineering",
	"r and d": "research and development", "rnd": "research and development", "randd": "research and development",
	"bd": "business development", "biz dev": "business development", "bizdev": "business development",
	"mktg": "marketing", "ops": "operations", "operation": "operations",
	"fin": "finance", "c suite": "executive", "c-suite": "executive", "csuite": "executive", "executives": "executive",
	"customer service": "customer support", "support": "customer support",
}

var countryAliases = map[string]string{
	"us": "united states", "usa": "united states", "u s": "united states", "u s a": "united states",
	"united states of america": "united states", "america": "united states", "the united states": "united states",
	"uk": "united kingdom", "u k": "united kingdom", "gb": "united kingdom", "great britain": "united kingdom",
	"britain": "united kingdom", "united kingdom of great britain and northern ireland": "united kingdom",
	"uae": "united arab emirates", "u a e": "united arab emirates",
	"republic of india": "india", "ind": "india",
	"korea republic of": "south korea", "republic of korea": "south korea", "korea south": "south korea", "korea": "south korea",
	"russian federation": "russia", "viet nam": "vietnam", "czech republic": "czechia",
	"the netherlands": "netherlands", "holland": "netherlands", "deutschland": "germany",
	"iran islamic republic of": "iran", "hong kong sar": "hong kong", "hong kong china": "hong kong",
	"turkiye": "turkey", "ksa": "saudi arabia", "kingdom of saudi arabia": "saudi arabia",
	"prc": "china", "peoples republic of china": "china", "china mainland": "china", "mainland china": "china",
	"the philippines": "philippines", "republic of singapore": "singapore", "aus": "australia",
	"nz": "new zealand", "can": "canada", "brasil": "brazil", "espana": "spain", "mexico city": "mexico city",
	"ivory coast": "cote divoire", "cote d ivoire": "cote divoire",
}

// PhoneDigits keeps only ASCII digits.
func PhoneDigits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ValidPhone: 6–20 digits and not an Excel scientific-notation artefact.
func ValidPhone(s string) bool {
	if strings.ContainsAny(s, "eE") && strings.Contains(s, "+") && strings.Contains(s, ".") {
		return false // 9.19876E+11
	}
	n := len(PhoneDigits(s))
	return n >= 6 && n <= 20
}

// EmailKey is the identity form of an email address.
func EmailKey(e string) string { return strings.ToLower(strings.TrimSpace(e)) }

// LinkedInKey returns "in/<slug>" (or "pub/…") for person profile URLs, else "".
func LinkedInKey(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || !strings.HasSuffix(strings.ToLower(u.Hostname()), "linkedin.com") {
		return ""
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 || (parts[0] != "in" && parts[0] != "pub") || parts[1] == "" {
		return ""
	}
	slug, err := url.PathUnescape(parts[1])
	if err != nil {
		slug = parts[1]
	}
	return parts[0] + "/" + strings.ToLower(slug)
}

// HashKey is the fallback identity for rows with neither email nor LinkedIn.
func HashKey(name, company, title, phone string) string {
	if Fold(name) == "" {
		return ""
	}
	h := sha1.Sum([]byte(Fold(name) + "|" + Fold(company) + "|" + Fold(title) + "|" + PhoneDigits(phone)))
	return base64.RawURLEncoding.EncodeToString(h[:15])
}

// Slug makes a URL path segment from a label: "Information Technology & Services" → "information-technology-and-services".
func Slug(label string) string {
	f := strings.ReplaceAll(Fold(label), "&", " and ")
	var b strings.Builder
	dash := false
	for _, r := range f {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(r)
			if b.Len() >= 80 {
				break
			}
			continue
		}
		dash = true
	}
	return b.String()
}

// Truncate cuts s to at most n runes.
func Truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}
