// Package stream fans captured frames out to connected viewers.
package stream

import (
	"context"
	"sync"
	"time"
)

// Frame is one screen frame.
//
// Keyframe and Duration are only meaningful for encoded video: Keyframe marks a
// frame a viewer can start decoding from, and Duration is how long the frame
// should be displayed. MJPEG frames set neither.
type Frame struct {
	Data     []byte
	Seq      uint64
	Time     time.Time
	Keyframe bool
	Duration time.Duration
}

// Hub keeps the most recent frame and fans it out to subscribers.
//
// Each subscriber has a one-slot queue: when a slow viewer falls behind the
// oldest queued frame is dropped, so the capture loop is never blocked by a
// stalled client.
type Hub struct {
	mu     sync.Mutex
	latest *Frame
	seq    uint64
	subs   map[chan Frame]struct{}
}

// NewHub returns an empty hub.
func NewHub() *Hub {
	return &Hub{subs: make(map[chan Frame]struct{})}
}

// Publish stores data as the newest frame and delivers it to subscribers.
func (h *Hub) Publish(data []byte) {
	h.PublishFrame(Frame{Data: data})
}

// PublishFrame stores f as the newest frame and delivers it to subscribers,
// assigning it the next sequence number.
func (h *Hub) PublishFrame(f Frame) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seq++
	f.Seq = h.seq
	if f.Time.IsZero() {
		f.Time = time.Now()
	}
	h.latest = &f
	for ch := range h.subs {
		// Drop the queued frame (if any) to make room for the fresh one.
		select {
		case <-ch:
		default:
		}
		select {
		case ch <- f:
		default:
		}
	}
}

// Latest returns the most recent frame, if any.
func (h *Hub) Latest() (Frame, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.latest == nil {
		return Frame{}, false
	}
	return *h.latest, true
}

// Subscribe registers a viewer. The current frame, when one exists, is
// queued immediately. The returned cancel function unregisters the viewer
// and closes the channel; it is safe to call more than once. Cancelling ctx
// does the same.
func (h *Hub) Subscribe(ctx context.Context) (<-chan Frame, func()) {
	ch := make(chan Frame, 1)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	if h.latest != nil {
		ch <- *h.latest
	}
	h.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subs, ch)
			h.mu.Unlock()
			close(ch)
		})
	}
	if ctx.Done() != nil {
		go func() {
			<-ctx.Done()
			cancel()
		}()
	}
	return ch, cancel
}

// ViewerCount reports how many viewers are subscribed.
func (h *Hub) ViewerCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}
