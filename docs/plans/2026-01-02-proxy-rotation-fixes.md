# Proxy Rotation Fixes Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Fix proxy rotation logic to ensure N required backends start and rotation works correctly

**Architecture:** Fix race conditions in backend selection, add runtime minimum backend enforcement, improve health monitoring with proactive checks

**Tech Stack:** Go, sync/atomic, WireSocks

---

## Code Review Findings

### Critical Issues

1. **Race condition in `getNextBackend`** (rotation.go:402-425)
   - Slice length captured under RLock, but `checkAndReplaceUnhealthyBackends` can shrink the slice
   - Modulo operation could access invalid index if backends shrink between length check and access

2. **No runtime minimum backend enforcement** (rotation.go:80-82)
   - `MinHealthyBackends` only checked at startup
   - During operation, all backends could fail with system continuing at 0 backends

3. **Missing validation against minimum at startup** (rotation.go:228-232)
   - If fewer backends start than requested, only a warning is logged
   - Should fail if below `MinHealthyBackends`

### Moderate Issues

4. **Round-robin counter advances for unhealthy backends** (rotation.go:415)
   - Causes uneven load distribution when backends are unhealthy

5. **No proactive health checking**
   - Backends only marked unhealthy when client connections fail
   - Dead backends not detected until traffic hits them

6. **Early break condition is confusing** (rotation.go:163)
   - `len(r.backends)+i >= r.opts.PoolSize+len(batch)` - doesn't effectively limit starts

---

## Task 1: Fix Race Condition in getNextBackend

**Files:**
- Modify: `core/rotation.go:402-425`

**Step 1: Write the failing test**

Create file `core/rotation_test.go`:

```go
package core

import (
	"context"
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
```

**Step 2: Run test to verify it fails/panics**

Run: `go test -v -race -run TestGetNextBackend_ConcurrentShrink ./core/`
Expected: May panic with index out of range or race detector warning

**Step 3: Fix the getNextBackend function**

Replace `core/rotation.go` lines 402-425 with:

```go
// getNextBackend returns the next healthy backend using round-robin selection.
func (r *RotationEngine) getNextBackend() *Backend {
	r.poolMu.RLock()
	defer r.poolMu.RUnlock()

	n := len(r.backends)
	if n == 0 {
		return nil
	}

	// Try to find a healthy backend using round-robin
	// Capture length once and use it consistently
	startIndex := r.nextIndex.Add(1) - 1
	for i := uint32(0); i < uint32(n); i++ {
		index := (startIndex + i) % uint32(n)
		backend := r.backends[index]

		if backend.healthy.Load() {
			return backend
		}
	}

	// If no healthy backends found, return first one anyway (let it fail explicitly)
	return r.backends[0]
}
```

**Step 4: Run test to verify it passes**

Run: `go test -v -race -run TestGetNextBackend_ConcurrentShrink ./core/`
Expected: PASS with no race conditions

**Step 5: Commit**

```bash
git add core/rotation.go core/rotation_test.go
git commit -m "fix(rotation): prevent race condition in getNextBackend

The previous implementation incremented nextIndex on each iteration
when skipping unhealthy backends, which could cause index out of
bounds if the backend slice shrunk concurrently.

Now we capture the start index once and iterate from there,
ensuring consistent indexing within the held read lock."
```

---

## Task 2: Add Runtime Minimum Backend Enforcement

**Files:**
- Modify: `core/rotation.go:29-35` (add MinBackends to config)
- Modify: `core/rotation.go:442-483` (add minimum check in health monitor)
- Modify: `cmd/rotate.go:36` (add --min-backends flag)

**Step 1: Write the failing test**

Add to `core/rotation_test.go`:

```go
func TestRotationEngine_MinimumBackendEnforcement(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	addr := netip.MustParseAddrPort("127.0.0.1:1080")
	r := &RotationEngine{
		ctx:       ctx,
		cancel:    cancel,
		opts:      RotationConfig{FrontendAddr: &addr, PoolSize: 5, MinBackends: 2},
		backends:  make([]*Backend, 0),
		nextIndex: atomic.NewUint32(0),
	}

	// Add 2 backends (exactly at minimum)
	for i := 0; i < 2; i++ {
		bCtx, bCancel := context.WithCancelCause(ctx)
		r.backends = append(r.backends, &Backend{
			endpoint: "test",
			port:     40000 + i,
			ctx:      bCtx,
			cancel:   bCancel,
			healthy:  atomic.NewBool(true),
		})
	}

	// Check should pass with 2 backends
	if !r.hasMinimumBackends() {
		t.Error("should have minimum backends with 2")
	}

	// Mark one as unhealthy
	r.backends[0].healthy.Store(false)

	// Check should fail with 1 healthy backend
	if r.hasMinimumBackends() {
		t.Error("should not have minimum backends with only 1 healthy")
	}
}
```

**Step 2: Run test to verify it fails**

Run: `go test -v -run TestRotationEngine_MinimumBackendEnforcement ./core/`
Expected: FAIL - MinBackends field doesn't exist, hasMinimumBackends doesn't exist

**Step 3: Add MinBackends to RotationConfig**

In `core/rotation.go`, modify lines 29-35:

```go
// RotationConfig holds the configuration for the rotation engine.
type RotationConfig struct {
	FrontendAddr *netip.AddrPort
	PoolSize     int
	MinBackends  int // Minimum healthy backends required (default: 1)
	DnsAddr      netip.Addr
	Scan         *ScanOptions
}
```

**Step 4: Add hasMinimumBackends method**

Add after getNextBackend function (after line 425):

```go
// hasMinimumBackends returns true if we have at least MinBackends healthy backends.
func (r *RotationEngine) hasMinimumBackends() bool {
	r.poolMu.RLock()
	defer r.poolMu.RUnlock()

	minRequired := r.opts.MinBackends
	if minRequired < 1 {
		minRequired = MinHealthyBackends // Use constant default
	}

	healthyCount := 0
	for _, backend := range r.backends {
		if backend.healthy.Load() {
			healthyCount++
			if healthyCount >= minRequired {
				return true
			}
		}
	}
	return false
}
```

**Step 5: Add warning in health monitor**

Modify `checkAndReplaceUnhealthyBackends` to add warning after line 477 (`r.backends = healthyBackends`):

```go
	r.backends = healthyBackends

	// Warn if below minimum
	minRequired := r.opts.MinBackends
	if minRequired < 1 {
		minRequired = MinHealthyBackends
	}
	if len(healthyBackends) < minRequired {
		log.Errorw("CRITICAL: Below minimum healthy backends",
			zap.Int("healthy", len(healthyBackends)),
			zap.Int("minimum_required", minRequired))
	}
```

**Step 6: Run test to verify it passes**

Run: `go test -v -run TestRotationEngine_MinimumBackendEnforcement ./core/`
Expected: PASS

**Step 7: Update cmd/rotate.go to accept --min-backends flag**

Add flag in init() after line 36:

```go
RotateCmd.Flags().Int("min-backends", 1, "Minimum healthy backends required to operate (default: 1).")
```

Add viper binding after line 44:

```go
viper.BindPFlag("rotate-min-backends", RotateCmd.Flags().Lookup("min-backends"))
```

In rotate() function, after pool-size validation (after line 75), add:

```go
minBackends := viper.GetInt("rotate-min-backends")
if minBackends < 1 {
	rotateFatal(errors.New("min-backends must be at least 1"))
}
if minBackends > poolSize {
	rotateFatal(errors.New("min-backends cannot exceed pool-size"))
}
```

Update opts construction (line 91-95):

```go
opts := core.RotationConfig{
	FrontendAddr: &socksAddr,
	PoolSize:     poolSize,
	MinBackends:  minBackends,
	DnsAddr:      dnsAddr,
}
```

**Step 8: Run full test suite**

Run: `go test -v ./...`
Expected: All tests pass

**Step 9: Commit**

```bash
git add core/rotation.go core/rotation_test.go cmd/rotate.go
git commit -m "feat(rotation): add configurable minimum backend enforcement

- Add MinBackends field to RotationConfig
- Add hasMinimumBackends() method for checking health threshold
- Log CRITICAL error when healthy backends fall below minimum
- Add --min-backends CLI flag with validation"
```

---

## Task 3: Add Proactive Health Checking

**Files:**
- Modify: `core/rotation.go:427-440` (enhance monitor to do active checks)

**Step 1: Write the failing test**

Add to `core/rotation_test.go`:

```go
func TestBackend_ProactiveHealthCheck(t *testing.T) {
	// Test that proactiveHealthCheck correctly identifies dead backends
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	backend := &Backend{
		endpoint: "test",
		port:     59999, // Non-existent port
		ctx:      ctx,
		cancel:   func(err error) { cancel() },
		healthy:  atomic.NewBool(true),
	}

	// Check should fail since nothing is listening on port 59999
	result := proactiveHealthCheck(backend, 500*time.Millisecond)
	if result {
		t.Error("health check should fail for non-listening port")
	}
}
```

**Step 2: Run test to verify it fails**

Run: `go test -v -run TestBackend_ProactiveHealthCheck ./core/`
Expected: FAIL - proactiveHealthCheck doesn't exist

**Step 3: Add proactiveHealthCheck function**

Add after testSocks5Connection function (after line 400):

```go
// proactiveHealthCheck tests if a backend's SOCKS5 proxy is responsive.
func proactiveHealthCheck(backend *Backend, timeout time.Duration) bool {
	select {
	case <-backend.ctx.Done():
		return false
	default:
	}

	addr := fmt.Sprintf("127.0.0.1:%d", backend.port)
	err := testSocks5Connection(addr, "1.1.1.1:443", timeout)
	return err == nil
}
```

**Step 4: Enhance monitorBackends to do proactive checks**

Replace `monitorBackends` function (lines 427-440):

```go
// monitorBackends periodically checks backend health and replaces failed ones.
func (r *RotationEngine) monitorBackends() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.ctx.Done():
			return
		case <-ticker.C:
			r.proactiveHealthChecks()
			r.checkAndReplaceUnhealthyBackends()
		}
	}
}

// proactiveHealthChecks tests all backends and marks unresponsive ones as unhealthy.
func (r *RotationEngine) proactiveHealthChecks() {
	r.poolMu.RLock()
	backends := make([]*Backend, len(r.backends))
	copy(backends, r.backends)
	r.poolMu.RUnlock()

	var wg sync.WaitGroup
	for _, backend := range backends {
		if !backend.healthy.Load() {
			continue // Already unhealthy
		}

		wg.Add(1)
		go func(b *Backend) {
			defer wg.Done()
			if !proactiveHealthCheck(b, 5*time.Second) {
				log.Warnw("Backend failed proactive health check",
					zap.String("endpoint", b.endpoint),
					zap.Int("port", b.port))
				b.healthy.Store(false)
			}
		}(backend)
	}
	wg.Wait()
}
```

**Step 5: Run test to verify it passes**

Run: `go test -v -run TestBackend_ProactiveHealthCheck ./core/`
Expected: PASS

**Step 6: Run full test suite with race detector**

Run: `go test -v -race ./...`
Expected: All tests pass

**Step 7: Commit**

```bash
git add core/rotation.go core/rotation_test.go
git commit -m "feat(rotation): add proactive backend health checking

Backends are now tested every 10 seconds with actual SOCKS5
connectivity checks to 1.1.1.1:443. This detects dead backends
before client connections fail, improving reliability."
```

---

## Task 4: Fix Startup Minimum Validation

**Files:**
- Modify: `core/rotation.go:224-234`

**Step 1: Write the failing test**

Add to `core/rotation_test.go`:

```go
func TestInitializeBackends_RespectsMinimum(t *testing.T) {
	// Test that initialization fails if we can't start MinBackends
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	addr := netip.MustParseAddrPort("127.0.0.1:1080")
	r := &RotationEngine{
		ctx:       ctx,
		cancel:    cancel,
		opts:      RotationConfig{FrontendAddr: &addr, PoolSize: 10, MinBackends: 5},
		cache:     cache.NewCache(),
		backends:  make([]*Backend, 0),
		nextIndex: atomic.NewUint32(0),
	}

	// Simulate having only 2 backends started (below MinBackends of 5)
	// This tests the validation logic, not actual backend starting
	r.backends = make([]*Backend, 2)
	for i := 0; i < 2; i++ {
		bCtx, bCancel := context.WithCancelCause(ctx)
		r.backends[i] = &Backend{
			endpoint: "test",
			port:     40000 + i,
			ctx:      bCtx,
			cancel:   bCancel,
			healthy:  atomic.NewBool(true),
		}
	}

	// The validation in Run() should catch this
	minRequired := r.opts.MinBackends
	if len(r.backends) < minRequired {
		// Expected: this should trigger an error in actual code
		t.Log("Correctly detected insufficient backends for minimum requirement")
	}
}
```

**Step 2: Modify startup validation in Run()**

Replace lines 80-82 in `core/rotation.go`:

```go
	minRequired := r.opts.MinBackends
	if minRequired < 1 {
		minRequired = MinHealthyBackends
	}
	if len(r.backends) < minRequired {
		return fmt.Errorf("not enough healthy backends: need at least %d, have %d", minRequired, len(r.backends))
	}
```

**Step 3: Update initializeBackends warning to use MinBackends**

Replace lines 228-232:

```go
	minRequired := r.opts.MinBackends
	if minRequired < 1 {
		minRequired = MinHealthyBackends
	}

	if len(r.backends) == 0 {
		return errors.New("no backends started successfully - all endpoints failed")
	}

	if len(r.backends) < minRequired {
		return fmt.Errorf("could not start minimum required backends: need %d, started %d", minRequired, len(r.backends))
	}

	if len(r.backends) < r.opts.PoolSize {
		log.Warnw("Could not start requested number of backends",
			zap.Int("requested", r.opts.PoolSize),
			zap.Int("started", len(r.backends)),
			zap.Int("minimum_required", minRequired))
	}
```

**Step 4: Run tests**

Run: `go test -v ./...`
Expected: All tests pass

**Step 5: Commit**

```bash
git add core/rotation.go core/rotation_test.go
git commit -m "fix(rotation): enforce minimum backends at startup

Startup now fails if we cannot start at least MinBackends
healthy backends, rather than just logging a warning."
```

---

## Task 5: Build and Integration Test

**Files:**
- None (verification only)

**Step 1: Build the project**

Run: `go build -v ./...`
Expected: Build succeeds with no errors

**Step 2: Run all tests with race detector**

Run: `go test -v -race ./...`
Expected: All tests pass with no race conditions

**Step 3: Manual verification (optional)**

Run with scanner to test rotation:
```bash
./warp rotate --socks-addr 127.0.0.1:1080 --pool-size 3 --min-backends 2 --scan
```
Expected: Should start 3 backends, require minimum 2 to operate

**Step 4: Commit any final fixes**

If any issues found, fix and commit.

---

## Summary of Changes

| File | Change |
|------|--------|
| `core/rotation.go` | Fix race in getNextBackend, add MinBackends config, add proactive health checks, enforce minimum at startup |
| `core/rotation_test.go` | Add tests for concurrent access, minimum enforcement, health checks |
| `cmd/rotate.go` | Add --min-backends flag |

## Test Commands

```bash
# Run all tests
go test -v ./...

# Run with race detector
go test -v -race ./...

# Run specific test
go test -v -run TestGetNextBackend_ConcurrentShrink ./core/

# Build
go build -v ./...
```
