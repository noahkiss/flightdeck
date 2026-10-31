package holder

import (
	"bytes"
	"math/rand/v2"
	"testing"
)

// TestRingModel writes random chunks into rings of several sizes and checks count, oldest and
// every readable range against a plain byte slice.
func TestRingModel(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for _, size := range []int{1, 7, 64, 1000} {
		r := newRing(size)
		var all []byte
		for i := 0; i < 300; i++ {
			chunk := make([]byte, rng.IntN(3*size+2))
			for j := range chunk {
				chunk[j] = byte(rng.IntN(256))
			}
			r.write(chunk)
			all = append(all, chunk...)
			if r.count != uint64(len(all)) {
				t.Fatalf("size %d: count %d, want %d", size, r.count, len(all))
			}
			oldest := max(0, len(all)-size)
			if r.oldest() != uint64(oldest) {
				t.Fatalf("size %d: oldest %d, want %d", size, r.oldest(), oldest)
			}
			if len(r.buf) > size {
				t.Fatalf("size %d: buffer grew to %d", size, len(r.buf))
			}
			from := oldest + rng.IntN(len(all)-oldest+1)
			n := rng.IntN(size + 2)
			got := r.read(uint64(from), n)
			want := all[from:min(len(all), from+n)]
			if !bytes.Equal(got, want) && !(len(got) == 0 && len(want) == 0) {
				t.Fatalf("size %d: read(%d, %d) = %q, want %q", size, from, n, got, want)
			}
		}
	}
}

func TestRingReadOutside(t *testing.T) {
	r := newRing(4)
	r.write([]byte("abcdef"))
	if r.oldest() != 2 || r.count != 6 {
		t.Fatalf("oldest %d count %d", r.oldest(), r.count)
	}
	if got := r.read(1, 10); got != nil {
		t.Fatalf("read before oldest: %q", got)
	}
	if got := r.read(6, 10); got != nil {
		t.Fatalf("read at count: %q", got)
	}
	if got := r.read(2, 10); string(got) != "cdef" {
		t.Fatalf("read(2) = %q", got)
	}
}

// TestRingGrowsAsItFills checks that an idle pane does not hold a full ring.
func TestRingGrowsAsItFills(t *testing.T) {
	r := newRing(1 << 20)
	r.write([]byte("hello"))
	if cap(r.buf) >= 1<<20 {
		t.Fatalf("ring allocated %d bytes for 5", cap(r.buf))
	}
}
