package holderproto

import (
	"errors"
	"fmt"
)

// Range is an inclusive range of protocol versions.
type Range struct{ Min, Max Version }

func (r Range) String() string { return fmt.Sprintf("%d..%d", r.Min, r.Max) }

// Negotiate returns the highest version both ranges contain. ok is false when they share none.
func Negotiate(client, holder Range) (v Version, ok bool) {
	hi := min(client.Max, holder.Max)
	lo := max(client.Min, holder.Min)
	if client.Min > client.Max || holder.Min > holder.Max || hi < lo {
		return 0, false
	}
	return hi, true
}

// RefusedError means the two sides share no protocol version.
type RefusedError struct {
	Offered Range // what gff-hab offered
	Holder  Range // what the holder speaks
	Reason  string
}

func (e *RefusedError) Error() string {
	return fmt.Sprintf("holderproto: no common version: offered %s, holder speaks %s: %s",
		e.Offered, e.Holder, e.Reason)
}

// ErrHandshake means the peer sent something other than the expected handshake frame.
var ErrHandshake = errors.New("holderproto: unexpected frame during handshake")

// Hello runs the gff-hab side of the handshake. It offers the registry's range and build, and
// returns the holder's welcome. On success the connection uses the welcome's version. When the
// holder refuses, Hello returns a *RefusedError.
func (c *Conn) Hello(build string) (*Welcome, error) {
	offered := c.reg.Range()
	if err := c.WriteFrame(&Hello{Min: offered.Min, Max: offered.Max, Build: build}); err != nil {
		return nil, err
	}
	f, err := c.ReadFrame()
	if err != nil {
		return nil, err
	}
	switch f := f.(type) {
	case *Welcome:
		if f.Version < offered.Min || f.Version > offered.Max {
			return nil, fmt.Errorf("%w: holder chose version %d outside %s", ErrHandshake, f.Version, offered)
		}
		c.setVersion(f.Version)
		return f, nil
	case *Refuse:
		return nil, &RefusedError{Offered: offered, Holder: Range{f.Min, f.Max}, Reason: f.Reason}
	default:
		return nil, fmt.Errorf("%w: kind %#04x", ErrHandshake, uint16(f.Kind()))
	}
}

// AcceptHello runs the holder side of the handshake up to the welcome. It reads the hello and
// picks the highest common version. With no common version it sends a refuse frame and returns
// a *RefusedError. Otherwise it sets the connection's version and returns the hello; the holder
// then sends its Welcome with Version set to c.Version().
func (c *Conn) AcceptHello() (*Hello, error) {
	f, err := c.ReadFrame()
	if err != nil {
		return nil, err
	}
	h, ok := f.(*Hello)
	if !ok {
		return nil, fmt.Errorf("%w: kind %#04x", ErrHandshake, uint16(f.Kind()))
	}
	offered, own := Range{h.Min, h.Max}, c.reg.Range()
	v, ok := Negotiate(offered, own)
	if !ok {
		reason := "no common protocol version"
		_ = c.WriteFrame(&Refuse{Min: own.Min, Max: own.Max, Reason: reason})
		return h, &RefusedError{Offered: offered, Holder: own, Reason: reason}
	}
	c.setVersion(v)
	return h, nil
}
