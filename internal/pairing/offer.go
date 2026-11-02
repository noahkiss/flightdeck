// Package pairing is the hab side of QR pairing (docs/specs/transport.md 6.1): the pairing
// offer and its link, the QR code for a terminal, the addresses a pairing window may bind, and
// the window itself, a short-lived SSH listener that serves one subsystem, flightdeck-pair.
package pairing

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"
)

// TokenSize is the token's length in bytes: 128 bits.
const TokenSize = 16

// MaxEndpoints is the most endpoints a code carries (transport.md 6.1.1).
const MaxEndpoints = 8

// Offer is what the QR code carries.
type Offer struct {
	DeviceID  string   // the hab's host-key fingerprint, SHA256:...
	Endpoints []string // host:port, in the order to try
	Token     []byte   // TokenSize bytes
	Label     string   // optional
}

// NewToken returns a fresh random token.
func NewToken() ([]byte, error) {
	t := make([]byte, TokenSize)
	if _, err := rand.Read(t); err != nil {
		return nil, err
	}
	return t, nil
}

// EncodeToken is the token as the code carries it: unpadded base64url.
func EncodeToken(t []byte) string { return base64.RawURLEncoding.EncodeToString(t) }

// DecodeToken reads a token from the code's form; it must decode to TokenSize bytes.
func DecodeToken(s string) ([]byte, error) {
	if len(s) != base64.RawURLEncoding.EncodedLen(TokenSize) {
		return nil, errors.New("token: wrong length")
	}
	b, err := base64.RawURLEncoding.Strict().DecodeString(s)
	if err != nil || len(b) != TokenSize {
		return nil, errors.New("token: not base64url")
	}
	return b, nil
}

// Link is the offer as a flightdeck://pair link, the format the app's PairingCode reads.
func (o Offer) Link() (string, error) {
	if !strings.HasPrefix(o.DeviceID, "SHA256:") {
		return "", fmt.Errorf("device id %q is not a SHA256 fingerprint", o.DeviceID)
	}
	if len(o.Endpoints) == 0 || len(o.Endpoints) > MaxEndpoints {
		return "", fmt.Errorf("%d endpoints; a code carries 1 to %d", len(o.Endpoints), MaxEndpoints)
	}
	if len(o.Token) != TokenSize {
		return "", errors.New("the token is not 128 bits")
	}
	var b strings.Builder
	b.WriteString("flightdeck://pair?v=1&id=")
	b.WriteString(url.QueryEscape(o.DeviceID))
	for _, ep := range o.Endpoints {
		b.WriteString("&ep=")
		b.WriteString(url.QueryEscape(ep))
	}
	b.WriteString("&t=")
	b.WriteString(EncodeToken(o.Token))
	if o.Label != "" {
		b.WriteString("&l=")
		b.WriteString(url.QueryEscape(o.Label))
	}
	return b.String(), nil
}

var (
	cgnat = netip.MustParsePrefix("100.64.0.0/10")
	ula   = netip.MustParsePrefix("fc00::/7")
)

// Qualifies says whether a pairing window binds addr on its own: a private IPv4 address, the
// CGNAT range a tailnet uses, or an IPv6 unique local address. Never loopback, link-local,
// multicast, unspecified or public.
func Qualifies(addr netip.Addr) bool {
	addr = addr.Unmap()
	switch {
	case !addr.IsValid(), addr.IsUnspecified(), addr.IsLoopback(), addr.IsMulticast(),
		addr.IsLinkLocalUnicast(), addr.IsLinkLocalMulticast(), addr.Zone() != "":
		return false
	case addr.Is4():
		return addr.IsPrivate() || cgnat.Contains(addr)
	default:
		return ula.Contains(addr)
	}
}

// CheckListen says whether the owner may name addr with -listen: what Qualifies allows, and
// loopback. An unspecified or public address is refused, even here.
func CheckListen(addr netip.Addr) error {
	addr = addr.Unmap()
	switch {
	case !addr.IsValid():
		return errors.New("not an address")
	case addr.IsUnspecified():
		return fmt.Errorf("%s listens on every interface; name one LAN or tailnet address", addr)
	case addr.IsLoopback(), Qualifies(addr):
		return nil
	}
	return fmt.Errorf("%s is not a LAN, tailnet or loopback address", addr)
}

// IfAddr is one address of an up interface, with its prefix length.
type IfAddr struct {
	Name   string
	Prefix netip.Prefix
}

// bridgeNames start the names of container and VM bridges, which other machines' containers and
// VMs reach: a window never binds them on its own.
var bridgeNames = []string{"docker", "br-", "veth", "virbr", "vmnet"}

// tailnetLike says whether a CGNAT address looks like a tailnet's: a /32, or an interface named
// tailscale* or utun*. Other CGNAT addresses may be an ISP's shared space that other
// subscribers reach.
func tailnetLike(ia IfAddr) bool {
	return ia.Prefix.Bits() == 32 || strings.HasPrefix(ia.Name, "tailscale") || strings.HasPrefix(ia.Name, "utun")
}

// PickLocal keeps the interface addresses a window binds on its own: what Qualifies allows, minus
// container and VM bridges, and minus CGNAT addresses that do not look like a tailnet's. IPv4
// first, at most MaxEndpoints.
func PickLocal(ifs []IfAddr) []netip.Addr {
	var keep []netip.Addr
	for _, ia := range ifs {
		bridge := false
		for _, p := range bridgeNames {
			if strings.HasPrefix(ia.Name, p) {
				bridge = true
			}
		}
		a := ia.Prefix.Addr().Unmap()
		if bridge || (a.Is4() && cgnat.Contains(a) && !tailnetLike(ia)) {
			continue
		}
		keep = append(keep, a)
	}
	return Pick(keep)
}

// LocalAddrs lists the addresses of this host's up interfaces that PickLocal keeps.
func LocalAddrs() ([]netip.Addr, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var all []IfAddr
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			n, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip, ok := netip.AddrFromSlice(n.IP)
			if !ok {
				continue
			}
			ip = ip.Unmap()
			ones, _ := n.Mask.Size()
			if ip.Is4() && ones > 32 {
				ones -= 96 // a 16-byte mask on an IPv4 address
			}
			all = append(all, IfAddr{Name: ifc.Name, Prefix: netip.PrefixFrom(ip, ones)})
		}
	}
	return PickLocal(all), nil
}

// Pick keeps the addresses that qualify, without repeats, IPv4 first, at most MaxEndpoints.
func Pick(addrs []netip.Addr) []netip.Addr {
	seen := map[netip.Addr]bool{}
	var v4, v6 []netip.Addr
	for _, a := range addrs {
		a = a.Unmap()
		if !Qualifies(a) || seen[a] {
			continue
		}
		seen[a] = true
		if a.Is4() {
			v4 = append(v4, a)
		} else {
			v6 = append(v6, a)
		}
	}
	out := append(v4, v6...)
	if len(out) > MaxEndpoints {
		out = out[:MaxEndpoints]
	}
	return out
}
