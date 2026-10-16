package importer

import (
	"archive/zip"
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"markemind/dials/internal/emailcheck"
	"markemind/dials/internal/normalize"
)

// Shapes taken from a real Slintel export (values synthetic).
func TestSlintelShapes(t *testing.T) {
	if v := normalize.Value("'+14155550123"); v != "+14155550123" {
		t.Fatalf("Excel text marker kept: %q", v)
	}
	if v := normalize.Value("'Neil"); v != "'Neil" {
		t.Fatalf("apostrophe in text must stay: %q", v)
	}
	if normalize.Dim("csize", "0 - 9") != normalize.Dim("csize", "0-9") || normalize.Dim("revenue", "$1M - $5M") != "$1m-$5m" {
		t.Fatalf("range spacing: %q %q", normalize.Dim("csize", "0 - 9"), normalize.Dim("revenue", "$1M - $5M"))
	}
	got := techItems("Google Tag Manager|Tag Management,Gmail|Email Management,AWS Route53|Domain Name Services")
	if !reflect.DeepEqual(got, []string{"Google Tag Manager", "Gmail", "AWS Route53"}) {
		t.Fatalf("name|category tech list: %v", got)
	}
	if got := techItems("HubSpot|Salesforce|Zendesk"); len(got) != 3 {
		t.Fatalf("pipe-separated list: %v", got)
	}
	m := AutoMap([]string{"lead_titles", "lead_divison", "company_sector", "company_phone_numbers", "company_city",
		"company_products_services", "company_description", "id", "company_address", "decision_making_power"})
	want := []string{FTitle, "division", "sector", FCoPhone, "city", FProducts, FIgnore, FIgnore, FIgnore, "decision"}
	for i, w := range want {
		if m[i].Field != w {
			t.Errorf("%s → %q, want %q", m[i].Header, m[i].Field, w)
		}
	}
}

func TestPersonLocationWinsCompanyFills(t *testing.T) {
	im, d := newImporter(t)
	run(t, im, "name,email,city,company_city\nA,a@x-corp.com,Berkeley,Venice\nB,b@x-corp.com,,Venice\n")
	var a, b string
	d.R.QueryRow(`SELECT x.label FROM contacts c JOIN contact_info i ON i.id=c.id JOIN dims x ON x.id=c.city WHERE i.email_key='a@x-corp.com'`).Scan(&a)
	d.R.QueryRow(`SELECT x.label FROM contacts c JOIN contact_info i ON i.id=c.id JOIN dims x ON x.id=c.city WHERE i.email_key='b@x-corp.com'`).Scan(&b)
	if a != "Berkeley" || b != "Venice" {
		t.Fatalf("city a=%q b=%q", a, b)
	}
}

func TestXLSXDateStyledRangesRecovered(t *testing.T) {
	p := filepath.Join(t.TempDir(), "dates.xlsx")
	f, _ := os.Create(p)
	zw := zip.NewWriter(f)
	put := func(name, body string) { w, _ := zw.Create(name); io.WriteString(w, body) }
	put("xl/workbook.xml", `<workbook xmlns:r="r"><sheets><sheet name="S" sheetId="1" r:id="rId1"/></sheets></workbook>`)
	put("xl/_rels/workbook.xml.rels", `<Relationships><Relationship Id="rId1" Target="worksheets/sheet1.xml"/></Relationships>`)
	// xf 0 general, xf 1 builtin mmm-yy (17), xf 2 custom "d-mmm-yy", xf 3 custom quoted text that is not a date
	put("xl/styles.xml", `<styleSheet><numFmts><numFmt numFmtId="164" formatCode="d\-mmm\-yy"/><numFmt numFmtId="165" formatCode="&quot;day&quot;0"/></numFmts>
		<cellXfs><xf numFmtId="0"/><xf numFmtId="17"/><xf numFmtId="164"/><xf numFmtId="165"/></cellXfs></styleSheet>`)
	put("xl/worksheets/sheet1.xml", `<worksheet><sheetData>
		<row><c t="inlineStr"><is><t>name</t></is></c><c t="inlineStr"><is><t>email</t></is></c><c t="inlineStr"><is><t>company_size</t></is></c><c t="inlineStr"><is><t>joined</t></is></c><c t="inlineStr"><is><t>n</t></is></c></row>
		<row><c t="inlineStr"><is><t>Ann</t></is></c><c t="inlineStr"><is><t>ann@acme.io</t></is></c><c s="1"><v>43739</v></c><c s="2"><v>45580</v></c><c s="3"><v>12</v></c></row>
		</sheetData></worksheet>`)
	zw.Close()
	f.Close()
	src, err := OpenSource(p, t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	res, err := DryRun(src, nil, 5, emailcheck.NewDisposable())
	if err != nil {
		t.Fatal(err)
	}
	row := res.Rows[0]
	if row.Fields["csize"] != "10-19" {
		t.Fatalf("Excel-mangled range: %q (want 10-19)", row.Fields["csize"])
	}
	if row.Extra["joined"] != "2024-10-15" || row.Extra["n"] != "12" {
		t.Fatalf("dates/numbers: %v", row.Extra)
	}
}

func TestRealSampleIfPresent(t *testing.T) {
	p := os.Getenv("DIALS_SAMPLE_XLSX")
	if p == "" {
		t.Skip("set DIALS_SAMPLE_XLSX to run against a real vendor export")
	}
	im, d := newImporter(t)
	src, err := OpenSource(p, t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	st, err := im.Run(context.Background(), src, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("rows=%d added=%d updated=%d skipped=%d reasons=%v cleaned=%v", st.Rows, st.Added, st.Updated, st.Skipped, st.Reasons, st.Cleaned)
	var noTitle, badPhone, techs, sizes int
	d.R.QueryRow(`SELECT COUNT(*) FROM contact_info WHERE title=''`).Scan(&noTitle)
	d.R.QueryRow(`SELECT COUNT(*) FROM contact_info WHERE phone LIKE '''%'`).Scan(&badPhone)
	d.R.QueryRow(`SELECT COUNT(*) FROM tech_fts WHERE tech_fts MATCH '"google"*'`).Scan(&techs)
	d.R.QueryRow(`SELECT COUNT(*) FROM dims WHERE dim=10 AND id>12`).Scan(&sizes)
	rows, _ := d.R.Query(`SELECT label, cnt FROM dims WHERE dim=10 AND id>12 ORDER BY cnt DESC`)
	var labels []string
	for rows.Next() {
		var l string
		var n int
		rows.Scan(&l, &n)
		labels = append(labels, l)
	}
	rows.Close()
	t.Logf("missing titles=%d phones-with-apostrophe=%d contacts using google tech=%d company sizes=%v", noTitle, badPhone, techs, labels)
	var techRows, companies int64
	d.R.QueryRow(`SELECT COUNT(*) FROM tech_fts`).Scan(&techRows)
	d.R.QueryRow(`SELECT COUNT(DISTINCT lower(website)) FROM contact_info`).Scan(&companies)
	var pages, psize int64
	d.R.QueryRow(`PRAGMA page_count`).Scan(&pages)
	d.R.QueryRow(`PRAGMA page_size`).Scan(&psize)
	t.Logf("contacts with tech=%d (%.2f of contacts) companies=%d db=%d KB (%.1f KB per contact)", techRows, float64(techRows)/float64(st.Added),
		companies, pages*psize/1024, float64(pages*psize)/1024/float64(st.Added))
	if badPhone != 0 || noTitle > int(st.Added)/10 || techs == 0 {
		t.Fatal("sample import quality check failed")
	}
	for _, l := range labels {
		if strings.HasPrefix(l, "4") && len(l) == 5 {
			t.Fatalf("Excel date serial leaked into company size: %s", l)
		}
	}
}
