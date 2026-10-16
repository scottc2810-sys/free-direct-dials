package seo

import (
	"bytes"
	"context"
	"testing"
	"time"

	"markemind/dials/internal/db/dbtest"
	"markemind/dials/internal/emailcheck"
	"markemind/dials/internal/importer"
	"markemind/dials/internal/platform"
)

func TestRefreshSelectsAndRetires(t *testing.T) {
	d := dbtest.Open(t)
	var buf bytes.Buffer
	importer.Generate(&buf, 3000, 5)
	im := &importer.Importer{DB: d, Disposable: emailcheck.NewDisposable()}
	if _, err := im.Run(context.Background(), &buf, importer.Options{}); err != nil {
		t.Fatal(err)
	}
	clk := platform.NewFakeClock(time.Unix(1_800_000_000, 0))
	ctx := context.Background()
	n, err := Refresh(ctx, d, clk, 50, 25)
	if err != nil || n == 0 {
		t.Fatalf("refresh: %d %v", n, err)
	}
	p, ok := Lookup(ctx, d.R, "country/united-states")
	if !ok || p.Gone || p.Count < 25 {
		t.Fatalf("country page: %+v %v", p, ok)
	}
	if _, ok := Lookup(ctx, d.R, "country/united-states/software"); !ok {
		t.Fatal("combo page missing")
	}
	if _, ok := Lookup(ctx, d.R, "industry/information-technology-and-services"); !ok {
		t.Fatal("industry page with & in label missing")
	}
	// raising the threshold retires small pages as gone (410), not 404
	if _, err := Refresh(ctx, d, clk, 50, 1_000_000); err != nil {
		t.Fatal(err)
	}
	p, ok = Lookup(ctx, d.R, "country/united-states")
	if !ok || !p.Gone {
		t.Fatalf("retired page should be gone: %+v", p)
	}
	live, _ := List(ctx, d.R, "", 0, 0, 100)
	if len(live) != 0 {
		t.Fatalf("no live pages expected, got %d", len(live))
	}
}
