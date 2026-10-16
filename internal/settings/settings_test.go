package settings

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"markemind/dials/internal/db/dbtest"
	"markemind/dials/internal/platform"
)

func newStore(t *testing.T) *Store {
	s := &Store{DB: dbtest.Open(t), Clock: platform.NewFakeClock(time.Unix(1_800_000_000, 0))}
	if err := s.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestDefaultsValid(t *testing.T) {
	a := Defaults()
	if err := a.Validate(); err != nil {
		t.Fatalf("defaults invalid: %v", err)
	}
	if a.Donations.Wallets[0].Address != "0x6DB11d0A652De0Ee57EDAdDfa70e5daC87D33BFb" && !strings.EqualFold(a.Donations.Wallets[0].Address, DefaultWallet) {
		t.Fatal("default wallet lost")
	}
	if a.Ads.SmartLink != DefaultSmartLink || a.Ads.Mode != "sponsor" {
		t.Fatal("ad defaults")
	}
}

func TestSaveDonationsAndPublic(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	body := `{"enabled":true,"heading":"Support us","message":"Thanks!",
		"upi":{"enabled":true,"vpa":"MarkeMind@okicici","name":"MarkeMind"},
		"paypal":{"enabled":true,"url":"markemind"},
		"link":{"enabled":false,"label":"","url":""},
		"wallets":[{"kind":"evm","address":"0x6db11d0a652de0ee57edaddfa70e5dac87d33bfb","networks":["ethereum","base"],"tokens":"ETH, USDC","label":"EVM","enabled":true}]}`
	a, err := s.Save(ctx, "donations", []byte(body), "boss")
	if err != nil {
		t.Fatal(err)
	}
	if a.Donations.UPI.VPA != "markemind@okicici" || a.Donations.PayPal.URL != "https://paypal.me/markemind" {
		t.Fatalf("normalisation: %+v", a.Donations)
	}
	var pub Public
	json.Unmarshal(s.PublicJSON(), &pub)
	if pub.Donations.UPI == nil || !strings.HasPrefix(pub.Donations.UPI.URL, "upi://pay?") || !strings.HasPrefix(pub.Donations.UPI.QR, "<svg") {
		t.Fatalf("public upi: %+v", pub.Donations.UPI)
	}
	if len(pub.Donations.Wallets) != 1 || len(pub.Donations.Wallets[0].Networks) != 2 || pub.Donations.Wallets[0].QR == "" {
		t.Fatalf("public wallets: %+v", pub.Donations.Wallets)
	}
	// persisted and reloaded
	s2 := &Store{DB: s.DB, Clock: s.Clock}
	s2.Load(ctx)
	if s2.Get().Donations.UPI.VPA != "markemind@okicici" {
		t.Fatal("not persisted")
	}
	var audits int
	s.DB.R.QueryRow(`SELECT count(*) FROM audit WHERE action='settings.donations' AND actor='boss'`).Scan(&audits)
	if audits != 1 {
		t.Fatal("audit missing")
	}
}

func TestRejectsBadInput(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	bad := map[string]string{
		"donations": `{"enabled":true,"wallets":[{"kind":"evm","address":"0x5AAeb6053F3E94C9b9A09f33669435E7Ef1BeAed","networks":["ethereum"],"enabled":true}]}`,
		"whatsapp":  `{"enabled":true,"number":"12","message":"hi","label":"x"}`,
		"ads":       `{"mode":"popunder","smartlink":"","min_seconds":10,"adblock":"nudge","slots":{}}`,
		"limits":    `{"contacts_per_ad":500,"max_ads_per_day":50,"max_ads_per_day_ip":150,"page_size":50,"preview_pages":5,"signups_enabled":true}`,
		"site":      `{"consent_mode":"all","unknown_field":1}`,
	}
	for sec, body := range bad {
		if _, err := s.Save(ctx, sec, []byte(body), "boss"); err == nil {
			t.Errorf("%s accepted bad input", sec)
		}
	}
	if s.Get().Ads.Mode != "sponsor" {
		t.Fatal("failed save must not change current settings")
	}
	// network/kind mismatch
	if _, err := s.Save(ctx, "donations", []byte(`{"enabled":true,"wallets":[{"kind":"evm","address":"0x6db11d0a652de0ee57edaddfa70e5dac87d33bfb","networks":["bitcoin"],"enabled":true}]}`), "boss"); err == nil {
		t.Fatal("evm address on bitcoin network accepted")
	}
}

func TestWhatsAppAndGlobalSlot(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if _, err := s.Save(ctx, "whatsapp", []byte(`{"enabled":true,"number":"+91 98765-43210","message":"Hi","label":"Bulk data?"}`), "boss"); err != nil {
		t.Fatal(err)
	}
	var pub Public
	json.Unmarshal(s.PublicJSON(), &pub)
	if pub.WhatsApp == nil || pub.WhatsApp.URL != "https://wa.me/919876543210" {
		t.Fatalf("whatsapp: %+v", pub.WhatsApp)
	}
	a, err := s.Save(ctx, "ads", []byte(`{"mode":"smartlink","smartlink":"https://asiafilm.org/4/da200810262f12a03472e20ac1c98e2e","min_seconds":12,"adblock":"allow",
		"slots":{"global":{"enabled":true,"html":"<script src=x></script>","isolation":"sandbox"},"top":{"enabled":true,"html":"","isolation":"sandbox","height":90}}}`), "boss")
	if err != nil {
		t.Fatal(err)
	}
	if a.Ads.Slots["global"].Isolation != "direct" || a.Ads.Slots["top"].Enabled {
		t.Fatalf("slot normalisation: %+v", a.Ads.Slots)
	}
}

func TestSmartLinkPlacements(t *testing.T) {
	s := newStore(t)
	var pub Public
	json.Unmarshal(s.PublicJSON(), &pub)
	if !pub.Ads.Placements["header"] || !pub.Ads.Placements["slot_fallback"] || pub.Ads.SmartLink != DefaultSmartLink || pub.Ads.Label != "Sponsored" {
		t.Fatalf("default placements: %+v", pub.Ads)
	}
	_, err := s.Save(context.Background(), "ads", []byte(`{"mode":"sponsor","smartlink":"https://asiafilm.org/4/da200810262f12a03472e20ac1c98e2e","min_seconds":10,"adblock":"nudge","slots":{},
		"placements":{"header":false,"results":true},"sponsor_label":"","sponsor_cta":"Visit sponsor"}`), "boss")
	if err != nil {
		t.Fatal(err)
	}
	pub = Public{} // json.Unmarshal merges into existing maps: decode into a fresh value
	json.Unmarshal(s.PublicJSON(), &pub)
	if pub.Ads.Placements["header"] || !pub.Ads.Placements["results"] || pub.Ads.Label != "Sponsored" {
		t.Fatalf("placements after save: %+v (label must never be empty)", pub.Ads)
	}
	if _, err := s.Save(context.Background(), "ads", []byte(`{"mode":"sponsor","smartlink":"","min_seconds":10,"adblock":"nudge","slots":{},"placements":{"popunder":true}}`), "boss"); err == nil {
		t.Fatal("unknown placement accepted")
	}
}
