package oggopus

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

// clip builds a hand-made Ogg Opus file: headers, then pages of fake 20 ms packets (960
// samples at 48 kHz each). The packets are not real Opus; Parse does not decode them.
func clip(pages int, perPage int) []byte {
	var b []byte
	b = append(b, Page(0x02, 0, 9, 0, Head(1, 312, 16000))...)
	b = append(b, Page(0, 0, 9, 1, Tags("hand-made"))...)
	gp := int64(312)
	for i := 0; i < pages; i++ {
		var ps [][]byte
		for j := 0; j < perPage; j++ {
			ps = append(ps, []byte{0x08, byte(i), byte(j)})
		}
		gp += int64(960 * perPage)
		typ := byte(0)
		if i == pages-1 {
			typ = 0x04
		}
		b = append(b, Page(typ, gp, 9, uint32(2+i), ps...)...)
	}
	return b
}

func TestParse(t *testing.T) {
	s, err := Parse(clip(5, 10)) // 50 packets of 20 ms: 1 s
	if err != nil {
		t.Fatal(err)
	}
	if s.Channels != 1 || s.PreSkip != 312 || s.InputRate != 16000 || len(s.Packets) != 50 {
		t.Fatalf("got %+v", s)
	}
	if s.DurationMS() != 1000 {
		t.Fatalf("duration %d ms, want 1000", s.DurationMS())
	}
	if !bytes.Equal(s.Packets[49], []byte{0x08, 4, 9}) {
		t.Fatalf("last packet %v", s.Packets[49])
	}
}

// TestLongPacket: a packet over 255 bytes takes several lacing values; one of exactly 510
// bytes ends with a zero lacing value.
func TestLongPacket(t *testing.T) {
	long := bytes.Repeat([]byte{7}, 600)
	even := bytes.Repeat([]byte{8}, 510)
	var b []byte
	b = append(b, Page(0x02, 0, 1, 0, Head(2, 0, 48000))...)
	b = append(b, Page(0, 0, 1, 1, Tags(""))...)
	b = append(b, Page(0x04, 1920, 1, 2, long, even)...)
	s, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Packets) != 2 || !bytes.Equal(s.Packets[0], long) || !bytes.Equal(s.Packets[1], even) {
		t.Fatalf("packets %d", len(s.Packets))
	}
}

// rawPage builds a page with the lacing values given, for packets that span pages.
func rawPage(typ byte, granule int64, seq uint32, lacing []byte, data []byte) []byte {
	b := make([]byte, 27)
	copy(b, "OggS")
	b[5] = typ
	binary.LittleEndian.PutUint64(b[6:], uint64(granule))
	binary.LittleEndian.PutUint32(b[14:], 1)
	binary.LittleEndian.PutUint32(b[18:], seq)
	b[26] = byte(len(lacing))
	b = append(b, lacing...)
	b = append(b, data...)
	binary.LittleEndian.PutUint32(b[22:], checksum(b[:22], b[26:]))
	return b
}

func TestPacketAcrossPages(t *testing.T) {
	var b []byte
	b = append(b, Page(0x02, 0, 1, 0, Head(1, 0, 16000))...)
	b = append(b, Page(0, 0, 1, 1, Tags(""))...)
	b = append(b, rawPage(0, -1, 2, []byte{255}, bytes.Repeat([]byte{1}, 255))...)
	b = append(b, rawPage(0x01|0x04, 960, 3, []byte{10}, bytes.Repeat([]byte{2}, 10))...)
	s, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Packets) != 1 || len(s.Packets[0]) != 265 || s.Granule != 960 {
		t.Fatalf("packets %d, granule %d", len(s.Packets), s.Granule)
	}
}

func TestParseRefuses(t *testing.T) {
	good := clip(2, 3)
	flip := append([]byte(nil), good...)
	flip[len(flip)-1] ^= 0xff
	missing := append(append([]byte(nil), good[:len(good)-len(Page(0x04, 0, 9, 3, []byte{1, 2, 3}, []byte{1, 2, 3}, []byte{1, 2, 3}))]...), Page(0x04, 0, 9, 5, []byte{1})...)
	var cont []byte
	cont = append(cont, Page(0x02, 0, 1, 0, Head(1, 0, 16000))...)
	cont = append(cont, Page(0, 0, 1, 1, Tags(""))...)
	cont = append(cont, rawPage(0x01, 0, 2, []byte{3}, []byte{1, 2, 3})...)
	var cut []byte
	cut = append(cut, Page(0x02, 0, 1, 0, Head(1, 0, 16000))...)
	cut = append(cut, Page(0, 0, 1, 1, Tags(""))...)
	cut = append(cut, rawPage(0, 0, 2, []byte{255}, bytes.Repeat([]byte{1}, 255))...)
	surround := append(Page(0x02, 0, 1, 0, Head(6, 0, 48000)), Page(0, 0, 1, 1, Tags(""))...)
	for _, tc := range []struct {
		name string
		b    []byte
		want string
	}{
		{"pcm", []byte{0, 0, 1, 0, 2, 0}, "not an Ogg"},
		{"short", good[:30], "cut short"},
		{"checksum", flip, "checksum"},
		{"missing page", missing, "missing"},
		{"continues nothing", cont, "never began"},
		{"cut packet", cut, "cut short"},
		{"no begin", Page(0, 0, 1, 0, Head(1, 0, 16000)), "does not begin"},
		{"no headers", Page(0x02, 0, 1, 0, []byte("x")), "no Opus headers"},
		{"not head", append(Page(0x02, 0, 1, 0, []byte("OpusTagsxxxxxxxxxxx")), Page(0, 0, 1, 1, Tags(""))...), "not OpusHead"},
		{"surround", surround, "mono and stereo"},
	} {
		_, err := Parse(tc.b)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", tc.name, err, tc.want)
		}
	}
}

// TestChecksumKnown pins the CRC to a published value. The Ogg CRC is CRC-32/CKSUM (poly
// 0x04c11db7, init 0) without its final inversion; CKSUM's check value for "123456789" is
// 0x765E7680, so Ogg's is its complement, 0x89A1897F.
func TestChecksumKnown(t *testing.T) {
	var c uint32
	for _, x := range []byte("123456789") {
		c = c<<8 ^ crcTable[byte(c>>24)^x]
	}
	if c != 0x89A1897F {
		t.Fatalf("crc %#x", c)
	}
}

// lyingClip has n packets of 120 ms (code 3, six 20 ms CELT frames) on pages whose granule is
// granule each.
func lyingClip(n int, granule int64) []byte {
	b := Page(0x02, 0, 1, 0, Head(1, 0, 16000))
	b = append(b, Page(0, 0, 1, 1, Tags(""))...)
	pkt := []byte{31<<3 | 3, 6} // config 31 (CELT 20 ms), code 3, 6 frames
	for i := 0; i < n; i += 50 {
		var ps [][]byte
		for j := i; j < min(i+50, n); j++ {
			ps = append(ps, pkt)
		}
		typ := byte(0)
		if i+50 >= n {
			typ = 0x04
		}
		b = append(b, Page(typ, granule, 1, uint32(2+i/50), ps...)...)
	}
	return b
}

// TestPacketLengths: a stream cannot claim less audio than its packets hold. 1009 packets of
// 120 ms with granule 0 still read as 121 s.
func TestPacketLengths(t *testing.T) {
	s, err := Parse(lyingClip(1009, 0))
	if err != nil {
		t.Fatal(err)
	}
	if s.DurationMS() != 121080 {
		t.Fatalf("duration %d ms, want 121080", s.DurationMS())
	}
	for _, tc := range []struct {
		p    []byte
		want int64
	}{
		{[]byte{0x08}, 960},           // SILK 20 ms, one frame
		{[]byte{0x01, 0}, 960},        // SILK 10 ms, two frames
		{[]byte{12<<3 | 2, 0}, 960},   // hybrid 10 ms, two frames
		{[]byte{16 << 3}, 120},        // CELT 2.5 ms
		{[]byte{3<<3 | 3, 2}, 5760},   // SILK 60 ms, two frames: 120 ms
		{[]byte{16<<3 | 3, 48}, 5760}, // 48 frames of 2.5 ms
	} {
		if n, err := packetSamples(tc.p); err != nil || n != tc.want {
			t.Errorf("%x: %d %v, want %d", tc.p, n, err, tc.want)
		}
	}
}

func TestPacketRefuses(t *testing.T) {
	headers := func() []byte {
		return append(Page(0x02, 0, 1, 0, Head(1, 0, 16000)), Page(0, 0, 1, 1, Tags(""))...)
	}
	for _, tc := range []struct {
		name    string
		granule int64
		pkt     []byte
		want    string
	}{
		{"no granule", -1, []byte{0x08}, "no granule position"},
		{"too long", 960, []byte{3<<3 | 3, 3}, "longer than 120 ms"},
		{"empty packet", 960, []byte{}, "empty Opus packet"},
		{"no count", 960, []byte{0x0b}, "no frame count"},
		{"granule past packets", 1 << 62, []byte{0x08}, "runs past the packets"},
	} {
		b := append(headers(), Page(0x04, tc.granule, 1, 2, tc.pkt)...)
		if _, err := Parse(b); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", tc.name, err, tc.want)
		}
	}
}

// FuzzParse feeds Parse arbitrary bytes, seeded with the hand-made streams: it must never panic.
func FuzzParse(f *testing.F) {
	f.Add(clip(3, 4))
	f.Add(lyingClip(60, 0))
	f.Add([]byte("OggS"))
	f.Fuzz(func(t *testing.T, b []byte) {
		if s, err := Parse(b); err == nil {
			_ = s.DurationMS()
		}
	})
}
