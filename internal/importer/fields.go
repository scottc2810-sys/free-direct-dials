package importer

import (
	"regexp"
	"strings"

	"markemind/dials/internal/normalize"
)

// Canonical field names. Dimension fields use the filter.Dims keys.
const (
	FName      = "name"
	FFirst     = "first_name"
	FLast      = "last_name"
	FTitle     = "title"
	FCompany   = "company"
	FWebsite   = "website"
	FLinkedIn  = "linkedin"
	FEmail     = "email"
	FDirect    = "direct_phone"
	FMobile    = "mobile"
	FPhone     = "phone"
	FWorkPhone = "work_phone"
	FCoPhone   = "company_phone"
	FTech      = "tech"
	FSkills    = "skills"
	FProducts  = "products"
	FKeywords  = "keywords"
	FExtra     = ""       // keep in the extra JSON
	FIgnore    = "ignore" // drop the column
)

// phonePriority: the first valid number wins.
var phonePriority = []string{FDirect, FMobile, FPhone, FWorkPhone, FCoPhone}

// Fields lists every mappable target (for the admin mapping UI).
var Fields = []string{FName, FFirst, FLast, FTitle, "seniority", "division", "decision", FCompany, FWebsite, FLinkedIn,
	FEmail, "econf", FDirect, FMobile, FPhone, FWorkPhone, FCoPhone, "city", "state", "country", "industry", "sector",
	"ctype", "csize", "revenue", FTech, FSkills, FProducts, FKeywords}

var aliases = map[string]string{}

func init() {
	add := func(field string, names ...string) {
		for _, n := range names {
			aliases[headerKey(n)] = field
		}
	}
	add(FName, "name", "full name", "contact name", "person name", "contact", "contact full name")
	add(FFirst, "first name", "firstname", "first", "given name", "fname")
	add(FLast, "last name", "lastname", "last", "surname", "family name", "lname")
	add(FTitle, "title", "job title", "designation", "position", "role", "job role", "contact title", "person title", "lead titles", "lead title", "titles", "job titles")
	add("seniority", "seniority", "seniority level", "management level", "level", "job level")
	add("division", "division", "department", "dept", "function", "job function", "departments", "lead divison", "lead division", "lead department")
	add("decision", "decision power", "decision making power", "decision maker", "buying role", "decision", "decision making")
	add(FCompany, "company", "company name", "organization", "organisation", "account name", "employer", "organization name", "account", "company name for emails")
	add(FWebsite, "website", "company website", "domain", "company domain", "url", "web", "website url", "web address")
	add(FLinkedIn, "linkedin", "linkedin url", "person linkedin url", "linkedin profile", "li url", "contact linkedin url", "profile url", "linkedin profile url")
	add(FEmail, "email", "email address", "work email", "business email", "e mail", "mail", "primary email", "email id", "contact email")
	add("econf", "email confidence", "email status", "email conf", "email verification", "email quality", "verification status", "email confidence score")
	add(FDirect, "direct dial", "direct phone", "direct number", "direct", "direct dial phone", "dd")
	add(FMobile, "mobile", "mobile phone", "cell", "cell phone", "mobile number", "mobile no")
	add(FPhone, "phone", "phone number", "telephone", "tel", "contact number", "contact phone", "phone no", "person phone")
	add(FWorkPhone, "work phone", "office phone", "business phone", "work direct phone")
	add(FCoPhone, "company phone", "corporate phone", "hq phone", "company phone number", "company phone numbers", "main phone", "switchboard")
	// company_* location columns map to the same field: the first non-empty value in file
	// order wins, so the person's own location is used and the company's fills the gaps.
	add("city", "city", "town", "person city", "contact city", "location city", "company city", "hq city")
	add("state", "state", "region", "province", "state province", "county", "person state", "contact state", "state region", "company state", "hq state")
	add("country", "country", "nation", "person country", "contact country", "country name", "country region", "company country", "hq country")
	add("industry", "industry", "industries", "company industry", "primary industry")
	add("sector", "sector", "sub industry", "subindustry", "vertical", "industry sector", "company sector")
	add("ctype", "company type", "type", "ownership", "organization type", "account type", "company ownership")
	add("csize", "company size", "employees", "employee count", "headcount", "no of employees", "number of employees", "company size range", "employee size range",
		"size", "employee size", "employee range", "num employees", "employees range")
	add("revenue", "revenue", "annual revenue", "company revenue", "revenue range", "turnover", "revenue usd")
	add(FTech, "technologies", "technology", "tech", "tech stack", "technology stack", "technographics", "technologies used")
	add(FSkills, "skills", "skill", "expertise")
	add(FProducts, "products", "product", "services", "products services", "products and services", "company products services", "company products")
	add(FKeywords, "keywords", "tags", "specialties", "specialities", "company keywords")
	// heavy or noisy vendor columns: ignored by default (the admin can still map them).
	// e.g. a 2 KB company description on 1 M rows is ~2 GB of disk and IOPS for no search value.
	add(FIgnore, "id", "company id", "lead id", "person id", "contact id", "record id", "company description", "description",
		"company address", "full address", "fiscal year", "stock exchange", "stock symbol", "stock price", "ebitda range", "interests")
}

// headerKey folds a header for alias lookup: "Company_Name " → "company name".
func headerKey(h string) string {
	f := normalize.Fold(h)
	var b strings.Builder
	for _, r := range f {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else {
			b.WriteByte(' ')
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// ColumnMap assigns one CSV column to a canonical field ("" = extra).
type ColumnMap struct {
	Header string `json:"header"`
	Field  string `json:"field"`
}

type Mapping []ColumnMap

// AutoMap maps headers by alias; unknown headers go to extra.
func AutoMap(headers []string) Mapping {
	m := make(Mapping, len(headers))
	for i, h := range headers {
		m[i] = ColumnMap{Header: h, Field: aliases[headerKey(dupSuffix.ReplaceAllString(h, ""))]}
	}
	return m
}

// dupSuffix matches the " (2)" CleanHeaders appends to repeated headers, so a
// second "Email" column is still an email column.
var dupSuffix = regexp.MustCompile(` \(\d+\)$`)

// Validate checks admin-supplied mappings.
func (m Mapping) Validate() error {
	known := map[string]bool{FExtra: true, FIgnore: true}
	for _, f := range Fields {
		known[f] = true
	}
	for _, c := range m {
		if !known[c.Field] {
			return &FieldError{c.Field}
		}
	}
	return nil
}

type FieldError struct{ Field string }

func (e *FieldError) Error() string { return "unknown mapping field: " + e.Field }

// index builds field → column indexes (in file order).
func (m Mapping) index() map[string][]int {
	idx := map[string][]int{}
	for i, c := range m {
		if c.Field != FIgnore {
			idx[c.Field] = append(idx[c.Field], i)
		}
	}
	return idx
}

// FieldOption is one choice in the admin mapping dropdown.
type FieldOption struct {
	Field string `json:"field"`
	Label string `json:"label"`
	Group string `json:"group"`
}

// FieldOptions lists mapping targets in display order.
var FieldOptions = []FieldOption{
	{FName, "Full name", "Person"}, {FFirst, "First name", "Person"}, {FLast, "Last name", "Person"},
	{FTitle, "Job title", "Person"}, {"seniority", "Seniority", "Person"}, {"division", "Department", "Person"},
	{"decision", "Decision power", "Person"}, {FLinkedIn, "LinkedIn profile URL", "Person"},
	{FEmail, "Email", "Contact"}, {"econf", "Email confidence / status", "Contact"}, {FDirect, "Direct dial", "Contact"},
	{FMobile, "Mobile", "Contact"}, {FPhone, "Phone", "Contact"}, {FWorkPhone, "Work phone", "Contact"},
	{FCoPhone, "Company phone", "Contact"},
	{FCompany, "Company name", "Company"}, {FWebsite, "Website / domain", "Company"}, {"industry", "Industry", "Company"},
	{"sector", "Sector / sub-industry", "Company"}, {"ctype", "Company type", "Company"}, {"csize", "Company size / employees", "Company"},
	{"revenue", "Revenue", "Company"}, {FTech, "Technologies (comma list)", "Company"}, {FProducts, "Products / services", "Company"},
	{"city", "City", "Location"}, {"state", "State / region", "Location"}, {"country", "Country", "Location"},
	{FSkills, "Skills", "Search text"}, {FKeywords, "Keywords / tags", "Search text"},
	{FExtra, "Keep as extra column", "Other"}, {FIgnore, "Ignore this column", "Other"},
}
