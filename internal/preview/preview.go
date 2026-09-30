// Package preview makes the cheap copy of the captures that the MJPEG endpoint
// is served from.
//
// The endpoint exists to put a picture on screen during the second or two before
// WebRTC takes over, and a captured 1080p JPEG is around 180 kB, which at any
// rate worth watching is tens of megabits a second of a viewer's link, spent on
// a picture that is about to be thrown away. A scaled and recompressed frame is
// a fraction of that, and is all a placeholder has to be, so the reduction is
// done once and shared by every viewer rather than once per viewer.
package preview

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"log"
	"time"

	"tailshare/internal/stream"
)

// Config is the picture the fallback endpoint is served.
type Config struct {
	// Source is the captured frames. They arrive at the capture rate whatever
	// this does with them, so the source is not slowed down to suit.
	Source *stream.Hub

	// Width is the widest preview to publish, 0 for the captured width. It is a
	// ceiling and never an upscale: a preview of a small screen is that screen.
	Width int

	// Quality is the JPEG quality to publish at, 1-100. It is well below what
	// the capture is taken at, because the size of the frame is what decides
	// what this costs on the wire.
	Quality int

	// FPS is the ceiling on the preview rate. Captures that arrive faster than
	// this are dropped rather than queued, so the rate a viewer sees does not
	// depend on how fast the machine happens to be capturing.
	FPS int
}

// Preview reduces the captures for the MJPEG endpoint. The zero value is not
// usable; call New.
type Preview struct {
	cfg      Config
	out      *stream.Hub
	interval time.Duration

	// now is the clock, replaceable in tests.
	now func() time.Time

	// next is when the next frame may be published, zero until one is.
	next time.Time

	// lastErr is the last failure reported, so a decode that keeps failing on
	// every frame is reported once rather than ten times a second.
	lastErr string
}

// New returns a preview of cfg's source.
func New(cfg Config) (*Preview, error) {
	if cfg.Source == nil {
		return nil, errors.New("preview: source hub is required")
	}
	if cfg.Width < 0 {
		return nil, fmt.Errorf("preview: width %d out of range", cfg.Width)
	}
	if cfg.Quality == 0 {
		cfg.Quality = 45
	}
	if cfg.Quality < 1 || cfg.Quality > 100 {
		return nil, fmt.Errorf("preview: quality %d out of range 1-100", cfg.Quality)
	}
	if cfg.FPS == 0 {
		cfg.FPS = 10
	}
	if cfg.FPS < 1 || cfg.FPS > 60 {
		return nil, fmt.Errorf("preview: fps %d out of range 1-60", cfg.FPS)
	}
	return &Preview{
		cfg:      cfg,
		out:      stream.NewHub(),
		interval: time.Second / time.Duration(cfg.FPS),
		now:      time.Now,
	}, nil
}

// due reports whether a frame arriving now may be published, and claims the slot
// if so.
//
// The rate is a ceiling on what goes on the wire rather than a queue of work: a
// capture that turns up early is dropped, because whatever is behind it is a
// fresher picture of the same screen and costs the same to send.
func (p *Preview) due(now time.Time) bool {
	if now.Before(p.next) {
		return false
	}
	p.next = now.Add(p.interval)
	return true
}

// Hub is where the previewed frames are published, and what the MJPEG endpoint
// is served from.
func (p *Preview) Hub() *stream.Hub { return p.out }

// String describes the pictures on offer, for the page banner.
func (p *Preview) String() string {
	width := "the captured size"
	if p.cfg.Width > 0 {
		width = fmt.Sprintf("%d px wide", p.cfg.Width)
	}
	return fmt.Sprintf("mjpeg preview %d fps at %s, quality %d", p.cfg.FPS, width, p.cfg.Quality)
}

// Run previews the source until ctx is done.
//
// It costs nothing until somebody is watching: with no subscribers the frames
// are dropped as they arrive rather than decoded, and the first viewer to
// arrive is served the next capture rather than a picture from before they came.
func (p *Preview) Run(ctx context.Context) {
	ch, cancel := p.cfg.Source.Subscribe(ctx)
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			return
		case f, ok := <-ch:
			if !ok {
				return
			}
			if p.out.ViewerCount() == 0 {
				continue
			}
			if !p.due(p.now()) {
				continue
			}
			data, err := p.frame(f.Data)
			if err != nil {
				if s := err.Error(); s != p.lastErr {
					p.lastErr = s
					log.Printf("preview: %v", err)
				}
				continue
			}
			p.out.Publish(data)
		}
	}
}

// frame is one previewed picture.
//
// A source that already fits is recompressed rather than passed along, because a
// capture at 180 kB is most of the problem even when the size is not.
func (p *Preview) frame(src []byte) ([]byte, error) {
	img, err := jpeg.Decode(bytes.NewReader(src))
	if err != nil {
		return nil, fmt.Errorf("decode capture: %w", err)
	}
	if p.cfg.Width > 0 && img.Bounds().Dx() > p.cfg.Width {
		img = shrink(img, p.cfg.Width)
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: p.cfg.Quality}); err != nil {
		return nil, fmt.Errorf("encode preview: %w", err)
	}
	return buf.Bytes(), nil
}

// shrink is img scaled down to width, keeping the aspect ratio.
//
// Every destination pixel is the average of the source pixels it covers, which
// is the filter that suits a large reduction: picking one source pixel per
// destination pixel is cheaper and turns text into a dotted mess, and unreadable
// is the one thing a preview must not be.
//
// The average is taken in RGBA, which spends a full size conversion of the frame
// that averaging the YCbCr the capture is already in would have saved. It buys
// one code path for every picture a capture might turn out to be, at ten frames
// a second on a stream that is about to be switched off.
func shrink(src image.Image, width int) *image.RGBA {
	b := src.Bounds()
	// A reduction only: a preview of a small screen is that screen, and the
	// average of a single pixel is that pixel, so widening is never wanted.
	width = min(width, b.Dx())
	height := max(1, b.Dy()*width/b.Dx())
	full := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(full, full.Rect, src, b.Min, draw.Src)

	dst := image.NewRGBA(image.Rect(0, 0, width, height))
	rows, cols := spans(height, b.Dy()), spans(width, b.Dx())
	for y := range rows {
		y0, y1 := rows[y].lo, rows[y].hi
		out := dst.Pix[y*dst.Stride : y*dst.Stride+4*width]
		for x := range width {
			x0, x1 := cols[x].lo, cols[x].hi
			var sr, sg, sb, n uint32
			for sy := y0; sy < y1; sy++ {
				line := full.Pix[sy*full.Stride:]
				for sx := x0; sx < x1; sx++ {
					// A capture is opaque, so the fourth byte is the same
					// everywhere and is written rather than averaged.
					p := line[sx*4 : sx*4+4 : sx*4+4]
					sr += uint32(p[0])
					sg += uint32(p[1])
					sb += uint32(p[2])
					n++
				}
			}
			out[x*4], out[x*4+1], out[x*4+2], out[x*4+3] =
				byte(sr/n), byte(sg/n), byte(sb/n), 0xff
		}
	}
	return dst
}

// interval is the half open range of source pixels one row or column of a
// reduced picture covers.
type interval struct{ lo, hi int }

// spans lays m source rows or samples out over n destination ones. No
// destination pixel is left without at least one source pixel, however far the
// picture is being reduced.
func spans(n, m int) []interval {
	out := make([]interval, n)
	for i := range out {
		lo := i * m / n
		hi := (i + 1) * m / n
		if hi <= lo {
			hi = lo + 1
		}
		out[i] = interval{lo, hi}
	}
	return out
}
