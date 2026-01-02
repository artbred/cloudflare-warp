package core

import (
	"context"
	"fmt"
	"net/netip"
	"sync"
	"testing"
	"time"

	"go.uber.org/atomic"
)

func TestGetNextBackend_ConcurrentShrink(t *testing.T) {
	// This test verifies that getNextBackend doesn't panic when
	// the backend slice shrinks during selection
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	addr := netip.MustParseAddrPort("127.0.0.1:1080")
	r := &RotationEngine{
		ctx:       ctx,
		cancel:    cancel,
		opts:      RotationConfig{FrontendAddr: &addr, PoolSize: 5},
		backends:  make([]*Backend, 0),
		nextIndex: atomic.NewUint32(0),
	}

	// Add 5 backends
	for i := 0; i < 5; i++ {
		bCtx, bCancel := context.WithCancelCause(ctx)
		r.backends = append(r.backends, &Backend{
			endpoint: "test",
			port:     40000 + i,
			ctx:      bCtx,
			cancel:   bCancel,
			healthy:  atomic.NewBool(true),
		})
	}

	// Advance counter to near a high index
	r.nextIndex.Store(4)

	var wg sync.WaitGroup
	errChan := make(chan error, 100)

	// Concurrent reads
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					errChan <- fmt.Errorf("panic: %v", r)
				}
			}()
			for j := 0; j < 100; j++ {
				_ = r.getNextBackend()
			}
		}()
	}

	// Concurrent shrinks
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				r.poolMu.Lock()
				if len(r.backends) > 1 {
					r.backends = r.backends[:len(r.backends)-1]
				}
				r.poolMu.Unlock()
				time.Sleep(time.Millisecond)
			}
		}()
	}

	wg.Wait()
	close(errChan)

	for err := range errChan {
		t.Error(err)
	}
}
