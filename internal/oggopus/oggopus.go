// Package oggopus reads an Ogg Opus file (RFC 7845) into its Opus packets. It checks the page
// structure and CRCs and reads the identification header; it does not decode audio. gff-hab
// uses it to bound a clip's length before the speech helper sees it, and the helper uses it to
// hand packets to libopus. Pure Go.
package oggopus

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
)

// Rate is the granule rate of every Ogg Opus stream: 48 kHz, whatever the input rate was.
const Rate = 48000

// Stream is one Ogg Opus logical stream.
type Stream struct {
	Channels  int      // from the identification header
	PreSkip   int      // samples at 48 kHz to drop from the start of the decoded output
	InputRate int      // the encoder's input rate; informational only
	Packets   [][]byte // the audio packets, in order; they alias the input
	Granule   int64    // granule position of the last page: decoded samples at 48 kHz, pre-skip included
	// PacketSamples is the sum of the packets' own lengths at 48 kHz, read from each packet's
	// TOC byte (RFC 6716 3.1). It bounds what a decoder outputs, whatever the granule says.
	PacketSamples int64
}

// Samples returns the stream's playable length in samples at 48 kHz, from the granule position;
// with no granule (0), from the packets' own lengths.
func (s *Stream) Samples() int64 {
	g := s.Granule
	if g <= 0 {
		g = s.PacketSamples
	}
	return max(g-int64(s.PreSkip), 0)
}

// MaxDurationMS returns the most audio the packets can decode to, in milliseconds, whatever the
// granule says. A stream that ends cleanly holds at most one packet more than its playable
// length (the encoder's lookahead); a larger gap means the granule understates the clip.
func (s *Stream) MaxDurationMS() int {
	return int(max(s.PacketSamples-int64(s.PreSkip), 0) * 1000 / Rate)
}

// frameSamples is the frame length at 48 kHz of each TOC config (RFC 6716 3.1, Table 2): SILK
// 10, 20, 40, 60 ms; hybrid 10, 20 ms; CELT 2.5, 5, 10, 20 ms.
func frameSamples(config byte) int64 {
	switch {
	case config < 12:
		return []int64{480, 960, 1920, 2880}[config%4]
	case config < 16:
		return []int64{480, 960}[config%2]
	default:
		return []int64{120, 240, 480, 960}[config%4]
	}
}

// packetSamples returns one Opus packet's length at 48 kHz from its TOC byte and frame count.
func packetSamples(p []byte) (int64, error) {
	if len(p) == 0 {
		return 0, errors.New("an empty Opus packet")
	}
	frames := int64(1)
	switch p[0] & 0x03 {
	case 1, 2:
		frames = 2
	case 3:
		if len(p) < 2 || p[1]&0x3f == 0 {
			return 0, errors.New("an Opus packet with no frame count")
		}
		frames = int64(p[1] & 0x3f)
	}
	n := frames * frameSamples(p[0]>>3)
	if n > 5760 { // 120 ms, the most one packet may hold
		return 0, errors.New("an Opus packet longer than 120 ms")
	}
	return n, nil
}

// DurationMS returns the stream's playable length in milliseconds.
func (s *Stream) DurationMS() int {
	return int(s.Samples() * 1000 / Rate)
}

var (
	errNotOgg = errors.New("not an Ogg file")
	errCut    = errors.New("the Ogg file is cut short")
)

// Parse reads b as an Ogg Opus file with one logical stream. Pages of other streams are skipped.
func Parse(b []byte) (*Stream, error) {
	if len(b) < 4 || !bytes.Equal(b[:4], []byte("OggS")) {
		return nil, errNotOgg
	}
	var (
		s       Stream
		serial  uint32
		started bool
		seq     uint32
		packet  []byte // the packet being read
		open    bool   // packet continues on the next page
		packets [][]byte
		lastGP  int64
		finalGP int64 // granule of the stream's last page, as written
		eos     bool
	)
	for off := 0; off < len(b); {
		if eos {
			break // data after the end of the stream is ignored
		}
		p, n, err := page(b[off:])
		if err != nil {
			return nil, fmt.Errorf("page at byte %d: %w", off, err)
		}
		off += n
		if !started {
			if p.typ&0x02 == 0 {
				return nil, errors.New("the first page does not begin a stream")
			}
			serial, seq, started = p.serial, p.seq, true
		} else {
			if p.serial != serial {
				continue
			}
			if p.seq != seq+1 {
				return nil, fmt.Errorf("page %d follows page %d: a page is missing", p.seq, seq)
			}
			seq = p.seq
		}
		if cont := p.typ&0x01 != 0; cont != open {
			if cont {
				return nil, errors.New("a page continues a packet that never began")
			}
			return nil, errors.New("a packet is cut off by the next page")
		}
		data := p.data
		for _, l := range p.lacing {
			packet = append(packet, data[:l]...)
			data = data[l:]
			open = l == 255
			if !open {
				packets = append(packets, packet)
				packet = nil
			}
		}
		if p.granule != -1 {
			lastGP = p.granule
		}
		finalGP = p.granule
		eos = p.typ&0x04 != 0
	}
	if open {
		return nil, errCut
	}
	if len(packets) < 2 {
		return nil, errors.New("no Opus headers")
	}
	if err := s.head(packets[0]); err != nil {
		return nil, err
	}
	if len(packets[1]) < 8 || !bytes.Equal(packets[1][:8], []byte("OpusTags")) {
		return nil, errors.New("the second packet is not OpusTags")
	}
	s.Packets = packets[2:]
	s.Granule = lastGP
	// The last page ends a packet, so it must carry a granule position (RFC 7845 4).
	if len(s.Packets) > 0 && finalGP < 0 {
		return nil, errors.New("the last page has no granule position")
	}
	for i, p := range s.Packets {
		n, err := packetSamples(p)
		if err != nil {
			return nil, fmt.Errorf("audio packet %d: %w", i, err)
		}
		s.PacketSamples += n
	}
	// A granule past what the packets hold, plus one packet of slack, is a lie, and a huge one
	// would overflow the length arithmetic.
	if len(s.Packets) > 0 && s.Granule > s.PacketSamples+5760 {
		return nil, errors.New("the granule position runs past the packets")
	}
	return &s, nil
}

// head reads the identification header (RFC 7845 5.1).
func (s *Stream) head(p []byte) error {
	if len(p) < 19 || !bytes.Equal(p[:8], []byte("OpusHead")) {
		return errors.New("the first packet is not OpusHead")
	}
	if p[8]>>4 != 0 {
		return fmt.Errorf("Opus header version %d is not supported", p[8])
	}
	s.Channels = int(p[9])
	s.PreSkip = int(binary.LittleEndian.Uint16(p[10:]))
	s.InputRate = int(binary.LittleEndian.Uint32(p[12:]))
	family := p[18]
	if s.Channels < 1 || s.Channels > 2 || family != 0 {
		return fmt.Errorf("only mono and stereo Opus are supported (channels %d, mapping family %d)", s.Channels, family)
	}
	return nil
}

type oggPage struct {
	typ     byte
	granule int64
	serial  uint32
	seq     uint32
	lacing  []byte
	data    []byte
}

// page reads one page from the start of b and returns it and its length.
func page(b []byte) (oggPage, int, error) {
	var p oggPage
	if len(b) < 27 {
		return p, 0, errCut
	}
	if !bytes.Equal(b[:4], []byte("OggS")) {
		return p, 0, errNotOgg
	}
	if b[4] != 0 {
		return p, 0, fmt.Errorf("Ogg version %d", b[4])
	}
	p.typ = b[5]
	p.granule = int64(binary.LittleEndian.Uint64(b[6:]))
	p.serial = binary.LittleEndian.Uint32(b[14:])
	p.seq = binary.LittleEndian.Uint32(b[18:])
	want := binary.LittleEndian.Uint32(b[22:])
	nseg := int(b[26])
	if len(b) < 27+nseg {
		return p, 0, errCut
	}
	p.lacing = b[27 : 27+nseg]
	size := 0
	for _, l := range p.lacing {
		size += int(l)
	}
	end := 27 + nseg + size
	if len(b) < end {
		return p, 0, errCut
	}
	p.data = b[27+nseg : end]
	if got := checksum(b[:22], b[26:end]); got != want {
		return p, 0, errors.New("bad page checksum")
	}
	return p, end, nil
}

// crcTable is the Ogg CRC-32: polynomial 0x04c11db7, no reflection, initial value 0.
var crcTable = func() [256]uint32 {
	var t [256]uint32
	for i := range t {
		r := uint32(i) << 24
		for j := 0; j < 8; j++ {
			if r&0x80000000 != 0 {
				r = r<<1 ^ 0x04c11db7
			} else {
				r <<= 1
			}
		}
		t[i] = r
	}
	return t
}()

// checksum computes a page's CRC with its checksum field taken as zero: head is the page up to
// the field, tail the page after it.
func checksum(head, tail []byte) uint32 {
	var c uint32
	for _, x := range head {
		c = c<<8 ^ crcTable[byte(c>>24)^x]
	}
	for i := 0; i < 4; i++ {
		c = c<<8 ^ crcTable[byte(c>>24)]
	}
	for _, x := range tail {
		c = c<<8 ^ crcTable[byte(c>>24)^x]
	}
	return c
}

// Page builds one Ogg page holding packets, for tests and tools. typ is the header type flags
// (0x02 begins the stream, 0x04 ends it). Every packet must fit on the page.
func Page(typ byte, granule int64, serial, seq uint32, packets ...[]byte) []byte {
	var lacing, data []byte
	for _, p := range packets {
		n := len(p)
		for n >= 255 {
			lacing = append(lacing, 255)
			n -= 255
		}
		lacing = append(lacing, byte(n))
		data = append(data, p...)
	}
	b := make([]byte, 27, 27+len(lacing)+len(data))
	copy(b, "OggS")
	b[5] = typ
	binary.LittleEndian.PutUint64(b[6:], uint64(granule))
	binary.LittleEndian.PutUint32(b[14:], serial)
	binary.LittleEndian.PutUint32(b[18:], seq)
	b[26] = byte(len(lacing))
	b = append(b, lacing...)
	b = append(b, data...)
	binary.LittleEndian.PutUint32(b[22:], checksum(b[:22], b[26:]))
	return b
}

// Head builds an OpusHead packet, for tests and tools.
func Head(channels, preSkip, inputRate int) []byte {
	p := make([]byte, 19)
	copy(p, "OpusHead")
	p[8] = 1
	p[9] = byte(channels)
	binary.LittleEndian.PutUint16(p[10:], uint16(preSkip))
	binary.LittleEndian.PutUint32(p[12:], uint32(inputRate))
	return p
}

// Tags builds an empty OpusTags packet, for tests and tools.
func Tags(vendor string) []byte {
	p := []byte("OpusTags")
	p = binary.LittleEndian.AppendUint32(p, uint32(len(vendor)))
	p = append(p, vendor...)
	return binary.LittleEndian.AppendUint32(p, 0)
}
