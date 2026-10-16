package importer

import (
	"bytes"
	"compress/gzip"
	"context"
	"strings"
	"testing"

	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/unicode"

	"markemind/dials/internal/db"
	"markemind/dials/internal/db/dbtest"
	"markemind/dials/internal/emailcheck"
)

func newImporter(t *testing.T) (*Importer, *db.DB) {
	d := dbtest.Open(t)
	return &Importer{DB: d, Disposable: emailcheck.NewDisposable()}, d
}

func run(t *testing.T, im *Importer, data string) Stats {
	t.Helper()
	st, err := im.Run(context.Background(), strings.NewReader(data), Options{BatchSize: 3})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	return st
}

func scalar(t *testing.T, d *db.DB, q string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := d.R.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

const basic = `Name,Email,Country,State,Company,Title,Technologies,Phone
Ann Lee,ann@acme.com,USA,CA,Acme,CEO,"Salesforce, HubSpot",+1 555 010 2030
Bob Ray,bob@acme.com,usa,N/A,Acme,CTO,,123
Cy Dee,cy@beta.io, USA ,,Beta,VP Sales,AWS,
Di Eve,di@mailinator.com,United States,NY,Gamma,Founder,,+1 555 999 0000
Ed Fox,not-an-email,India,MH,Delta,CFO,,
`

func TestAcceptance1VariantsAreOneOption(t *testing.T) {
	im, d := newImporter(t)
	st := run(t, im, basic)
	if st.Added != 5 || st.Skipped != 0 {
		t.Fatalf("stats %+v", st)
	}
	if n := scalar(t, d, `SELECT count(*) FROM dims WHERE dim=1 AND id>12`); n != 2 {
		t.Fatalf("want 2 countries (united states, india), got %d", n)
	}
	us := scalar(t, d, `SELECT cnt FROM dims WHERE dim=1 AND value_n='united states'`)
	if us != 4 {
		t.Fatalf("united states count = %d, want 4 (sum of variants)", us)
	}
	var label string
	d.R.QueryRow(`SELECT label FROM dims WHERE dim=1 AND value_n='united states'`).Scan(&label)
	if label != "USA" {
		t.Fatalf("label = %q, want most common spelling USA", label)
	}
	// N/A state → blank id 2
	if n := scalar(t, d, `SELECT count(*) FROM contacts WHERE state=2`); n != 2 {
		t.Fatalf("blank states = %d, want 2", n)
	}
	// disposable + invalid emails dropped, rows kept, has_email=0
	if n := scalar(t, d, `SELECT count(*) FROM contacts WHERE has_email=1`); n != 3 {
		t.Fatalf("has_email = %d, want 3", n)
	}
	if st.Cleaned["email_disposable"] != 1 || st.Cleaned["email_invalid"] != 1 || st.Cleaned["phone_invalid"] != 1 {
		t.Fatalf("cleaned %+v", st.Cleaned)
	}
	if n := scalar(t, d, `SELECT count(*) FROM tech_fts WHERE tech_fts MATCH '"hubspot"*'`); n != 1 {
		t.Fatalf("tech rows %d", n)
	}
	if n := scalar(t, d, `SELECT count(*) FROM fts WHERE fts MATCH '"ac"*'`); n != 2 {
		t.Fatalf("fts prefix 'ac' matched %d, want 2 (Acme)", n)
	}
}

func TestAcceptance10ReuploadIsIdempotent(t *testing.T) {
	im, d := newImporter(t)
	first := run(t, im, basic)
	v1 := d.DatasetVersion()
	second := run(t, im, basic)
	if second.Added != 0 || second.Updated != 0 || second.Unchanged != first.Added {
		t.Fatalf("re-upload: %+v (first added %d)", second, first.Added)
	}
	if scalar(t, d, `SELECT count(*) FROM contacts`) != 5 {
		t.Fatal("duplicates created")
	}
	if d.DatasetVersion() <= v1 {
		t.Fatal("dataset version not bumped")
	}
}

func TestMergeLaterRowWinsNonEmpty(t *testing.T) {
	im, d := newImporter(t)
	run(t, im, "Name,Email,Title,Country,Person Linkedin Url\nAnn Lee,ann@acme.com,CEO,India,\n")
	st := run(t, im, "Name,Email,Title,Country,Person Linkedin Url\nAnn Lee,ann@acme.com,,USA,linkedin.com/in/annlee\n")
	if st.Updated != 1 || st.Added != 0 {
		t.Fatalf("stats %+v", st)
	}
	var title, li string
	d.R.QueryRow(`SELECT title, li_key FROM contact_info`).Scan(&title, &li)
	if title != "CEO" || li != "in/annlee" {
		t.Fatalf("title=%q li=%q: empty must not overwrite, new key must be set", title, li)
	}
	// a later row with only LinkedIn (no email) merges into the same person
	st = run(t, im, "Name,Email,Title,Person Linkedin Url\nAnn Lee,,Chair,https://www.linkedin.com/in/AnnLee/\n")
	if st.Updated != 1 || scalar(t, d, `SELECT count(*) FROM contacts`) != 1 {
		t.Fatalf("linkedin merge failed: %+v", st)
	}
	// no email, no LinkedIn: hash identity prevents duplicates on re-upload
	run(t, im, "Name,Company,Title,Phone\nZed Q,Omega,Dev,+44 20 7946 0000\n")
	st = run(t, im, "Name,Company,Title,Phone\nZed Q,Omega,Dev,+44 20 7946 0000\n")
	if st.Unchanged != 1 || scalar(t, d, `SELECT count(*) FROM contacts`) != 2 {
		t.Fatalf("hash identity: %+v", st)
	}
}

func TestEncodingsAndDelimiters(t *testing.T) {
	csvText := "Name;Email;City\n\"René, Jr.\";rene@acme.fr;Montréal\n\"Multi\nLine\";ml@acme.fr;Paris\n"
	utf16 := func(be bool) string {
		e := unicode.UTF16(unicode.LittleEndian, unicode.UseBOM)
		if be {
			e = unicode.UTF16(unicode.BigEndian, unicode.UseBOM)
		}
		b, _ := e.NewEncoder().Bytes([]byte(csvText))
		return string(b)
	}
	cp, _ := charmap.Windows1252.NewEncoder().String(csvText)
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write([]byte(csvText))
	zw.Close()
	cases := map[string]string{
		"utf8":    csvText,
		"bom":     "\xef\xbb\xbf" + csvText,
		"utf16le": utf16(false),
		"utf16be": utf16(true),
		"cp1252":  cp,
		"gzip":    gz.String(),
		"tabs":    strings.ReplaceAll(csvText, ";", "\t"),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			im, d := newImporter(t)
			st := run(t, im, data)
			if st.Added != 2 {
				t.Fatalf("%s: stats %+v", name, st)
			}
			var n string
			d.R.QueryRow(`SELECT label FROM dims WHERE dim=3 AND value_n='montreal'`).Scan(&n)
			if n != "Montréal" {
				t.Fatalf("%s: city label %q (sniffed %+v)", name, n, st.Sniffed)
			}
		})
	}
}

func TestRaggedRowsDuplicateHeadersAndSkips(t *testing.T) {
	im, d := newImporter(t)
	data := "Name,Email,Email,Notes,\n" +
		"Ann,ann@x-corp.com\n" + // short row
		",,,,\n" + // empty row → skipped
		"Bob,,bob@y-corp.com,hello,extra,overflow\n" // long row; second Email column used
	st := run(t, im, data)
	if st.Added != 2 || st.Reasons["empty_row"] != 1 {
		t.Fatalf("stats %+v", st)
	}
	var extra []byte
	d.R.QueryRow(`SELECT extra FROM contact_info WHERE email_key='bob@y-corp.com'`).Scan(&extra)
	if DecodeExtra(extra)["Notes"] != "hello" {
		t.Fatalf("extra = %v", DecodeExtra(extra))
	}
}

func TestSuppressedRowsSkipped(t *testing.T) {
	im, d := newImporter(t)
	for _, k := range SuppressKeys("gone@acme.com", "", "") {
		d.W.Exec(`INSERT INTO suppress(k, ts) VALUES(?, 0)`, k)
	}
	st := run(t, im, "Name,Email\nGone Person,GONE@acme.com\nStay,stay@acme.com\n")
	if st.Added != 1 || st.Reasons["suppressed"] != 1 {
		t.Fatalf("stats %+v", st)
	}
}

func TestExtraRoundTrip(t *testing.T) {
	big := map[string]string{"Zip": "12345", "Notes": strings.Repeat("long text ", 40)}
	if got := DecodeExtra(EncodeExtra(big)); got["Notes"] != big["Notes"] || got["Zip"] != "12345" {
		t.Fatal("zstd extra round trip failed")
	}
	if b := EncodeExtra(map[string]string{"a": "b"}); b[0] != 0 {
		t.Fatal("small extra should stay raw")
	}
	if len(DecodeExtra(nil)) != 0 || len(DecodeExtra([]byte{1, 2, 3})) != 0 {
		t.Fatal("corrupt extra must decode to empty")
	}
}

func TestGeneratedFileImports(t *testing.T) {
	im, d := newImporter(t)
	var buf bytes.Buffer
	if err := Generate(&buf, 3000, 7); err != nil {
		t.Fatal(err)
	}
	st, err := im.Run(context.Background(), &buf, Options{})
	if err != nil {
		t.Fatal(err)
	}
	total := scalar(t, d, `SELECT count(*) FROM contacts`)
	if st.Added != total || st.Added+st.Updated+st.Unchanged+st.Skipped != st.Rows || st.Rows != 3000 {
		t.Fatalf("accounting mismatch: %+v total=%d", st, total)
	}
	// every dimension's counts add up to the table size
	for dim := 1; dim <= 12; dim++ {
		if s := scalar(t, d, `SELECT SUM(cnt) FROM dims WHERE dim=?`, dim); s != total {
			t.Fatalf("dim %d counts sum %d != %d", dim, s, total)
		}
	}
	h := LoadStats(context.Background(), d.R)
	if h.Contacts != total || h.Countries < 10 {
		t.Fatalf("headline stats %+v", h)
	}
}

func TestDeleteSuppressesAndFixesCounts(t *testing.T) {
	im, d := newImporter(t)
	run(t, im, basic)
	ctx := context.Background()
	ids, err := FindByIdentity(ctx, d.R, "email", "ANN@acme.com")
	if err != nil || len(ids) != 1 {
		t.Fatalf("find: %v %v", ids, err)
	}
	usBefore := scalar(t, d, `SELECT cnt FROM dims WHERE dim=1 AND value_n='united states'`)
	if n, err := DeleteContacts(ctx, d, ids, 1); err != nil || n != 1 {
		t.Fatalf("delete: %d %v", n, err)
	}
	if scalar(t, d, `SELECT cnt FROM dims WHERE dim=1 AND value_n='united states'`) != usBefore-1 {
		t.Fatal("dims count not decremented")
	}
	if LoadStats(ctx, d.R).Contacts != 4 {
		t.Fatal("headline stats not refreshed")
	}
	st := run(t, im, basic) // re-upload: the removed person must not come back
	if st.Reasons["suppressed"] != 1 || scalar(t, d, `SELECT count(*) FROM contacts`) != 4 {
		t.Fatalf("suppression failed: %+v", st)
	}
	phoneIDs, _ := FindByIdentity(ctx, d.R, "phone", "(555) 999-0000")
	if len(phoneIDs) != 1 {
		t.Fatalf("phone lookup: %v", phoneIDs)
	}
}
