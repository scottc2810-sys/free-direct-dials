// Package settings holds everything the superadmin can change at runtime
// (ads, affiliates, donations, WhatsApp, limits, site/legal) with strict validation, an
// in-memory snapshot for hot paths and a public projection for /api/config.
package settings

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync/atomic"

	"markemind/dials/internal/db"
	"markemind/dials/internal/emailcheck"
	"markemind/dials/internal/payments"
	"markemind/dials/internal/platform"
)

const DefaultSmartLink = "https://asiafilm.org/4/da200810262f12a03472e20ac1c98e2e"
const DefaultWallet = "0x6db11d0a652de0ee57edaddfa70e5dac87d33bfb"
const DefaultWhatsApp = "919834346179" // +91 98343 46179

var SlotNames = []string{"top", "incontent", "sponsor", "footer", "global"}

type Slot struct {
	Enabled    bool   `json:"enabled"`
	HTML       string `json:"html"`        // desktop / default snippet
	HTMLMobile string `json:"html_mobile"` // optional narrow-screen snippet (e.g. 320×50)
	Isolation  string `json:"isolation"`   // sandbox | direct
	Height     int    `json:"height"`      // reserved px (no layout shift)
	HeightM    int    `json:"height_mobile"`
}

type Ads struct {
	Mode       string          `json:"mode"` // sponsor | smartlink | gpt_rewarded
	SmartLink  string          `json:"smartlink"`
	MinSeconds int             `json:"min_seconds"`
	Adblock    string          `json:"adblock"`   // allow | nudge | require
	ProbeURL   string          `json:"probe_url"` // an ad script URL used to detect blockers
	Slots      map[string]Slot `json:"slots"`

	// Smart Link placements (always labelled, rel="sponsored", new tab).
	Placements   map[string]bool `json:"placements"`
	SponsorLabel string          `json:"sponsor_label"`
	SponsorCTA   string          `json:"sponsor_cta"`

	// Google Ad Manager rewarded ad unit (mode gpt_rewarded), e.g. /1234567/fdd_rewarded.
	// When Google has no ad to show, the visitor gets the sponsor break instead.
	GPTAdUnit string `json:"gpt_ad_unit"`

	// Each further page of masked preview rows opens after a short sponsor break.
	PreviewGate    bool `json:"preview_gate"`
	PreviewSeconds int  `json:"preview_seconds"`

	// ads.txt lines (Authorized Digital Sellers), served at /ads.txt.
	AdsTxt string `json:"ads_txt"`
}

// Affiliate is one partner offer (FTC/ASCI: always labelled as a partner link).
type Affiliate struct {
	ID         string   `json:"id"`
	Enabled    bool     `json:"enabled"`
	Name       string   `json:"name"` // brand, e.g. "Apollo.io"
	Text       string   `json:"text"` // one-line pitch
	CTA        string   `json:"cta"`  // button text
	URL        string   `json:"url"`  // your tracking link (https)
	Placements []string `json:"placements"`
	Weight     int      `json:"weight"` // relative frequency when several fit one place
}

type Affiliates struct {
	Enabled bool        `json:"enabled"`
	Label   string      `json:"label"` // disclosure shown on every card
	Items   []Affiliate `json:"items"`
}

// AffiliatePlacements are where partner cards can appear.
var AffiliatePlacements = []string{"results", "dock", "empty", "break", "footer", "seo"}

// PlacementNames are the places a labelled Smart Link may appear.
var PlacementNames = []string{"header", "sponsor_button", "results", "empty_states", "slot_fallback", "footer", "seo_pages"}

type Wallet struct {
	ID       string   `json:"id"`
	Kind     string   `json:"kind"` // evm | btc | sol | tron
	Address  string   `json:"address"`
	Networks []string `json:"networks"`
	Tokens   string   `json:"tokens"`
	Label    string   `json:"label"`
	Enabled  bool     `json:"enabled"`
}

type Donations struct {
	Enabled bool   `json:"enabled"`
	Heading string `json:"heading"`
	Message string `json:"message"`
	UPI     struct {
		Enabled bool   `json:"enabled"`
		VPA     string `json:"vpa"`
		Name    string `json:"name"`
	} `json:"upi"`
	PayPal struct {
		Enabled bool   `json:"enabled"`
		URL     string `json:"url"`
	} `json:"paypal"`
	Link struct { // Buy Me a Coffee / Ko-fi / any https link
		Enabled bool   `json:"enabled"`
		Label   string `json:"label"`
		URL     string `json:"url"`
	} `json:"link"`
	Wallets []Wallet `json:"wallets"`
}

type WhatsApp struct {
	Enabled bool   `json:"enabled"`
	Number  string `json:"number"` // digits with country code
	Message string `json:"message"`
	Label   string `json:"label"`
}

type Limits struct {
	ContactsPerAd        int  `json:"contacts_per_ad"`
	MaxAdsPerDay         int  `json:"max_ads_per_day"`
	MaxAdsPerDayIP       int  `json:"max_ads_per_day_ip"`
	PageSize             int  `json:"page_size"`     // unlocked rows per page for signed-in visitors
	PreviewPages         int  `json:"preview_pages"` // masked pages anyone can browse per filter
	SignupsEnabled       bool `json:"signups_enabled"`
	RequireBusinessEmail bool `json:"require_business_email"`
	UseReacher           bool `json:"use_reacher"`
}

type Site struct {
	Announcement     string `json:"announcement"`
	ReadOnly         bool   `json:"read_only"` // maintenance: browsing works, unlocks/sign-ups paused
	LegalName        string `json:"legal_name"`
	Address          string `json:"address"`
	ContactEmail     string `json:"contact_email"`
	ConsentMode      string `json:"consent_mode"` // all | eu_only | off
	SubscribeDefault bool   `json:"subscribe_default"`
}

type All struct {
	Ads        Ads        `json:"ads"`
	Affiliates Affiliates `json:"affiliates"`
	Donations  Donations  `json:"donations"`
	WhatsApp   WhatsApp   `json:"whatsapp"`
	Limits     Limits     `json:"limits"`
	Site       Site       `json:"site"`
}

func Defaults() All {
	var a All
	a.Ads = Ads{Mode: "sponsor", SmartLink: DefaultSmartLink, MinSeconds: 10, Adblock: "nudge", Slots: map[string]Slot{}}
	for _, n := range SlotNames {
		a.Ads.Slots[n] = Slot{Isolation: "sandbox", Height: defaultHeight(n), HeightM: defaultHeightM(n)}
	}
	a.Ads.Slots["global"] = Slot{Isolation: "direct"}
	a.Ads.Placements = map[string]bool{}
	for _, n := range PlacementNames {
		a.Ads.Placements[n] = true
	}
	a.Ads.SponsorLabel = "Sponsored"
	a.Ads.SponsorCTA = "See today's sponsor offer"
	a.Ads.PreviewGate, a.Ads.PreviewSeconds = true, 5
	a.Affiliates = Affiliates{Enabled: true, Label: "Partner offer", Items: []Affiliate{
		{ID: "apollo", Name: "Apollo.io", Text: "Need more verified emails? Apollo has 275M+ contacts with a free plan.",
			CTA: "Try Apollo free", URL: "https://www.apollo.io/", Placements: []string{"results", "empty"}, Weight: 50},
		{ID: "lusha", Name: "Lusha", Text: "Find direct dials from LinkedIn in one click with Lusha's browser extension.",
			CTA: "Get Lusha free", URL: "https://www.lusha.com/", Placements: []string{"dock", "empty"}, Weight: 50},
	}}
	a.Donations.Enabled = true
	a.Donations.Heading = "Keep this directory free"
	a.Donations.Message = "Every contact here is free. If it saved you time, a small tip keeps the servers and data refreshes running."
	a.Donations.Link.Label = "Buy me a coffee"
	a.Donations.Wallets = []Wallet{{ID: "evm-main", Kind: "evm", Address: DefaultWallet,
		Networks: []string{"ethereum", "bsc", "polygon", "base"}, Tokens: "ETH, BNB, POL, USDT, USDC", Label: "Crypto (EVM)", Enabled: true}}
	a.WhatsApp = WhatsApp{Enabled: true, Number: DefaultWhatsApp, Message: "Hi! I'm looking for custom / bulk B2B contact data.", Label: "Need bulk or custom data?"}
	a.Limits = Limits{ContactsPerAd: 10, MaxAdsPerDay: 50, MaxAdsPerDayIP: 150, PageSize: 50, PreviewPages: 5, SignupsEnabled: true}
	a.Site = Site{ConsentMode: "eu_only", SubscribeDefault: true, LegalName: "MarkeMind AI"}
	return a
}

func defaultHeight(n string) int {
	switch n {
	case "top", "footer":
		return 90
	case "incontent", "sponsor":
		return 250
	}
	return 0
}

func defaultHeightM(n string) int {
	switch n {
	case "top", "footer":
		return 50
	case "incontent", "sponsor":
		return 250
	}
	return 0
}

var (
	digitsRe  = regexp.MustCompile(`^[0-9]{8,15}$`)
	walletIDs = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)
	gptUnitRe = regexp.MustCompile(`^/[0-9]{3,20}(,[0-9]{3,20})?(/[A-Za-z0-9_.\-]{1,100}){1,5}$`)
)

func oneOf(v string, opts ...string) bool {
	for _, o := range opts {
		if v == o {
			return true
		}
	}
	return false
}

func limitLen(errs *[]error, field, v string, n int) string {
	v = strings.TrimSpace(v)
	if len(v) > n {
		*errs = append(*errs, fmt.Errorf("%s is too long (max %d)", field, n))
	}
	return v
}

// Validate normalises and checks every section.
func (a *All) Validate() error {
	var errs []error
	ad := &a.Ads
	if !oneOf(ad.Mode, "sponsor", "smartlink", "gpt_rewarded") {
		errs = append(errs, errors.New("ads.mode must be sponsor, smartlink or gpt_rewarded"))
	}
	ad.GPTAdUnit = strings.TrimSpace(ad.GPTAdUnit)
	if ad.GPTAdUnit != "" && !gptUnitRe.MatchString(ad.GPTAdUnit) {
		errs = append(errs, errors.New("the Ad Manager ad unit must look like /1234567/your_rewarded_unit"))
	} else if ad.Mode == "gpt_rewarded" && ad.GPTAdUnit == "" {
		errs = append(errs, errors.New("rewarded video mode needs a Google Ad Manager rewarded ad unit path"))
	}
	if ad.PreviewSeconds < 0 || ad.PreviewSeconds > 30 {
		errs = append(errs, errors.New("preview break must be 0–30 seconds"))
	}
	ad.AdsTxt = cleanAdsTxt(&errs, ad.AdsTxt)
	if ad.SmartLink != "" {
		if u, err := payments.HTTPSURL(ad.SmartLink); err != nil {
			errs = append(errs, fmt.Errorf("ads.smartlink: %w", err))
		} else {
			ad.SmartLink = u
		}
	} else if ad.Mode == "smartlink" {
		errs = append(errs, errors.New("smartlink mode needs a Smart Link URL"))
	}
	if ad.MinSeconds < 5 || ad.MinSeconds > 60 {
		errs = append(errs, errors.New("ads.min_seconds must be 5–60"))
	}
	if !oneOf(ad.Adblock, "allow", "nudge", "require") {
		errs = append(errs, errors.New("ads.adblock must be allow, nudge or require"))
	}
	if ad.ProbeURL != "" {
		if u, err := payments.HTTPSURL(ad.ProbeURL); err != nil {
			errs = append(errs, fmt.Errorf("ads.probe_url: %w", err))
		} else {
			ad.ProbeURL = u
		}
	}
	if ad.Slots == nil {
		ad.Slots = map[string]Slot{}
	}
	for k, s := range ad.Slots {
		if !oneOf(k, SlotNames...) {
			errs = append(errs, fmt.Errorf("unknown ad slot %q", k))
			continue
		}
		if len(s.HTML) > 20000 || len(s.HTMLMobile) > 20000 {
			errs = append(errs, fmt.Errorf("ad slot %s: snippet over 20 KB", k))
		}
		if s.Isolation == "" {
			s.Isolation = "sandbox"
		}
		if k == "global" {
			s.Isolation = "direct" // overlays (Social Bar) must run in the page
		}
		if !oneOf(s.Isolation, "sandbox", "direct") {
			errs = append(errs, fmt.Errorf("ad slot %s: isolation must be sandbox or direct", k))
		}
		if s.Height < 0 || s.Height > 600 || s.HeightM < 0 || s.HeightM > 600 {
			errs = append(errs, fmt.Errorf("ad slot %s: height must be 0–600", k))
		}
		if strings.TrimSpace(s.HTML) == "" && strings.TrimSpace(s.HTMLMobile) == "" {
			s.Enabled = false
		}
		ad.Slots[k] = s
	}

	if ad.Placements == nil {
		ad.Placements = map[string]bool{}
	}
	for k := range ad.Placements {
		if !oneOf(k, PlacementNames...) {
			errs = append(errs, fmt.Errorf("unknown Smart Link placement %q", k))
		}
	}
	ad.SponsorLabel = limitLen(&errs, "sponsor label", ad.SponsorLabel, 30)
	if ad.SponsorLabel == "" {
		ad.SponsorLabel = "Sponsored" // disclosure is mandatory: it can be reworded, not removed
	}
	ad.SponsorCTA = limitLen(&errs, "sponsor button text", ad.SponsorCTA, 80)
	if ad.SponsorCTA == "" {
		ad.SponsorCTA = "See today's sponsor offer"
	}

	af := &a.Affiliates
	af.Label = limitLen(&errs, "partner label", af.Label, 30)
	if af.Label == "" {
		af.Label = "Partner offer" // disclosure is mandatory: it can be reworded, not removed
	}
	if len(af.Items) > 20 {
		errs = append(errs, errors.New("at most 20 partner offers"))
	}
	ids := map[string]bool{}
	for i := range af.Items {
		it := &af.Items[i]
		if it.ID == "" {
			it.ID = "p" + strings.ToLower(platform.Token(4))
			it.ID = strings.Map(func(r rune) rune {
				if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
					return r
				}
				return 'x'
			}, it.ID)
		}
		if !walletIDs.MatchString(it.ID) || ids[it.ID] {
			errs = append(errs, fmt.Errorf("partner offer %d: bad or duplicate id", i+1))
		}
		ids[it.ID] = true
		it.Name = limitLen(&errs, "partner name", it.Name, 40)
		it.Text = limitLen(&errs, "partner text", it.Text, 160)
		it.CTA = limitLen(&errs, "partner button", it.CTA, 40)
		if it.Name == "" {
			errs = append(errs, fmt.Errorf("partner offer %d needs a name", i+1))
		}
		if it.CTA == "" {
			it.CTA = "Learn more"
		}
		if u, err := payments.HTTPSURL(it.URL); err != nil {
			errs = append(errs, fmt.Errorf("partner %q link: %w", it.Name, err))
		} else {
			it.URL = u
		}
		seen := map[string]bool{}
		var pl []string
		for _, p := range it.Placements {
			if !oneOf(p, AffiliatePlacements...) {
				errs = append(errs, fmt.Errorf("partner %q: unknown placement %q", it.Name, p))
				continue
			}
			if !seen[p] {
				seen[p] = true
				pl = append(pl, p)
			}
		}
		it.Placements = pl
		if it.Weight <= 0 {
			it.Weight = 50
		}
		if it.Weight > 100 {
			it.Weight = 100
		}
	}

	d := &a.Donations
	d.Heading = limitLen(&errs, "donations.heading", d.Heading, 80)
	d.Message = limitLen(&errs, "donations.message", d.Message, 400)
	if d.UPI.VPA != "" {
		if v, err := payments.ValidateUPI(d.UPI.VPA); err != nil {
			errs = append(errs, err)
		} else {
			d.UPI.VPA = v
		}
	} else if d.UPI.Enabled {
		errs = append(errs, errors.New("UPI is enabled but the UPI ID is empty"))
	}
	d.UPI.Name = limitLen(&errs, "UPI payee name", d.UPI.Name, 60)
	if d.PayPal.URL != "" {
		if u, err := payments.NormalizePayPal(d.PayPal.URL); err != nil {
			errs = append(errs, err)
		} else {
			d.PayPal.URL = u
		}
	} else if d.PayPal.Enabled {
		errs = append(errs, errors.New("PayPal is enabled but the link is empty"))
	}
	if d.Link.URL != "" {
		if u, err := payments.HTTPSURL(d.Link.URL); err != nil {
			errs = append(errs, fmt.Errorf("donation link: %w", err))
		} else {
			d.Link.URL = u
		}
	} else if d.Link.Enabled {
		errs = append(errs, errors.New("donation link is enabled but empty"))
	}
	d.Link.Label = limitLen(&errs, "donation link label", d.Link.Label, 40)
	if len(d.Wallets) > 10 {
		errs = append(errs, errors.New("at most 10 wallets"))
	}
	seen := map[string]bool{}
	for i := range d.Wallets {
		w := &d.Wallets[i]
		if w.ID == "" {
			w.ID = strings.ToLower(platform.Token(6))
			w.ID = strings.Map(func(r rune) rune {
				if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
					return r
				}
				return 'x'
			}, w.ID)
		}
		if !walletIDs.MatchString(w.ID) || seen[w.ID] {
			errs = append(errs, fmt.Errorf("wallet %d: bad or duplicate id", i+1))
		}
		seen[w.ID] = true
		addr, err := payments.ValidateWallet(w.Kind, w.Address)
		if err != nil {
			errs = append(errs, fmt.Errorf("wallet %q: %w", w.Label, err))
		} else {
			w.Address = addr
		}
		for _, n := range w.Networks {
			nw, ok := payments.NetworkByKey(n)
			if !ok || nw.Kind != w.Kind {
				errs = append(errs, fmt.Errorf("wallet %q: network %q does not match a %s address", w.Label, n, w.Kind))
			}
		}
		if len(w.Networks) == 0 {
			errs = append(errs, fmt.Errorf("wallet %q: choose at least one network", w.Label))
		}
		w.Tokens = limitLen(&errs, "wallet tokens", w.Tokens, 80)
		w.Label = limitLen(&errs, "wallet label", w.Label, 40)
	}

	wa := &a.WhatsApp
	wa.Number = strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, wa.Number)
	if wa.Number != "" && !digitsRe.MatchString(wa.Number) {
		errs = append(errs, errors.New("WhatsApp number must be 8–15 digits including country code (e.g. 919876543210)"))
	}
	if wa.Enabled && wa.Number == "" {
		errs = append(errs, errors.New("WhatsApp is enabled but the number is empty"))
	}
	wa.Message = limitLen(&errs, "WhatsApp message", wa.Message, 500)
	wa.Label = limitLen(&errs, "WhatsApp label", wa.Label, 60)

	l := &a.Limits
	if l.ContactsPerAd < 1 || l.ContactsPerAd > 50 {
		errs = append(errs, errors.New("contacts per ad must be 1–50"))
	}
	if l.MaxAdsPerDay < 1 || l.MaxAdsPerDay > 1000 {
		errs = append(errs, errors.New("max ads per user per day must be 1–1000"))
	}
	if l.MaxAdsPerDayIP < l.MaxAdsPerDay || l.MaxAdsPerDayIP > 10000 {
		errs = append(errs, errors.New("max ads per IP per day must be ≥ the per-user limit and ≤ 10000"))
	}
	if l.PageSize < 10 || l.PageSize > 200 {
		errs = append(errs, errors.New("rows per page must be 10–200"))
	}
	if l.PreviewPages < 1 || l.PreviewPages > 10 {
		errs = append(errs, errors.New("preview pages must be 1–10"))
	}

	s := &a.Site
	s.Announcement = limitLen(&errs, "announcement", s.Announcement, 300)
	s.LegalName = limitLen(&errs, "legal name", s.LegalName, 120)
	s.Address = limitLen(&errs, "postal address", s.Address, 300)
	if s.ContactEmail != "" {
		if e, ok, _ := emailcheck.Syntax(s.ContactEmail); !ok {
			errs = append(errs, errors.New("contact email is not valid"))
		} else {
			s.ContactEmail = e
		}
	}
	if !oneOf(s.ConsentMode, "all", "eu_only", "off") {
		errs = append(errs, errors.New("consent mode must be all, eu_only or off"))
	}
	return errors.Join(errs...)
}

// cleanAdsTxt normalises ads.txt: LF line ends, trimmed lines, size limits and
// no markup (the file is served as text/plain, but nothing odd should get in).
func cleanAdsTxt(errs *[]error, s string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
	if len(s) > 20000 {
		*errs = append(*errs, errors.New("ads.txt is over 20 KB"))
		return s
	}
	var out []string
	for i, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if strings.ContainsAny(line, "<>\x00") {
			*errs = append(*errs, fmt.Errorf("ads.txt line %d contains < > or a control character", i+1))
			continue
		}
		if len(line) > 500 {
			*errs = append(*errs, fmt.Errorf("ads.txt line %d is too long", i+1))
			continue
		}
		out = append(out, line)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// Store keeps the current snapshot in memory; readers never touch the DB.
type Store struct {
	DB    *db.DB
	Clock platform.Clock

	cur     atomic.Pointer[snapshot]
	version atomic.Int64
}

type snapshot struct {
	all    All
	public []byte // pre-rendered public JSON (without per-request fields)
}

var sections = []string{"ads", "affiliates", "donations", "whatsapp", "limits", "site"}

// Load reads stored sections over the defaults.
func (s *Store) Load(ctx context.Context) error {
	a := Defaults()
	rows, err := s.DB.R.QueryContext(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return err
		}
		if err := a.setSection(k, []byte(v)); err != nil {
			return fmt.Errorf("settings %s: %w", k, err)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := a.Validate(); err != nil {
		// stored data was valid when saved; a failure here means a rule tightened.
		// Keep serving with what we have rather than refusing to boot.
		_ = err
	}
	return s.publish(a)
}

func (a *All) setSection(k string, raw []byte) error {
	switch k {
	case "ads":
		return json.Unmarshal(raw, &a.Ads)
	case "affiliates":
		return json.Unmarshal(raw, &a.Affiliates)
	case "donations":
		return json.Unmarshal(raw, &a.Donations)
	case "whatsapp":
		return json.Unmarshal(raw, &a.WhatsApp)
	case "limits":
		return json.Unmarshal(raw, &a.Limits)
	case "site":
		return json.Unmarshal(raw, &a.Site)
	}
	return fmt.Errorf("unknown settings section %q", k)
}

func (a *All) section(k string) any {
	switch k {
	case "ads":
		return a.Ads
	case "affiliates":
		return a.Affiliates
	case "donations":
		return a.Donations
	case "whatsapp":
		return a.WhatsApp
	case "limits":
		return a.Limits
	case "site":
		return a.Site
	}
	return nil
}

func (s *Store) publish(a All) error {
	pub, err := json.Marshal(BuildPublic(a))
	if err != nil {
		return err
	}
	s.cur.Store(&snapshot{all: a, public: pub})
	s.version.Add(1)
	return nil
}

// Get returns the current settings (treat as read-only).
func (s *Store) Get() All {
	if p := s.cur.Load(); p != nil {
		return p.all
	}
	return Defaults()
}

// PublicJSON is the cached projection served by /api/config.
func (s *Store) PublicJSON() []byte {
	if p := s.cur.Load(); p != nil {
		return p.public
	}
	b, _ := json.Marshal(BuildPublic(Defaults()))
	return b
}

func (s *Store) Version() int64 { return s.version.Load() }

// Save validates and persists one section, returning the new settings.
func (s *Store) Save(ctx context.Context, section string, raw []byte, by string) (All, error) {
	if !oneOf(section, sections...) {
		return All{}, fmt.Errorf("unknown settings section %q", section)
	}
	next := s.Get()
	// deep-copy maps/slices that the section decode would otherwise share
	next.Ads.Slots = cloneSlots(next.Ads.Slots)
	next.Donations.Wallets = append([]Wallet(nil), next.Donations.Wallets...)
	next.Affiliates.Items = append([]Affiliate(nil), next.Affiliates.Items...)
	if section == "ads" {
		next.Ads = Ads{}
	}
	if section == "affiliates" {
		next.Affiliates = Affiliates{}
	}
	if section == "donations" {
		next.Donations = Donations{}
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	var err error
	switch section {
	case "ads":
		err = dec.Decode(&next.Ads)
	case "affiliates":
		err = dec.Decode(&next.Affiliates)
	case "donations":
		err = dec.Decode(&next.Donations)
	case "whatsapp":
		next.WhatsApp = WhatsApp{}
		err = dec.Decode(&next.WhatsApp)
	case "limits":
		next.Limits = Limits{}
		err = dec.Decode(&next.Limits)
	case "site":
		next.Site = Site{}
		err = dec.Decode(&next.Site)
	}
	if err != nil {
		return All{}, fmt.Errorf("invalid JSON: %w", err)
	}
	if err := next.Validate(); err != nil {
		return All{}, err
	}
	val, _ := json.Marshal(next.section(section))
	now := s.Clock.Now().Unix()
	err = s.DB.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO settings(key, value, updated, updated_by) VALUES(?,?,?,?)
			ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated=excluded.updated, updated_by=excluded.updated_by`,
			section, string(val), now, by); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO audit(ts, actor, ip, action, detail) VALUES(?,?,?,?,?)`,
			now, by, "", "settings."+section, string(val))
		return err
	})
	if err != nil {
		return All{}, err
	}
	return next, s.publish(next)
}

func cloneSlots(m map[string]Slot) map[string]Slot {
	out := make(map[string]Slot, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
