package importer

import (
	"encoding/csv"
	"fmt"
	"io"
	"math/rand/v2"
	"strings"
)

// GenHeader is the column layout of synthetic files (mimics common B2B exports).
var GenHeader = []string{"First Name", "Last Name", "Title", "Seniority", "Department", "Decision Power", "Company Name",
	"Website", "Person Linkedin Url", "Email", "Email Status", "Direct Phone", "Mobile Phone", "Corporate Phone",
	"City", "State", "Country", "Industry", "Sector", "Company Type", "# Employees", "Annual Revenue", "Technologies",
	"Keywords", "Zip Code"}

type geo struct{ country, state, city string }

var (
	genGeo = []geo{
		{"USA", "California", "San Francisco"}, {"usa", "California", "Los Angeles"}, {" USA ", "New York", "New York"},
		{"United States", "Texas", "Austin"}, {"U.S.A.", "Washington", "Seattle"}, {"India", "Maharashtra", "Mumbai"},
		{"india", "Karnataka", "Bengaluru"}, {"India", "Delhi", "New Delhi"}, {"United Kingdom", "England", "London"},
		{"UK", "Scotland", "Edinburgh"}, {"Germany", "Bayern", "München"}, {"Germany", "Berlin", "Berlin"},
		{"Canada", "Ontario", "Toronto"}, {"Australia", "NSW", "Sydney"}, {"France", "Île-de-France", "Paris"},
		{"Brazil", "São Paulo", "São Paulo"}, {"Singapore", "", "Singapore"}, {"UAE", "Dubai", "Dubai"},
		{"Japan", "Tokyo", "Tokyo"}, {"Georgia", "", "Tbilisi"}, {"N/A", "N/A", ""},
	}
	genIndustry = []struct{ ind, sec string }{
		{"Software", "SaaS"}, {"software", "Developer Tools"}, {"Information Technology & Services", "IT Consulting"},
		{"Financial Services", "Banking"}, {"Healthcare", "Hospitals"}, {"Manufacturing", "Automotive"},
		{"Retail", "E-commerce"}, {"Marketing & Advertising", "Agencies"}, {"Education", "EdTech"},
		{"Real Estate", "Commercial"}, {"Telecommunications", ""}, {"", ""},
	}
	genFirst  = []string{"James", "Priya", "José", "Zoë", "Søren", "Aarav", "Mei", "Olivia", "Liam", "Fatima", "Noah", "Ananya", "Lukas", "Chloé", "Rahul"}
	genLast   = []string{"Smith", "Sharma", "García", "Müller", "Nguyen", "Patel", "O'Brien", "Kim", "Rossi", "Khan", "Dubois", "Iyer", "Johnson"}
	genTitle  = []struct{ title, sen, dept, dec string }{{"Chief Executive Officer", "C-Level", "Executive", "Decision Maker"}, {"VP of Sales", "VP", "Sales", "Decision Maker"}, {"Head of Marketing", "Director", "Marketing", "Influencer"}, {"Software Engineer", "Entry", "Engineering", "User"}, {"Procurement Manager", "Manager", "Operations", "Decision Maker"}, {"HR Director", "Director", "Human Resources", "Influencer"}, {"CTO", "C-Level", "Engineering", "Decision Maker"}}
	genSize   = []string{"1-10", "11-50", "51-200", "201-500", "501-1000", "1001-5000", "5001+", ""}
	genRev    = []string{"<$1M", "$1M-$10M", "$10M-$50M", "$50M-$100M", "$100M-$1B", "$1B+", ""}
	genType   = []string{"Private", "Public", "Non-profit", "Government", ""}
	genTech   = []string{"Salesforce", "HubSpot", "Google Analytics", "AWS", "Shopify", "WordPress", "Slack", "Zendesk", "Stripe", "React", "Node.js", ".NET", "C++"}
	genEconf  = []string{"Verified", "verified", "Catch-all", "Unverified", ""}
	genWords  = []string{"cloud", "analytics", "payments", "logistics", "ai", "security", "crm", "automation"}
	genDomain = []string{"acme", "globex", "initech", "umbrella", "hooli", "vandelay", "stark", "wayne", "wonka", "tyrell"}
)

// Generate writes rows of deterministic, deliberately messy CSV (variants,
// blanks, diacritics, invalid & disposable emails, duplicates) for tests and
// benchmarks.
func Generate(w io.Writer, rows int, seed uint64) error {
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	cw := csv.NewWriter(w)
	if err := cw.Write(GenHeader); err != nil {
		return err
	}
	pick := func(xs []string) string { return xs[rng.IntN(len(xs))] }
	for i := 0; i < rows; i++ {
		n := i
		if i > 10 && rng.IntN(100) < 3 {
			n = rng.IntN(i) // duplicate identity of an earlier row
		}
		r := rand.New(rand.NewPCG(uint64(n), seed))
		first, last := genFirst[r.IntN(len(genFirst))], genLast[r.IntN(len(genLast))]
		co := genDomain[r.IntN(len(genDomain))] + fmt.Sprint(r.IntN(5000))
		g := genGeo[rng.IntN(len(genGeo))]
		ind := genIndustry[rng.IntN(len(genIndustry))]
		t := genTitle[rng.IntN(len(genTitle))]
		email := fmt.Sprintf("%s.%s%d@%s.com", strings.ToLower(asciiOnly(first)), strings.ToLower(asciiOnly(last)), n, co)
		switch v := rng.IntN(100); {
		case v < 8:
			email = ""
		case v < 10:
			email = "N/A"
		case v < 12:
			email = fmt.Sprintf("user%d@mailinator.com", n)
		case v < 13:
			email = "broken@@example"
		}
		phone := fmt.Sprintf("+1 (%03d) %03d-%04d", 200+r.IntN(700), r.IntN(1000), r.IntN(10000))
		if rng.IntN(100) < 15 {
			phone = ""
		} else if rng.IntN(100) < 3 {
			phone = "123"
		}
		li := fmt.Sprintf("https://www.linkedin.com/in/%s-%s-%d", strings.ToLower(asciiOnly(first)), strings.ToLower(asciiOnly(last)), n)
		if rng.IntN(100) < 30 {
			li = ""
		}
		var tech []string
		for k := rng.IntN(4); k > 0; k-- {
			tech = append(tech, pick(genTech))
		}
		rec := []string{first, last, t.title, t.sen, t.dept, t.dec, strings.ToUpper(co[:1]) + co[1:] + " Inc",
			co + ".com", li, email, pick(genEconf), phone, "", "+1 800 555 0100",
			g.city, g.state, g.country, ind.ind, ind.sec, pick(genType), pick(genSize), pick(genRev),
			strings.Join(tech, ", "), pick(genWords) + " " + pick(genWords), fmt.Sprintf("%05d", rng.IntN(99999))}
		if err := cw.Write(rec); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

func asciiOnly(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < 128 && r != '\'' {
			b.WriteRune(r)
		}
	}
	return b.String()
}
