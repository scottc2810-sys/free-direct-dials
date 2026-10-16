package settings

import (
	"net/url"

	"markemind/dials/internal/payments"
)

// Public is the visitor-facing projection of settings (no internal fields).
type Public struct {
	Ads struct {
		Mode           string                `json:"mode"`
		SmartLink      string                `json:"smartlink"`
		MinSeconds     int                   `json:"min_seconds"`
		Adblock        string                `json:"adblock"`
		ProbeURL       string                `json:"probe_url,omitempty"`
		Slots          map[string]PublicSlot `json:"slots"`
		Placements     map[string]bool       `json:"placements"`
		Label          string                `json:"label"`
		CTA            string                `json:"cta"`
		GPTAdUnit      string                `json:"gpt_ad_unit,omitempty"`
		PreviewGate    bool                  `json:"preview_gate"`
		PreviewSeconds int                   `json:"preview_seconds"`
	} `json:"ads"`
	Affiliates struct {
		Label string            `json:"label"`
		Items []PublicAffiliate `json:"items"`
	} `json:"affiliates"`
	Donations struct {
		Enabled bool           `json:"enabled"`
		Heading string         `json:"heading"`
		Message string         `json:"message"`
		UPI     *PublicUPI     `json:"upi,omitempty"`
		PayPal  string         `json:"paypal,omitempty"`
		Link    *PublicLink    `json:"link,omitempty"`
		Wallets []PublicWallet `json:"wallets"`
	} `json:"donations"`
	WhatsApp *PublicWhatsApp `json:"whatsapp,omitempty"`
	Limits   struct {
		ContactsPerAd int  `json:"contacts_per_ad"`
		MaxAdsPerDay  int  `json:"max_ads_per_day"`
		PageSize      int  `json:"page_size"`
		PreviewPages  int  `json:"preview_pages"`
		Signups       bool `json:"signups"`
	} `json:"limits"`
	Site struct {
		Announcement     string `json:"announcement,omitempty"`
		ReadOnly         bool   `json:"read_only"`
		ConsentMode      string `json:"consent_mode"`
		SubscribeDefault bool   `json:"subscribe_default"`
		ContactEmail     string `json:"contact_email,omitempty"`
		LegalName        string `json:"legal_name,omitempty"`
	} `json:"site"`
}

type PublicSlot struct {
	HTML       string `json:"html"`
	HTMLMobile string `json:"html_mobile,omitempty"`
	Isolation  string `json:"isolation"`
	Height     int    `json:"h"`
	HeightM    int    `json:"hm"`
}

type PublicAffiliate struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Text       string   `json:"text"`
	CTA        string   `json:"cta"`
	URL        string   `json:"url"`
	Placements []string `json:"placements"`
	Weight     int      `json:"weight"`
}

type PublicUPI struct {
	VPA  string `json:"vpa"`
	Name string `json:"name,omitempty"`
	URL  string `json:"url"`
	QR   string `json:"qr"`
}

type PublicLink struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

type PublicWallet struct {
	ID       string             `json:"id"`
	Label    string             `json:"label"`
	Address  string             `json:"address"`
	Networks []payments.Network `json:"networks"`
	Tokens   string             `json:"tokens"`
	QR       string             `json:"qr"`
}

type PublicWhatsApp struct {
	Label   string `json:"label"`
	Number  string `json:"number"`
	Message string `json:"message"`
	URL     string `json:"url"` // base wa.me link; the page appends the filter summary
}

// BuildPublic computes the projection (QR codes are rendered once here).
func BuildPublic(a All) Public {
	var p Public
	p.Ads.Mode, p.Ads.SmartLink, p.Ads.MinSeconds, p.Ads.Adblock, p.Ads.ProbeURL =
		a.Ads.Mode, a.Ads.SmartLink, a.Ads.MinSeconds, a.Ads.Adblock, a.Ads.ProbeURL
	p.Ads.Label, p.Ads.CTA = a.Ads.SponsorLabel, a.Ads.SponsorCTA
	p.Ads.PreviewGate, p.Ads.PreviewSeconds = a.Ads.PreviewGate, a.Ads.PreviewSeconds
	if a.Ads.Mode == "gpt_rewarded" {
		p.Ads.GPTAdUnit = a.Ads.GPTAdUnit
	}
	p.Affiliates.Label = a.Affiliates.Label
	p.Affiliates.Items = []PublicAffiliate{}
	if a.Affiliates.Enabled {
		for _, it := range a.Affiliates.Items {
			if it.Enabled && it.URL != "" && len(it.Placements) > 0 {
				p.Affiliates.Items = append(p.Affiliates.Items, PublicAffiliate{ID: it.ID, Name: it.Name, Text: it.Text, CTA: it.CTA,
					URL: it.URL, Placements: it.Placements, Weight: it.Weight})
			}
		}
	}
	p.Ads.Placements = map[string]bool{}
	if a.Ads.SmartLink != "" {
		for k, v := range a.Ads.Placements {
			if v {
				p.Ads.Placements[k] = true
			}
		}
	}
	p.Ads.Slots = map[string]PublicSlot{}
	for k, s := range a.Ads.Slots {
		if s.Enabled {
			p.Ads.Slots[k] = PublicSlot{HTML: s.HTML, HTMLMobile: s.HTMLMobile, Isolation: s.Isolation, Height: s.Height, HeightM: s.HeightM}
		}
	}
	d := a.Donations
	p.Donations.Enabled, p.Donations.Heading, p.Donations.Message = d.Enabled, d.Heading, d.Message
	p.Donations.Wallets = []PublicWallet{}
	if d.Enabled {
		if d.UPI.Enabled && d.UPI.VPA != "" {
			u := payments.UPIURL(d.UPI.VPA, d.UPI.Name, "Free Direct Dials")
			qr, _ := payments.QRSVG(u)
			p.Donations.UPI = &PublicUPI{VPA: d.UPI.VPA, Name: d.UPI.Name, URL: u, QR: qr}
		}
		if d.PayPal.Enabled {
			p.Donations.PayPal = d.PayPal.URL
		}
		if d.Link.Enabled && d.Link.URL != "" {
			p.Donations.Link = &PublicLink{Label: d.Link.Label, URL: d.Link.URL}
		}
		for _, w := range d.Wallets {
			if !w.Enabled {
				continue
			}
			pw := PublicWallet{ID: w.ID, Label: w.Label, Address: w.Address, Tokens: w.Tokens}
			for _, n := range w.Networks {
				if nw, ok := payments.NetworkByKey(n); ok {
					pw.Networks = append(pw.Networks, nw)
				}
			}
			pw.QR, _ = payments.QRSVG(w.Address)
			p.Donations.Wallets = append(p.Donations.Wallets, pw)
		}
	}
	if a.WhatsApp.Enabled && a.WhatsApp.Number != "" {
		p.WhatsApp = &PublicWhatsApp{Label: a.WhatsApp.Label, Number: a.WhatsApp.Number, Message: a.WhatsApp.Message,
			URL: "https://wa.me/" + url.PathEscape(a.WhatsApp.Number)}
	}
	p.Limits.ContactsPerAd, p.Limits.MaxAdsPerDay, p.Limits.Signups = a.Limits.ContactsPerAd, a.Limits.MaxAdsPerDay, a.Limits.SignupsEnabled
	p.Limits.PageSize, p.Limits.PreviewPages = a.Limits.PageSize, a.Limits.PreviewPages
	p.Site.Announcement, p.Site.ReadOnly, p.Site.ConsentMode = a.Site.Announcement, a.Site.ReadOnly, a.Site.ConsentMode
	p.Site.SubscribeDefault, p.Site.ContactEmail, p.Site.LegalName = a.Site.SubscribeDefault, a.Site.ContactEmail, a.Site.LegalName
	return p
}
