package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// transferIdleBudget allows for bandwidth shared by concurrent parts and server
// acknowledgement. It is renewed by activity, never a whole-file deadline.
func transferIdleBudget(size int64, concurrency int) time.Duration {
	seconds := min(int64(90), max(0, size>>20)*int64(max(1, min(concurrency, 32))))
	return 30*time.Second + time.Duration(seconds)*time.Second
}

func (cfg *config) startTransfer(parent context.Context, size int64, concurrency int) (context.Context, *http.Client, func(int), func()) {
	ctx, cancel := context.WithCancelCause(parent)
	idle := max(transferIdleBudget(size, concurrency), cfg.timeout)
	var mu sync.Mutex
	deadline := time.Now().Add(idle)
	stopped := false
	var timer *time.Timer
	mu.Lock()
	timer = time.AfterFunc(idle, func() {
		mu.Lock()
		defer mu.Unlock()
		if stopped {
			return
		}
		if remaining := time.Until(deadline); remaining > 0 {
			timer.Reset(remaining)
			return
		}
		cancel(fmt.Errorf("transfer received/sent no data for %s: %w", idle, context.DeadlineExceeded))
	})
	mu.Unlock()
	touch := func(n int) {
		if n <= 0 {
			return
		}
		mu.Lock()
		deadline = time.Now().Add(idle)
		mu.Unlock()
	}
	stop := func() {
		mu.Lock()
		stopped = true
		timer.Stop()
		mu.Unlock()
		cancel(nil)
	}
	client := *cfg.client
	// API calls retain their short total timeout; healthy file transfers do not.
	client.Timeout = 0
	return ctx, &client, touch, stop
}

func transferError(ctx context.Context, err error) error {
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	return safeNetworkError(err)
}

type activityReader struct {
	reader io.Reader
	read   func(int)
}

// A transport may still be reading the request when Do returns. Closing this
// wrapper joins the current read and prevents callbacks after an attempt ends.
type uploadBody struct {
	mu     sync.Mutex
	reader io.Reader
	closed bool
}

func (b *uploadBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return 0, io.ErrClosedPipe
	}
	return b.reader.Read(p)
}

func (b *uploadBody) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	return nil
}

func (r *activityReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.read(n)
	return n, err
}

// Closing a response also releases its idle timer and request context.
type transferBody struct {
	io.ReadCloser
	touch   func(int)
	stop    func()
	failure func(error) error
}

func (b *transferBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.touch(n)
	if err != nil && err != io.EOF {
		err = b.failure(err)
	}
	return n, err
}

func (b *transferBody) Close() error {
	b.stop()
	return b.ReadCloser.Close()
}

func pauseRetry(ctx context.Context, attempt int) error {
	timer := time.NewTimer(time.Second << attempt)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-timer.C:
		return nil
	}
}
