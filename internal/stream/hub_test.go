package stream

import (
	"context"
	"testing"
	"time"
)

func TestSubscribeReceivesPublishedFrames(t *testing.T) {
	h := NewHub()
	ch, cancel := h.Subscribe(context.Background())
	defer cancel()

	// Publish then read, one frame at a time: an unread frame is replaced by
	// the next one (drop-oldest), so back-to-back publishes before a read
	// would only deliver the newest.
	for _, want := range []string{"one", "two"} {
		h.Publish([]byte(want))
		select {
		case f := <-ch:
			if string(f.Data) != want {
				t.Fatalf("got frame %q, want %q", f.Data, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for frame %q", want)
		}
	}
}

// The frame a hub hands a viewer as it arrives is how a viewer arriving to a
// hub nobody has published to for a while is given a picture rather than a
// wait, so throwing that frame away is what makes the next one wait.
func TestInvalidateLeavesTheNextViewerToWait(t *testing.T) {
	h := NewHub()
	h.Publish([]byte("old"))

	h.Invalidate()
	if _, ok := h.Latest(); ok {
		t.Error("the frame was still on offer after an invalidate")
	}

	ch, cancel := h.Subscribe(context.Background())
	defer cancel()
	select {
	case f := <-ch:
		t.Errorf("a viewer arriving after an invalidate was handed %q", f.Data)
	default:
	}
}

func TestSubscribeSeesLatestFrameFirst(t *testing.T) {
	h := NewHub()
	h.Publish([]byte("old"))
	h.Publish([]byte("new"))

	ch, cancel := h.Subscribe(context.Background())
	defer cancel()

	select {
	case f := <-ch:
		if string(f.Data) != "new" {
			t.Fatalf("first frame is %q, want %q", f.Data, "new")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the initial frame")
	}
}

func TestSlowSubscriberDropsOldest(t *testing.T) {
	h := NewHub()
	ch, cancel := h.Subscribe(context.Background())
	defer cancel()

	// Nobody reads: each publish must replace the queued frame, not block.
	for i := 0; i < 100; i++ {
		h.Publish([]byte{byte(i)})
	}

	select {
	case f := <-ch:
		if f.Data[0] != 99 {
			t.Fatalf("queued frame is %d, want the newest (99)", f.Data[0])
		}
		if f.Seq != 100 {
			t.Fatalf("seq is %d, want 100", f.Seq)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the dropped-oldest frame")
	}
}

func TestCancelUnsubscribes(t *testing.T) {
	h := NewHub()
	ctx, stop := context.WithCancel(context.Background())
	ch, cancel := h.Subscribe(ctx)
	h.Publish([]byte("x"))
	<-ch // drain so the next publish has room

	stop()
	// Wait for the context watcher to unregister.
	deadline := time.Now().Add(time.Second)
	for h.ViewerCount() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := h.ViewerCount(); got != 0 {
		t.Fatalf("viewer count is %d after cancel, want 0", got)
	}
	if _, ok := <-ch; ok {
		t.Fatal("channel still open after cancel")
	}

	// Publishing after cancel must not panic on the closed channel.
	h.Publish([]byte("y"))

	cancel() // second call is a no-op
}

func TestViewerCount(t *testing.T) {
	h := NewHub()
	_, cancel1 := h.Subscribe(context.Background())
	_, cancel2 := h.Subscribe(context.Background())
	if got := h.ViewerCount(); got != 2 {
		t.Fatalf("viewer count is %d, want 2", got)
	}
	cancel1()
	if got := h.ViewerCount(); got != 1 {
		t.Fatalf("viewer count is %d, want 1", got)
	}
	cancel2()
	if got := h.ViewerCount(); got != 0 {
		t.Fatalf("viewer count is %d, want 0", got)
	}
}
