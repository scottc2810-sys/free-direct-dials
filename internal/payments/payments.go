// Package payments validates donation destinations (UPI, PayPal, crypto
// wallets) and renders QR codes as compact SVG.
package payments

import (
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"golang.org/x/crypto/sha3"
	"rsc.io/qr"
)

var (
	upiRe    = regexp.MustCompile(`^[a-zA-Z0-9._-]{2,256}@[a-zA-Z][a-zA-Z0-9]{1,63}$`)
	ppHandle = regexp.MustCompile(`^[A-Za-z0-9]{1,20}$`)
	evmRe    = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)
	btcRe    = regexp.MustCompile(`^(bc1[02-9ac-hj-np-z]{11,71}|[13][a-km-zA-HJ-NP-Z1-9]{25,34})$`)
	solRe    = regexp.MustCompile(`^[1-9A-HJ-NP-Za-km-z]{32,44}$`)
	tronRe   = regexp.MustCompile(`^T[1-9A-HJ-NP-Za-km-z]{33}$`)
)

// ValidateUPI checks a UPI virtual payment address (e.g. name@okicici).
func ValidateUPI(vpa string) (string, error) {
	v := strings.TrimSpace(vpa)
	if !upiRe.MatchString(v) {
		return "", errors.New("UPI ID must look like name@bank (e.g. markemind@okicici)")
	}
	return strings.ToLower(v), nil
}

// UPIURL builds the upi:// intent that UPI apps open.
func UPIURL(vpa, name, note string) string {
	q := url.Values{"pa": {vpa}, "cu": {"INR"}}
	if name != "" {
		q.Set("pn", name)
	}
	if note != "" {
		q.Set("tn", note)
	}
	return "upi://pay?" + q.Encode()
}

// NormalizePayPal accepts a paypal.me handle or a PayPal https URL.
func NormalizePayPal(s string) (string, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "@")
	if ppHandle.MatchString(s) {
		return "https://paypal.me/" + s, nil
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" {
		return "", errors.New("PayPal link must be https://paypal.me/yourname or a paypal.com link")
	}
	h := strings.ToLower(u.Hostname())
	if h != "paypal.me" && h != "www.paypal.me" && h != "paypal.com" && h != "www.paypal.com" {
		return "", errors.New("PayPal link must be on paypal.me or paypal.com")
	}
	u.Host = h
	return u.String(), nil
}

// HTTPSURL validates a generic https link (Buy Me a Coffee, Ko-fi, Smart Link…).
func HTTPSURL(s string) (string, error) {
	s = strings.TrimSpace(s)
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || u.Host == "" || len(s) > 500 {
		return "", errors.New("must be a full https:// link")
	}
	return u.String(), nil
}

// EVMChecksum returns the EIP-55 mixed-case form. A mixed-case input whose
// checksum is wrong is rejected (it signals a typo that would lose funds).
func EVMChecksum(addr string) (string, error) {
	addr = strings.TrimSpace(addr)
	if !evmRe.MatchString(addr) {
		return "", errors.New("EVM address must be 0x followed by 40 hex characters")
	}
	hexPart := addr[2:]
	lower := strings.ToLower(hexPart)
	h := sha3.NewLegacyKeccak256()
	h.Write([]byte(lower))
	digest := hex.EncodeToString(h.Sum(nil))
	var b strings.Builder
	b.WriteString("0x")
	for i, c := range lower {
		if c >= 'a' && c <= 'f' && digest[i] >= '8' {
			b.WriteRune(c - 32)
		} else {
			b.WriteRune(c)
		}
	}
	sum := b.String()
	mixed := hexPart != lower && hexPart != strings.ToUpper(hexPart)
	if mixed && sum != addr {
		return "", errors.New("EVM address checksum mismatch: check for a typo")
	}
	return sum, nil
}

// Network describes where a wallet can receive funds.
type Network struct {
	Key     string `json:"key"`
	Name    string `json:"name"`
	Kind    string `json:"kind"` // evm | btc | sol | tron
	ChainID int    `json:"chain_id,omitempty"`
}

var Networks = []Network{
	{"ethereum", "Ethereum", "evm", 1}, {"bsc", "BNB Smart Chain", "evm", 56}, {"polygon", "Polygon", "evm", 137},
	{"base", "Base", "evm", 8453}, {"arbitrum", "Arbitrum One", "evm", 42161}, {"optimism", "OP Mainnet", "evm", 10},
	{"avalanche", "Avalanche C-Chain", "evm", 43114}, {"bitcoin", "Bitcoin", "btc", 0}, {"solana", "Solana", "sol", 0},
	{"tron", "TRON", "tron", 0},
}

func NetworkByKey(k string) (Network, bool) {
	for _, n := range Networks {
		if n.Key == k {
			return n, true
		}
	}
	return Network{}, false
}

// ValidateWallet normalises an address for its kind.
func ValidateWallet(kind, addr string) (string, error) {
	addr = strings.TrimSpace(addr)
	switch kind {
	case "evm":
		return EVMChecksum(addr)
	case "btc":
		a := addr
		if strings.HasPrefix(strings.ToLower(a), "bc1") {
			a = strings.ToLower(a)
		}
		if !btcRe.MatchString(a) {
			return "", errors.New("not a valid Bitcoin address")
		}
		return a, nil
	case "sol":
		if !solRe.MatchString(addr) {
			return "", errors.New("not a valid Solana address")
		}
		return addr, nil
	case "tron":
		if !tronRe.MatchString(addr) {
			return "", errors.New("not a valid TRON address")
		}
		return addr, nil
	}
	return "", fmt.Errorf("unknown wallet type %q", kind)
}

// QRSVG renders text as a QR code (error correction M) in a single SVG path.
// Colours use currentColor so the code follows the page theme.
func QRSVG(text string) (string, error) {
	c, err := qr.Encode(text, qr.M)
	if err != nil {
		return "", err
	}
	const quiet = 4
	n := c.Size + quiet*2
	var p strings.Builder
	for y := 0; y < c.Size; y++ {
		for x := 0; x < c.Size; x++ {
			if !c.Black(x, y) {
				continue
			}
			run := 1
			for x+run < c.Size && c.Black(x+run, y) {
				run++
			}
			fmt.Fprintf(&p, "M%d %dh%dv1h-%dz", x+quiet, y+quiet, run, run)
			x += run - 1
		}
	}
	return fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" shape-rendering="crispEdges" role="img"><rect width="%d" height="%d" fill="#fff"/><path fill="#111" d="%s"/></svg>`,
		n, n, n, n, p.String()), nil
}
