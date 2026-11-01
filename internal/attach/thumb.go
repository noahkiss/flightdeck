package attach

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif" // decoders for Thumbnail
	"image/jpeg"
	_ "image/png"
	"io"
	"sync"
)

// ThumbSide is the longest side of a thumbnail, in pixels.
const ThumbSide = 320

// maxPixels refuses a source image larger than this before it is decoded: a small file can
// claim a huge size. The app never sends more than 2048 px on a side (4.2 M pixels); 24 M
// pixels leaves room for a large file sent from Files and bounds one decode to about 200 MB.
const maxPixels = 24_000_000

// Thumbnail decodes a PNG, JPEG or GIF and returns a JPEG of at most ThumbSide pixels on its
// longest side, with any transparency drawn over white.
func Thumbnail(ctx context.Context, r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		return nil, errors.New("not a PNG, JPEG or GIF image")
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > maxPixels {
		return nil, errors.New("the image is too large to preview")
	}
	src, _, err := image.Decode(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dst := scale(src, ThumbSide)
	var out bytes.Buffer
	if err := jpeg.Encode(&out, dst, &jpeg.Options{Quality: 80}); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// scale shrinks src to fit side by averaging each target pixel's source box, over white. It
// never enlarges.
func scale(src image.Image, side int) *image.RGBA {
	sb := src.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	w, h := sw, sh
	if w > side || h > side {
		if w >= h {
			w, h = side, max(1, sh*side/sw)
		} else {
			w, h = max(1, sw*side/sh), side
		}
	}
	// Flattened over white first, so transparent pixels average to white, not black.
	flat := image.NewRGBA(image.Rect(0, 0, sw, sh))
	draw.Draw(flat, flat.Bounds(), &image.Uniform{color.White}, image.Point{}, draw.Src)
	draw.Draw(flat, flat.Bounds(), src, sb.Min, draw.Over)
	if w == sw && h == sh {
		return flat
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		y0, y1 := y*sh/h, max(y*sh/h+1, (y+1)*sh/h)
		for x := 0; x < w; x++ {
			x0, x1 := x*sw/w, max(x*sw/w+1, (x+1)*sw/w)
			var r, g, b, n uint64
			for yy := y0; yy < y1; yy++ {
				row := flat.Pix[yy*flat.Stride:]
				for xx := x0; xx < x1; xx++ {
					p := row[xx*4:]
					r += uint64(p[0])
					g += uint64(p[1])
					b += uint64(p[2])
					n++
				}
			}
			o := dst.Pix[y*dst.Stride+x*4:]
			o[0], o[1], o[2], o[3] = uint8(r/n), uint8(g/n), uint8(b/n), 255
		}
	}
	return dst
}

// thumbCacheSize is how many thumbnails the hab keeps: every device asks again on each chat load.
const thumbCacheSize = 64

// thumbCache holds recent thumbnails by id, the least recently used out first. A stored file
// never changes, so an entry never goes stale.
type thumbCache struct {
	mu    sync.Mutex
	order []string
	m     map[string][]byte
}

func (c *thumbCache) get(id string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, ok := c.m[id]
	if ok {
		c.touch(id)
	}
	return b, ok
}

func (c *thumbCache) put(id string, b []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string][]byte{}
	}
	c.m[id] = b
	c.touch(id)
	for len(c.order) > thumbCacheSize {
		delete(c.m, c.order[0])
		c.order = c.order[1:]
	}
}

func (c *thumbCache) touch(id string) {
	for i, x := range c.order {
		if x == id {
			c.order = append(c.order[:i], c.order[i+1:]...)
			break
		}
	}
	c.order = append(c.order, id)
}
