# Simple Rotation Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Simplify rotation to use a single endpoint (`engage.cloudflareclient.com:2408`) with background reconnection cycling.

**Architecture:** Remove all IP scanning. Create N backend connections to the same WARP endpoint. A background goroutine periodically disconnects and reconnects backends one at a time, cycling through them to refresh connections and potentially get different exit IPs.

**Tech Stack:** Go, wiresocks, WireGuard

---

## Task 1: Simplify RotationConfig

**Files:**
- Modify: `core/rotation.go:30-37`

**Step 1: Remove ScanOptions from RotationConfig**

Replace the current RotationConfig with a simplified version:

```go
// RotationConfig holds the configuration for the rotation engine.
type RotationConfig struct {
	FrontendAddr     *netip.AddrPort
	PoolSize         int
	MinBackends      int           // Minimum healthy backends required (default: 1)
	DnsAddr          netip.Addr
	Endpoint         string        // WARP endpoint (default: engage.cloudflareclient.com:2408)
	RotationInterval time.Duration // How often to rotate backends (default: 5 minutes)
}
```

**Step 2: Run build to verify syntax**

Run: `go build ./...`
Expected: Build errors (ScanOptions references)

**Step 3: Commit**

```bash
git add core/rotation.go
git commit -m "refactor(rotation): simplify RotationConfig, remove ScanOptions"
```

---

## Task 2: Remove Scanner from RotationEngine

**Files:**
- Modify: `core/rotation.go:50-65`

**Step 1: Simplify RotationEngine struct**

Remove scanner-related fields:

```go
// RotationEngine manages a pool of wiresocks backends with round-robin selection.
type RotationEngine struct {
	ctx       context.Context
	cancel    context.CancelFunc
	opts      RotationConfig
	backends  []*Backend
	nextIndex *atomic.Uint32
	poolMu    sync.RWMutex
	frontend  *FrontendProxy
}
```

**Step 2: Update NewRotationEngine**

```go
// NewRotationEngine creates a new rotation engine.
func NewRotationEngine(ctx context.Context, opts RotationConfig) *RotationEngine {
	ctx, cancel := context.WithCancel(ctx)

	// Set defaults
	if opts.Endpoint == "" {
		opts.Endpoint = "engage.cloudflareclient.com:2408"
	}
	if opts.RotationInterval == 0 {
		opts.RotationInterval = 5 * time.Minute
	}

	return &RotationEngine{
		ctx:       ctx,
		cancel:    cancel,
		opts:      opts,
		backends:  make([]*Backend, 0, opts.PoolSize),
		nextIndex: atomic.NewUint32(0),
	}
}
```

**Step 3: Commit**

```bash
git add core/rotation.go
git commit -m "refactor(rotation): remove scanner from RotationEngine"
```

---

## Task 3: Rewrite initializeBackends

**Files:**
- Modify: `core/rotation.go` (initializeBackends function)

**Step 1: Replace initializeBackends with simple version**

```go
// initializeBackends creates the initial pool of backend wiresocks instances.
func (r *RotationEngine) initializeBackends() error {
	log.Infow("Initializing backend pool",
		zap.String("endpoint", r.opts.Endpoint),
		zap.Int("pool_size", r.opts.PoolSize))

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

		backend, err := r.startBackend(r.opts.Endpoint, port)
		if err != nil {
			log.Warnw("Failed to start backend",
				zap.Int("port", port),
				zap.Error(err))
			continue
		}

		r.poolMu.Lock()
		r.backends = append(r.backends, backend)
		r.poolMu.Unlock()

		log.Infow("Backend started successfully",
			zap.Int("port", backend.port),
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

**Step 2: Commit**

```bash
git add core/rotation.go
git commit -m "refactor(rotation): simplify initializeBackends to use single endpoint"
```

---

## Task 4: Add Background Rotation

**Files:**
- Modify: `core/rotation.go`

**Step 1: Add rotateBackends goroutine**

Add this new function and update Run() to call it:

```go
// rotateBackends periodically disconnects and reconnects backends to refresh connections.
func (r *RotationEngine) rotateBackends() {
	ticker := time.NewTicker(r.opts.RotationInterval)
	defer ticker.Stop()

	backendIndex := 0

	for {
		select {
		case <-r.ctx.Done():
			return
		case <-ticker.C:
			r.rotateOneBackend(backendIndex)
			backendIndex = (backendIndex + 1) % r.opts.PoolSize
		}
	}
}

// rotateOneBackend disconnects and reconnects a single backend.
func (r *RotationEngine) rotateOneBackend(index int) {
	r.poolMu.Lock()
	if index >= len(r.backends) {
		r.poolMu.Unlock()
		return
	}

	oldBackend := r.backends[index]
	port := oldBackend.port
	r.poolMu.Unlock()

	log.Infow("Rotating backend",
		zap.Int("index", index),
		zap.Int("port", port))

	// Start new backend first
	newBackend, err := r.startBackend(r.opts.Endpoint, port+100) // Use different port temporarily
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
		// Update port on new backend and replace
		r.backends[index] = newBackend
	}
	r.poolMu.Unlock()

	log.Infow("Backend rotated successfully",
		zap.Int("index", index),
		zap.Int("port", newBackend.port))
}
```

**Step 2: Update Run() to start rotation goroutine**

In the Run() function, add after `go r.monitorBackends()`:

```go
// Start backend rotation
go r.rotateBackends()
```

**Step 3: Commit**

```bash
git add core/rotation.go
git commit -m "feat(rotation): add background rotation to cycle backends"
```

---

## Task 5: Remove Scanner-Related Code

**Files:**
- Modify: `core/rotation.go`

**Step 1: Remove all scanner-related functions**

Delete these functions entirely:
- `startBackgroundScanner()`
- `feedEndpointsFromScanner()`
- `getNextEndpoint()`

**Step 2: Remove scanner imports**

Remove from imports:
```go
"github.com/artbred/cloudflare-warp/cloudflare/network"
"github.com/artbred/cloudflare-warp/ipscanner"
```

**Step 3: Simplify monitorBackends**

```go
// monitorBackends periodically checks backend health.
func (r *RotationEngine) monitorBackends() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.ctx.Done():
			return
		case <-ticker.C:
			r.checkBackendHealth()
		}
	}
}

// checkBackendHealth removes crashed backends.
func (r *RotationEngine) checkBackendHealth() {
	r.poolMu.Lock()
	defer r.poolMu.Unlock()

	var healthyBackends []*Backend
	for _, backend := range r.backends {
		select {
		case <-backend.ctx.Done():
			log.Warnw("Backend crashed, removing from pool",
				zap.Int("port", backend.port))
		default:
			if backend.healthy.Load() {
				healthyBackends = append(healthyBackends, backend)
			} else {
				log.Warnw("Backend unhealthy, removing from pool",
					zap.Int("port", backend.port))
				backend.cancel(nil)
			}
		}
	}

	r.backends = healthyBackends

	if len(r.backends) < r.getEffectiveMinBackends() {
		log.Errorw("CRITICAL: Below minimum healthy backends",
			zap.Int("healthy", len(r.backends)),
			zap.Int("minimum_required", r.getEffectiveMinBackends()))
	}
}
```

**Step 4: Update Stop() to remove scanner stop**

```go
// Stop gracefully stops the rotation engine.
func (r *RotationEngine) Stop() {
	log.Info("Stopping rotation engine...")
	r.cancel()

	// Stop all backends
	r.poolMu.Lock()
	for _, backend := range r.backends {
		if backend.cancel != nil {
			backend.cancel(nil)
		}
	}
	r.poolMu.Unlock()

	log.Info("Rotation engine stopped")
}
```

**Step 5: Commit**

```bash
git add core/rotation.go
git commit -m "refactor(rotation): remove all scanner-related code"
```

---

## Task 6: Update cmd/rotate.go

**Files:**
- Modify: `cmd/rotate.go`

**Step 1: Remove scanner flags and simplify**

```go
package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.uber.org/zap"

	"github.com/artbred/cloudflare-warp/core"
	"github.com/artbred/cloudflare-warp/log"
)

var RotateCmd = &cobra.Command{
	Use:   "rotate",
	Short: "Run WARP proxy with connection rotation",
	Long: `Run the Cloudflare WARP proxy with connection rotation.
This command starts a SOCKS5 proxy that maintains multiple connections to WARP,
rotating them in the background to potentially get different exit IPs.`,
	Run: rotate,
}

func init() {
	RotateCmd.Flags().String("socks-addr", "", "Socks5 proxy bind address (required, e.g., 127.0.0.1:1080).")
	RotateCmd.Flags().Int("pool-size", 3, "Number of backend connections (default: 3).")
	RotateCmd.Flags().Int("min-backends", 1, "Minimum healthy backends required (default: 1).")
	RotateCmd.Flags().String("dns", "1.1.1.1", "DNS server address to use.")
	RotateCmd.Flags().String("endpoint", "engage.cloudflareclient.com:2408", "WARP endpoint to connect to.")
	RotateCmd.Flags().Duration("rotation-interval", 5*time.Minute, "How often to rotate each backend connection.")

	viper.BindPFlag("rotate-socks-addr", RotateCmd.Flags().Lookup("socks-addr"))
	viper.BindPFlag("rotate-pool-size", RotateCmd.Flags().Lookup("pool-size"))
	viper.BindPFlag("rotate-min-backends", RotateCmd.Flags().Lookup("min-backends"))
	viper.BindPFlag("rotate-dns", RotateCmd.Flags().Lookup("dns"))
	viper.BindPFlag("rotate-endpoint", RotateCmd.Flags().Lookup("endpoint"))
	viper.BindPFlag("rotate-rotation-interval", RotateCmd.Flags().Lookup("rotation-interval"))
}

func rotate(cmd *cobra.Command, args []string) {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Validate flags
	socksAddrStr := viper.GetString("rotate-socks-addr")
	if socksAddrStr == "" {
		cmd.Help()
		fmt.Println("\nError: --socks-addr is required")
		return
	}

	socksAddr, err := netip.ParseAddrPort(socksAddrStr)
	if err != nil {
		rotateFatal(fmt.Errorf("invalid socks-addr: %w", err))
	}

	poolSize := viper.GetInt("rotate-pool-size")
	if poolSize < 1 {
		rotateFatal(errors.New("pool-size must be at least 1"))
	}
	if poolSize > 20 {
		rotateFatal(errors.New("pool-size cannot exceed 20"))
	}

	minBackends := viper.GetInt("rotate-min-backends")
	if minBackends < 1 {
		rotateFatal(errors.New("min-backends must be at least 1"))
	}
	if minBackends > poolSize {
		rotateFatal(errors.New("min-backends cannot exceed pool-size"))
	}

	dnsAddr, err := netip.ParseAddr(viper.GetString("rotate-dns"))
	if err != nil {
		rotateFatal(fmt.Errorf("invalid dns address: %w", err))
	}

	opts := core.RotationConfig{
		FrontendAddr:     &socksAddr,
		PoolSize:         poolSize,
		MinBackends:      minBackends,
		DnsAddr:          dnsAddr,
		Endpoint:         viper.GetString("rotate-endpoint"),
		RotationInterval: viper.GetDuration("rotate-rotation-interval"),
	}

	log.Infow("Starting rotation proxy",
		zap.String("socks-addr", socksAddr.String()),
		zap.String("endpoint", opts.Endpoint),
		zap.Int("pool-size", opts.PoolSize),
		zap.Duration("rotation-interval", opts.RotationInterval))

	// Create and run engine
	engine := core.NewRotationEngine(ctx, opts)
	defer engine.Stop()

	go func() {
		if err := engine.Run(); err != nil {
			log.Fatalw("Rotation engine failed", zap.Error(err))
		}
	}()

	<-ctx.Done()
	log.Info("Shutting down rotation proxy...")
}

func rotateFatal(err error) {
	log.Fatalw("Rotation proxy encountered a fatal error", zap.Error(err))
}
```

**Step 2: Run build**

Run: `go build ./...`
Expected: PASS

**Step 3: Commit**

```bash
git add cmd/rotate.go
git commit -m "refactor(cmd): simplify rotate command, remove scanner flags"
```

---

## Task 7: Clean Up Unused Code

**Files:**
- Modify: `core/rotation.go`

**Step 1: Remove unused functions**

Delete these if still present:
- `proactiveHealthCheck()`
- `proactiveHealthChecks()`
- `testSocks5Listening()` (if not used elsewhere)
- `checkAndReplaceUnhealthyBackends()` (replaced by checkBackendHealth)
- `replaceBackends()`

**Step 2: Remove ScanOptions type if still present**

Delete from core/scanner.go or rotation.go:
```go
type ScanOptions struct {
	V4         bool
	V6         bool
	MaxRTT     time.Duration
	PrivateKey string
	PublicKey  string
}
```

**Step 3: Run build and tests**

Run: `go build ./... && go test ./core/...`
Expected: PASS

**Step 4: Commit**

```bash
git add core/
git commit -m "refactor(rotation): clean up unused code"
```

---

## Task 8: Integration Test

**Step 1: Build the binary**

Run: `go build -o cloudflare-warp .`
Expected: PASS

**Step 2: Run rotation**

Run: `./cloudflare-warp rotate --socks-addr 127.0.0.1:1080 --pool-size 3`
Expected:
- See "Starting rotation proxy" with endpoint engage.cloudflareclient.com:2408
- See "Backend started successfully" x3
- See "Frontend proxy listening"

**Step 3: Test connectivity**

Run: `curl -x socks5://127.0.0.1:1080 ifconfig.me`
Expected: Returns an IP address

**Step 4: Wait for rotation**

Wait 5+ minutes or use `--rotation-interval 30s` for faster testing.
Expected: See "Rotating backend" and "Backend rotated successfully" logs

**Step 5: Commit final state**

```bash
git add .
git commit -m "feat(rotation): complete simple rotation implementation"
```
