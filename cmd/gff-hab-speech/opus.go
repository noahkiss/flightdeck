//go:build speech

package main

/*
#cgo pkg-config: opus
#include <opus.h>

// The opus_encoder_ctl requests are variadic macros, which cgo cannot call.
static int fd_opus_set_bitrate(OpusEncoder *e, opus_int32 b) { return opus_encoder_ctl(e, OPUS_SET_BITRATE(b)); }
static int fd_opus_get_lookahead(OpusEncoder *e, opus_int32 *n) { return opus_encoder_ctl(e, OPUS_GET_LOOKAHEAD(n)); }
*/
import "C"

import (
	"fmt"
	"unsafe"

	"github.com/noahkiss/flightdeck/internal/oggopus"
)

// opusRate is the rate libopus decodes to here: the model's own 16 kHz, so no resampling.
const opusRate = 16000

// maxOpusFrame is the most samples one Opus packet can hold at opusRate: 120 ms.
const maxOpusFrame = opusRate * 120 / 1000

// maxClipMS is the longest clip the helper decodes, the API's limit (voice.md).
const maxClipMS = 120_000

// decodeOpus decodes an Ogg Opus file (RFC 7845) to mono floats at opusRate. A stereo stream is
// mixed down by libopus. The pre-skip is dropped and the end is cut at the last granule
// position, as RFC 7845 4.2 and 4.4 say.
func decodeOpus(b []byte) ([]float32, error) {
	s, err := oggopus.Parse(b)
	if err != nil {
		return nil, err
	}
	var cerr C.int
	dec := C.opus_decoder_create(C.opus_int32(opusRate), 1, &cerr)
	if cerr != C.OPUS_OK || dec == nil {
		return nil, fmt.Errorf("opus decoder: %s", C.GoString(C.opus_strerror(cerr)))
	}
	defer C.opus_decoder_destroy(dec)
	// gff-hab checks the length first; the helper still never decodes more than the limit plus
	// the pre-skip, whatever the stream says.
	limit := (maxClipMS*opusRate)/1000 + s.PreSkip*opusRate/oggopus.Rate + maxOpusFrame
	want := min(int(max(s.Granule, s.PacketSamples)*opusRate/oggopus.Rate), limit)
	out := make([]float32, 0, want)
	buf := make([]float32, maxOpusFrame)
	for i, p := range s.Packets {
		if len(p) == 0 {
			continue
		}
		n := C.opus_decode_float(dec, (*C.uchar)(unsafe.Pointer(&p[0])), C.opus_int32(len(p)),
			(*C.float)(unsafe.Pointer(&buf[0])), C.int(maxOpusFrame), 0)
		if n < 0 {
			return nil, fmt.Errorf("opus packet %d: %s", i, C.GoString(C.opus_strerror(n)))
		}
		if len(out)+int(n) > limit {
			return nil, fmt.Errorf("the clip is longer than %d ms", maxClipMS)
		}
		out = append(out, buf[:n]...)
	}
	skip := s.PreSkip * opusRate / oggopus.Rate
	end := int(s.Granule * opusRate / oggopus.Rate)
	if end > len(out) || s.Granule <= 0 {
		end = len(out)
	}
	if skip > end {
		skip = end
	}
	return out[skip:end], nil
}

// encodeOgg encodes mono 16 kHz samples as an Ogg Opus file the way the app records a turn:
// 20 ms packets at bitrate bits per second, one packet per page. The tests use it to make
// Opus clips from hand-made audio; the helper itself only decodes.
func encodeOgg(samples []int16, bitrate int) ([]byte, error) {
	var cerr C.int
	enc := C.opus_encoder_create(C.opus_int32(opusRate), 1, C.OPUS_APPLICATION_VOIP, &cerr)
	if cerr != C.OPUS_OK || enc == nil {
		return nil, fmt.Errorf("opus encoder: %s", C.GoString(C.opus_strerror(cerr)))
	}
	defer C.opus_encoder_destroy(enc)
	C.fd_opus_set_bitrate(enc, C.opus_int32(bitrate))
	var look C.opus_int32
	C.fd_opus_get_lookahead(enc, &look)
	preSkip := int(look) * oggopus.Rate / opusRate
	const frame = opusRate / 50 // 20 ms
	b := oggopus.Page(0x02, 0, 1, 0, oggopus.Head(1, preSkip, opusRate))
	b = append(b, oggopus.Page(0, 0, 1, 1, oggopus.Tags("gff-hab-speech test"))...)
	pkt := make([]byte, 1500)
	seq := uint32(2)
	gp := int64(preSkip)
	total := int64(preSkip) + int64(len(samples))*oggopus.Rate/opusRate
	// Encode past the end by the encoder's lookahead, padded with silence, so the decoder's
	// output covers the pre-skip plus every sample (RFC 7845 4.4).
	end := len(samples) + int(look)
	for off := 0; off < end; off += frame {
		in := make([]int16, frame)
		if off < len(samples) {
			copy(in, samples[off:])
		}
		n := C.opus_encode(enc, (*C.opus_int16)(unsafe.Pointer(&in[0])), C.int(frame), (*C.uchar)(unsafe.Pointer(&pkt[0])), C.opus_int32(len(pkt)))
		if n < 0 {
			return nil, fmt.Errorf("opus encode: %s", C.GoString(C.opus_strerror(n)))
		}
		gp += frame * oggopus.Rate / opusRate
		typ := byte(0)
		if off+frame >= end {
			typ, gp = 0x04, total
		}
		b = append(b, oggopus.Page(typ, gp, 1, seq, pkt[:n])...)
		seq++
	}
	return b, nil
}
