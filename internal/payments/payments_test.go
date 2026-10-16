package payments

import (
	"strings"
	"testing"
)

func TestEIP55Vectors(t *testing.T) {
	for _, a := range []string{
		"0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed", "0xfB6916095ca1df60bB79Ce92cE3Ea74c37c5d359",
		"0xdbF03B407c01E7cD3CBea99509d93f8DDDC8C6FB", "0xD1220A0cf47c7B9Be7A2E6BA89F429762e7b9aDb",
	} {
		got, err := EVMChecksum(strings.ToLower(a))
		if err != nil || got != a {
			t.Errorf("checksum(%s) = %s, %v", a, got, err)
		}
		if _, err := EVMChecksum(a); err != nil {
			t.Errorf("valid checksum rejected: %s", a)
		}
	}
	// a single wrong-case letter is a typo signal
	if _, err := EVMChecksum("0x5AAeb6053F3E94C9b9A09f33669435E7Ef1BeAed"); err == nil {
		t.Error("bad checksum accepted")
	}
	// the founder's donation address (given in lower case) normalises and re-validates
	founder := "0x6db11d0a652de0ee57edaddfa70e5dac87d33bfb"
	sum, err := EVMChecksum(founder)
	if err != nil || !strings.EqualFold(sum, founder) {
		t.Fatalf("founder address: %s %v", sum, err)
	}
	if again, err := EVMChecksum(sum); err != nil || again != sum {
		t.Fatal("checksum not a fixed point")
	}
	for _, bad := range []string{"0x123", "6db11d0a652de0ee57edaddfa70e5dac87d33bfb", "0xZZb11d0a652de0ee57edaddfa70e5dac87d33bfb"} {
		if _, err := EVMChecksum(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestUPIAndPayPal(t *testing.T) {
	if v, err := ValidateUPI(" MarkeMind@OKICICI "); err != nil || v != "markemind@okicici" {
		t.Fatalf("upi: %s %v", v, err)
	}
	for _, bad := range []string{"no-at", "a@1bank", "@ybl", "x y@ybl", "a@b@c"} {
		if _, err := ValidateUPI(bad); err == nil {
			t.Errorf("upi accepted %q", bad)
		}
	}
	if u := UPIURL("a@ybl", "Marke Mind", ""); u != "upi://pay?cu=INR&pa=a%40ybl&pn=Marke+Mind" {
		t.Fatalf("upi url %s", u)
	}
	cases := map[string]string{
		"markemind":           "https://paypal.me/markemind",
		"paypal.me/markemind": "https://paypal.me/markemind",
		"https://www.PayPal.com/donate?hosted_button_id=ABC": "https://www.paypal.com/donate?hosted_button_id=ABC",
	}
	for in, want := range cases {
		if got, err := NormalizePayPal(in); err != nil || got != want {
			t.Errorf("paypal(%q) = %q %v", in, got, err)
		}
	}
	for _, bad := range []string{"https://paypal.me.evil.com/x", "http://paypal.me/x", "javascript:alert(1)"} {
		if _, err := NormalizePayPal(bad); err == nil {
			t.Errorf("paypal accepted %q", bad)
		}
	}
}

func TestWalletKinds(t *testing.T) {
	ok := map[string]string{
		"btc":  "bc1qar0srrr7xfkvy5l643lydnw9re59gtzzwf5mdq",
		"sol":  "4Nd1mBQtrMJVYVfKf2PJy9NZUZdTAsp7D4xWLs4gDB4T",
		"tron": "TNPeeaaFB7K9cmo4uQpcU32zGK8G1NYqeL",
	}
	for k, a := range ok {
		if _, err := ValidateWallet(k, a); err != nil {
			t.Errorf("%s rejected: %v", k, err)
		}
	}
	if _, err := ValidateWallet("doge", "x"); err == nil {
		t.Error("unknown kind accepted")
	}
}

func TestQRSVG(t *testing.T) {
	s, err := QRSVG("0x6DB11d0a652De0ee57EDAddFa70e5dAc87d33BFB")
	if err != nil || !strings.HasPrefix(s, "<svg") || !strings.Contains(s, `d="M`) {
		t.Fatalf("qr: %v %s", err, s[:min(80, len(s))])
	}
	if len(s) > 12000 {
		t.Fatalf("qr svg too large: %d bytes", len(s))
	}
}
