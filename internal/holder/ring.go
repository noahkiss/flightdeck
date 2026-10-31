package holder

// ring keeps the most recent bytes a pane produced, up to its size, and the pane's byte count:
// the number of bytes produced since spawn. Offsets are absolute: byte i of the pane's output
// has offset i for its whole life, whether or not it is still kept.
//
// The buffer grows as it fills, so an idle pane holds little memory. Once it reaches its size,
// byte i lives at index i mod size.
type ring struct {
	size  int
	buf   []byte
	count uint64
}

func newRing(size int) *ring {
	if size < 1 {
		size = 1
	}
	return &ring{size: size}
}

// oldest returns the offset of the oldest byte still kept.
func (r *ring) oldest() uint64 {
	if r.count > uint64(r.size) {
		return r.count - uint64(r.size)
	}
	return 0
}

// write appends p. It never blocks and never fails: the oldest bytes give way.
func (r *ring) write(p []byte) {
	if len(p) >= r.size {
		// Only the last size bytes survive. Skip the rest, but count them.
		r.count += uint64(len(p) - r.size)
		p = p[len(p)-r.size:]
		// The ring is full from here on, so every index must exist.
		if len(r.buf) < r.size {
			r.buf = append(r.buf, make([]byte, r.size-len(r.buf))...)
		}
	}
	for len(p) > 0 {
		if len(r.buf) < r.size {
			// Still growing: the buffer holds offsets 0..count-1 in place.
			n := min(len(p), r.size-len(r.buf))
			r.buf = append(r.buf, p[:n]...)
			r.count += uint64(n)
			p = p[n:]
			continue
		}
		i := int(r.count % uint64(r.size))
		n := copy(r.buf[i:], p)
		r.count += uint64(n)
		p = p[n:]
	}
}

// read returns a copy of up to max bytes starting at offset from. from must be within
// [oldest, count]; the caller clamps it.
func (r *ring) read(from uint64, max int) []byte {
	if from < r.oldest() || from >= r.count || max <= 0 {
		return nil
	}
	n := int(min(uint64(max), r.count-from))
	out := make([]byte, 0, n)
	for len(out) < n {
		i := int(from % uint64(r.size))
		end := min(len(r.buf), i+n-len(out))
		out = append(out, r.buf[i:end]...)
		from += uint64(end - i)
	}
	return out
}
