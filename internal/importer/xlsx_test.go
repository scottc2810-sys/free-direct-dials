package importer

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"markemind/dials/internal/emailcheck"
)

// writeXLSX builds a minimal workbook: one sheet, shared + inline strings,
// numbers stored as floats, a gap column and an empty row.
func writeXLSX(t *testing.T, rows int) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "book.xlsx")
	f, _ := os.Create(p)
	zw := zip.NewWriter(f)
	put := func(name, body string) {
		w, _ := zw.Create(name)
		io.WriteString(w, body)
	}
	put("xl/workbook.xml", `<?xml version="1.0"?><workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets><sheet name="Leads" sheetId="1" r:id="rId1"/><sheet name="Other" sheetId="2" r:id="rId2"/></sheets></workbook>`)
	put("xl/_rels/workbook.xml.rels", `<?xml version="1.0"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Target="worksheets/sheet1.xml"/><Relationship Id="rId2" Target="/xl/worksheets/sheet2.xml"/></Relationships>`)
	put("xl/sharedStrings.xml", `<?xml version="1.0"?><sst><si><t>Full Name</t></si><si><t>Work Email</t></si><si><t>Country</t></si><si><r><t>Direct </t></r><r><t>Dial</t></r><rPh><t>IGNORED</t></rPh></si><si><t>USA</t></si></sst>`)
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0"?><worksheet><sheetData>`)
	sb.WriteString(`<row r="1"><c r="A1" t="s"><v>0</v></c><c r="B1" t="s"><v>1</v></c><c r="C1" t="s"><v>2</v></c><c r="E1" t="s"><v>3</v></c></row>`)
	sb.WriteString(`<row r="2"></row>`)
	for i := 0; i < rows; i++ {
		r := i + 3
		fmt.Fprintf(&sb, `<row r="%d"><c r="A%d" t="inlineStr"><is><t>Person %d</t></is></c><c r="B%d" t="str"><f>x</f><v>p%d@acme.com</v></c><c r="C%d" t="s"><v>4</v></c><c r="E%d"><v>9.19876543210E+11</v></c></row>`,
			r, r, i, r, i, r, r)
	}
	sb.WriteString(`</sheetData></worksheet>`)
	put("xl/worksheets/sheet1.xml", sb.String())
	put("xl/worksheets/sheet2.xml", `<worksheet><sheetData><row><c t="inlineStr"><is><t>x</t></is></c></row></sheetData></worksheet>`)
	zw.Close()
	f.Close()
	return p
}

func TestXLSXImport(t *testing.T) {
	im, d := newImporter(t)
	p := writeXLSX(t, 25)
	src, err := OpenSource(p, t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if src.Kind != "xlsx" || len(src.Sheets) != 2 || src.Sheets[0].Name != "Leads" {
		t.Fatalf("source %+v", src)
	}
	st, err := im.Run(context.Background(), src, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if st.Added != 25 {
		t.Fatalf("stats %+v", st)
	}
	var phone string
	d.R.QueryRow(`SELECT phone FROM contact_info WHERE email_key='p3@acme.com'`).Scan(&phone)
	if phone != "919876543210" {
		t.Fatalf("numeric phone mangled: %q", phone)
	}
	if n := scalar(t, d, `SELECT cnt FROM dims WHERE dim=1 AND value_n='united states'`); n != 25 {
		t.Fatalf("country from shared strings: %d", n)
	}
}

func TestXLSXSpillsSharedStringsToDisk(t *testing.T) {
	s := &stringStore{dir: t.TempDir()}
	big := strings.Repeat("x", 1<<20)
	for i := 0; i < 40; i++ {
		if err := s.add(fmt.Sprintf("%d-%s", i, big)); err != nil {
			t.Fatal(err)
		}
	}
	if s.file == nil {
		t.Fatal("expected spill to disk past the memory budget")
	}
	v, _ := s.get(37)
	if !strings.HasPrefix(v, "37-") || len(v) != len(big)+3 {
		t.Fatalf("spilled read wrong: %q…", v[:5])
	}
	s.close()
}

func TestLegacyXLSRejected(t *testing.T) {
	p := filepath.Join(t.TempDir(), "old.xls")
	os.WriteFile(p, []byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1, 0, 0}, 0o600)
	if _, err := OpenSource(p, t.TempDir(), 0); err != ErrLegacyXLS {
		t.Fatalf("want ErrLegacyXLS, got %v", err)
	}
}

func TestDryRunShowsMappingResult(t *testing.T) {
	data := "Who,Mail,Where,Phone No,Notes\nAnn Lee,ann@mailinator.com,usa,12,hello\nBob,bob@acme.com,India,+91 98765 43210,\n,,,,\n"
	auto := AutoMap(CleanHeaders([]string{"Who", "Mail", "Where", "Phone No", "Notes"}))
	if auto[0].Field != "" || auto[1].Field != FEmail || auto[3].Field != FPhone {
		t.Fatalf("automap %+v", auto)
	}
	// the admin fixes the unknown headers by hand
	m := Mapping{{"Who", FName}, {"Mail", FEmail}, {"Where", "country"}, {"Phone No", FPhone}, {"Notes", FExtra}}
	res, err := DryRun(strings.NewReader(data), m, 50, emailcheck.NewDisposable())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 3 || res.Rows[2].Skip != "empty_row" {
		t.Fatalf("rows %+v", res.Rows)
	}
	ann := res.Rows[0]
	if ann.Fields["email"] != "" || ann.Fields["country"] != "usa" || ann.Extra["Notes"] != "hello" || len(ann.Notes) != 2 {
		t.Fatalf("ann %+v", ann)
	}
	if res.Rows[1].Fields["phone"] != "+91 98765 43210" {
		t.Fatalf("bob %+v", res.Rows[1])
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("unexpected warnings %v", res.Warnings)
	}
	if w := (Mapping{{"x", FTitle}}).Warnings(); len(w) < 3 {
		t.Fatalf("missing warnings: %v", w)
	}
	if Signature([]string{"A", "b "}) != Signature([]string{"a", "B"}) {
		t.Fatal("signature must ignore case/space")
	}
	if _, err := DryRun(strings.NewReader(data), Mapping{{"Who", "bogus"}}, 5, nil); err == nil {
		t.Fatal("bad mapping accepted")
	}
}
