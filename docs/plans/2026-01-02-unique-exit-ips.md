# Unique Exit IP Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Ensure each backend connection gets a distinct exit IP from Cloudflare WARP, with retry logic if IPs collide.

**Architecture:** After starting a backend, query its exit IP via an HTTP request through the SOCKS5 proxy. Compare against existing backend IPs. If duplicate, wait 5 seconds, reconnect once, and verify uniqueness. Store exit IP on Backend struct for tracking.

**Tech Stack:** Go, net/http with SOCKS5 proxy, golang.org/x/net/proxy

---

## Task 1: Add exitIP Field to Backend Struct

**Files:**
- Modify: `core/rotation.go:38-47`

**Step 1: Update Backend struct**

Add `exitIP` field to track the exit IP:

```go
// Backend represents a single wiresocks backend instance.
type Backend struct {
	endpoint  string
	port      int
	ws        *wiresocks.WireSocks
	ctx       context.Context
	cancel    context.CancelCauseFunc
	healthy   *atomic.Bool
	startedAt time.Time
	exitIP    netip.Addr
}
```

**Step 2: Run build to verify syntax**

Run: `go build ./...`
Expected: PASS

**Step 3: Commit**

```bash
git add core/rotation.go
git commit -m "feat(rotation): add exitIP field to Backend struct"
```

---

## Task 2: Create getExitIP Function

**Files:**
- Modify: `core/rotation.go`

**Step 1: Add imports**

Add to imports if not present:

```go
import (
	"io"
	"net/http"
	"strings"

	"golang.org/x/net/proxy"
)
```

**Step 2: Add getExitIP function**

Add after `testSocks5Connection` function:

```go
// getExitIP queries the exit IP of a backend by making an HTTP request through its SOCKS5 proxy.
func getExitIP(proxyAddr string, timeout time.Duration) (netip.Addr, error) {
	dialer, err := proxy.SOCKS5("tcp", proxyAddr, nil, proxy.Direct)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("failed to create SOCKS5 dialer: %w", err)
	}

	transport := &http.Transport{
		Dial: dialer.Dial,
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   timeout,
	}

	resp, err := client.Get("https://1.1.1.1/cdn-cgi/trace")
	if err != nil {
		return netip.Addr{}, fmt.Errorf("failed to query exit IP: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("failed to read response: %w", err)
	}

	// Parse response - format is key=value lines, we want ip=x.x.x.x
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, "ip=") {
			ipStr := strings.TrimPrefix(line, "ip=")
			ipStr = strings.TrimSpace(ipStr)
			addr, err := netip.ParseAddr(ipStr)
			if err != nil {
				return netip.Addr{}, fmt.Errorf("failed to parse IP %q: %w", ipStr, err)
			}
			return addr, nil
		}
	}

	return netip.Addr{}, errors.New("no IP found in response")
}
```

**Step 3: Run build to verify syntax**

Run: `go build ./...`
Expected: PASS

**Step 4: Commit**

```bash
git add core/rotation.go
git commit -m "feat(rotation): add getExitIP function to query backend exit IP"
```

---

## Task 3: Create Helper to Collect Current Exit IPs

**Files:**
- Modify: `core/rotation.go`

**Step 1: Add getUsedExitIPs helper**

Add this helper method to RotationEngine:

```go
// getUsedExitIPs returns a set of exit IPs currently in use by backends.
// Caller must hold poolMu read lock.
func (r *RotationEngine) getUsedExitIPs() map[netip.Addr]bool {
	usedIPs := make(map[netip.Addr]bool)
	for _, b := range r.backends {
		if b.exitIP.IsValid() {
			usedIPs[b.exitIP] = true
		}
	}
	return usedIPs
}
```

**Step 2: Run build to verify syntax**

Run: `go build ./...`
Expected: PASS

**Step 3: Commit**

```bash
git add core/rotation.go
git commit -m "feat(rotation): add getUsedExitIPs helper method"
```

---

## Task 4: Create startBackendWithUniqueIP Function

**Files:**
- Modify: `core/rotation.go`

**Step 1: Add startBackendWithUniqueIP function**

Add this function that wraps startBackend with IP uniqueness check:

```go
// startBackendWithUniqueIP starts a backend and ensures it has a unique exit IP.
// If the exit IP matches an existing backend, it waits 5 seconds and retries once.
func (r *RotationEngine) startBackendWithUniqueIP(endpoint string, port int, usedIPs map[netip.Addr]bool) (*Backend, error) {
	backend, err := r.startBackend(endpoint, port)
	if err != nil {
		return nil, err
	}

	// Query exit IP
	proxyAddr := fmt.Sprintf("127.0.0.1:%d", port)
	exitIP, err := getExitIP(proxyAddr, 10*time.Second)
	if err != nil {
		log.Warnw("Failed to get exit IP for backend",
			zap.Int("port", port),
			zap.Error(err))
		// Continue without IP tracking - backend is still usable
		return backend, nil
	}

	backend.exitIP = exitIP
	log.Infow("Backend exit IP detected",
		zap.Int("port", port),
		zap.String("exit_ip", exitIP.String()))

	// Check if IP is unique
	if !usedIPs[exitIP] {
		return backend, nil
	}

	log.Warnw("Backend has duplicate exit IP, will retry",
		zap.Int("port", port),
		zap.String("exit_ip", exitIP.String()))

	// Cancel current backend
	backend.cancel(nil)

	// Wait 5 seconds before retry
	select {
	case <-r.ctx.Done():
		return nil, r.ctx.Err()
	case <-time.After(5 * time.Second):
	}

	// Retry once
	backend, err = r.startBackend(endpoint, port)
	if err != nil {
		return nil, fmt.Errorf("retry failed: %w", err)
	}

	// Query exit IP again
	exitIP, err = getExitIP(proxyAddr, 10*time.Second)
	if err != nil {
		log.Warnw("Failed to get exit IP on retry",
			zap.Int("port", port),
			zap.Error(err))
		return backend, nil
	}

	backend.exitIP = exitIP
	log.Infow("Backend exit IP on retry",
		zap.Int("port", port),
		zap.String("exit_ip", exitIP.String()))

	if usedIPs[exitIP] {
		log.Warnw("Backend still has duplicate exit IP after retry, accepting anyway",
			zap.Int("port", port),
			zap.String("exit_ip", exitIP.String()))
	}

	return backend, nil
}
```

**Step 2: Run build to verify syntax**

Run: `go build ./...`
Expected: PASS

**Step 3: Commit**

```bash
git add core/rotation.go
git commit -m "feat(rotation): add startBackendWithUniqueIP with retry logic"
```

---

## Task 5: Update initializeBackends to Use Unique IP Logic

**Files:**
- Modify: `core/rotation.go:131-178`

**Step 1: Update initializeBackends function**

Replace the current initializeBackends to use the unique IP logic:

```go
// initializeBackends creates the initial pool of backend wiresocks instances.
func (r *RotationEngine) initializeBackends() error {
	log.Infow("Initializing backend pool",
		zap.String("endpoint", r.opts.Endpoint),
		zap.Int("pool_size", r.opts.PoolSize))

	usedIPs := make(map[netip.Addr]bool)

	for i := 0; i < r.opts.PoolSize; i++ {
		select {
		case <-r.ctx.Done():
			return r.ctx.Err()
		default:
		}

		port := BackendPortStart + i

		log.Infow("Starting backend",
			zap.String("endpoint", r.opts.Endpoint),
			zap.Int("port", port),
			zap.Int("index", i+1),
			zap.Int("total", r.opts.PoolSize))

		backend, err := r.startBackendWithUniqueIP(r.opts.Endpoint, port, usedIPs)
		if err != nil {
			log.Warnw("Failed to start backend",
				zap.Int("port", port),
				zap.Error(err))
			continue
		}

		r.poolMu.Lock()
		r.backends = append(r.backends, backend)
		r.poolMu.Unlock()

		// Track this backend's IP for subsequent backends
		if backend.exitIP.IsValid() {
			usedIPs[backend.exitIP] = true
		}

		log.Infow("Backend started successfully",
			zap.Int("port", backend.port),
			zap.String("exit_ip", backend.exitIP.String()),
			zap.Int("backends_ready", len(r.backends)))
	}

	if len(r.backends) == 0 {
		return errors.New("no backends started successfully")
	}

	minRequired := r.getEffectiveMinBackends()
	if len(r.backends) < minRequired {
		return fmt.Errorf("could not start minimum required backends: need %d, started %d", minRequired, len(r.backends))
	}

	return nil
}
```

**Step 2: Run build to verify syntax**

Run: `go build ./...`
Expected: PASS

**Step 3: Commit**

```bash
git add core/rotation.go
git commit -m "feat(rotation): update initializeBackends to ensure unique exit IPs"
```

---

## Task 6: Update rotateOneBackend for Unique IPs

**Files:**
- Modify: `core/rotation.go` (rotateOneBackend function)

**Step 1: Update rotateOneBackend to check IP uniqueness**

Find and update the `rotateOneBackend` function:

```go
// rotateOneBackend disconnects and reconnects a single backend.
func (r *RotationEngine) rotateOneBackend(index int) {
	r.poolMu.Lock()
	if index >= len(r.backends) {
		r.poolMu.Unlock()
		return
	}

	oldBackend := r.backends[index]
	port := oldBackend.port
	usedIPs := r.getUsedExitIPs()
	// Remove the old backend's IP from used set since we're replacing it
	delete(usedIPs, oldBackend.exitIP)
	r.poolMu.Unlock()

	log.Infow("Rotating backend",
		zap.Int("index", index),
		zap.Int("port", port),
		zap.String("old_exit_ip", oldBackend.exitIP.String()))

	// Use a different port temporarily for the new backend
	tempPort := port + 100

	// Start new backend with unique IP check
	newBackend, err := r.startBackendWithUniqueIP(r.opts.Endpoint, tempPort, usedIPs)
	if err != nil {
		log.Warnw("Failed to start replacement backend during rotation",
			zap.Int("index", index),
			zap.Error(err))
		return
	}

	// Swap backends
	r.poolMu.Lock()
	if index < len(r.backends) {
		// Cancel old backend
		oldBackend.cancel(nil)
		// Replace with new backend
		r.backends[index] = newBackend
	}
	r.poolMu.Unlock()

	log.Infow("Backend rotated successfully",
		zap.Int("index", index),
		zap.Int("port", newBackend.port),
		zap.String("new_exit_ip", newBackend.exitIP.String()))
}
```

**Step 2: Run build to verify syntax**

Run: `go build ./...`
Expected: PASS

**Step 3: Commit**

```bash
git add core/rotation.go
git commit -m "feat(rotation): update rotateOneBackend to ensure unique exit IPs"
```

---

## Task 7: Add Logging for Exit IPs in getNextBackend

**Files:**
- Modify: `core/rotation.go` (getNextBackend function)

**Step 1: Add exit IP to connection logging**

Find `getNextBackend` and add exit IP info to logs if present:

```go
// In getNextBackend, update the log statement to include exit IP
log.Debugw("Selected backend for connection",
	zap.Int("port", backend.port),
	zap.String("exit_ip", backend.exitIP.String()))
```

**Step 2: Run build to verify syntax**

Run: `go build ./...`
Expected: PASS

**Step 3: Commit**

```bash
git add core/rotation.go
git commit -m "feat(rotation): add exit IP logging to backend selection"
```

---

## Task 8: Build and Test

**Step 1: Build the binary**

Run: `go build ./...`
Expected: PASS

**Step 2: Run unit tests**

Run: `go test ./core/... -v`
Expected: PASS (or existing tests pass)

**Step 3: Integration test**

Run: `./cloudflare-warp rotate --socks-addr 127.0.0.1:1080 --pool-size 3`

Expected logs:
- "Backend exit IP detected" with different IPs for each backend
- If duplicates occur: "Backend has duplicate exit IP, will retry"
- After retry: "Backend exit IP on retry"

**Step 4: Verify unique IPs**

Run multiple curl requests and observe different IPs being used:
```bash
for i in {1..10}; do curl -x socks5://127.0.0.1:1080 https://1.1.1.1/cdn-cgi/trace 2>/dev/null | grep ip=; done
```

**Step 5: Commit final state**

```bash
git add .
git commit -m "feat(rotation): complete unique exit IP implementation"
```

---

## Summary

This implementation:
1. Adds `exitIP` field to Backend struct for tracking
2. Uses Cloudflare's `1.1.1.1/cdn-cgi/trace` endpoint to detect exit IPs (reliable, fast)
3. Checks IP uniqueness when starting each backend
4. If duplicate IP detected: waits 5 seconds, reconnects once, accepts result
5. Applies same logic during background rotation
6. Logs exit IPs for debugging and monitoring
