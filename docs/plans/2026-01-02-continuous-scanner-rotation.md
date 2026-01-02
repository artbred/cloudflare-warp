# Continuous Scanner Rotation Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Replace cache-based endpoint discovery with continuous scanning to always maintain N healthy proxies

**Architecture:** Run a persistent background scanner that continuously finds new endpoints. When backends fail, immediately get fresh endpoints from the scanner (not cache). Remove all cache dependencies from rotation. The scanner runs infinitely, feeding a channel of discovered endpoints that the rotation engine consumes.

**Tech Stack:** Go, ipscanner, channels, goroutines

---

## Task 1: Remove Cache from RotationEngine

**Files:**
- Modify: `core/rotation.go:49-58` (RotationEngine struct)
- Modify: `core/rotation.go:61-71` (NewRotationEngine)

**Step 1: Remove cache field from RotationEngine struct**

In `core/rotation.go`, find the `RotationEngine` struct and remove the `cache` field:

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

**Step 2: Remove cache initialization from NewRotationEngine**

```go
// NewRotationEngine creates a new rotation engine.
func NewRotationEngine(ctx context.Context, opts RotationConfig) *RotationEngine {
	ctx, cancel := context.WithCancel(ctx)
	return &RotationEngine{
		ctx:       ctx,
		cancel:    cancel,
		opts:      opts,
		backends:  make([]*Backend, 0, opts.PoolSize),
		nextIndex: atomic.NewUint32(0),
	}
}
```

**Step 3: Run build to check for errors**

Run: `go build ./core/`
Expected: FAIL with errors about `r.cache` usage

**Step 4: Commit partial progress**

```bash
git add core/rotation.go
git commit -m "refactor(rotation): remove cache field from RotationEngine

Part 1 of removing cache dependency - struct cleanup"
```

---

## Task 2: Make ScanOptions Mandatory in RotationConfig

**Files:**
- Modify: `core/rotation.go:29-35` (RotationConfig)
- Modify: `cmd/rotate.go` (remove --scan flag, make scanning default)

**Step 1: Change ScanOptions from pointer to value in RotationConfig**

```go
// RotationConfig holds the configuration for the rotation engine.
type RotationConfig struct {
	FrontendAddr *netip.AddrPort
	PoolSize     int
	MinBackends  int
	DnsAddr      netip.Addr
	Scan         ScanOptions // No longer optional - scanning is always enabled
}
```

**Step 2: Update cmd/rotate.go - remove --scan flag and make scanning default**

Remove these lines from init():
```go
RotateCmd.Flags().Bool("scan", false, "Scan for endpoints on startup if cache is insufficient.")
viper.BindPFlag("rotate-scan", RotateCmd.Flags().Lookup("scan"))
```

Update the rotate() function to always use scanning:

```go
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
	if poolSize > 50 {
		rotateFatal(errors.New("pool-size cannot exceed 50"))
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

	useV4, useV6 := viper.GetBool("rotate-4"), viper.GetBool("rotate-6")
	if useV4 && useV6 {
		rotateFatal(errors.New("cannot force both v4 and v6 at the same time"))
	}
	if !useV4 && !useV6 {
		useV4, useV6 = true, true
	}

	// Build config - scanning is always enabled
	opts := core.RotationConfig{
		FrontendAddr: &socksAddr,
		PoolSize:     poolSize,
		MinBackends:  minBackends,
		DnsAddr:      dnsAddr,
		Scan: core.ScanOptions{
			V4:     useV4,
			V6:     useV6,
			MaxRTT: viper.GetDuration("rotate-scan-rtt"),
		},
	}

	log.Infow("Starting rotation proxy with continuous scanning",
		zap.String("socks-addr", socksAddr.String()),
		zap.Int("pool-size", opts.PoolSize),
		zap.Int("min-backends", opts.MinBackends))

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
```

**Step 3: Remove cache import from cmd/rotate.go**

Remove this import:
```go
"github.com/artbred/cloudflare-warp/core/cache"
```

And remove any code that uses `cache.NewCache()`.

**Step 4: Build to verify**

Run: `go build ./cmd/`
Expected: May still fail due to rotation.go cache references

**Step 5: Commit**

```bash
git add core/rotation.go cmd/rotate.go
git commit -m "refactor(rotation): make scanning mandatory, remove --scan flag

Scanning is now always enabled for rotation. Cache-based endpoint
discovery is being removed."
```

---

## Task 3: Add Background Scanner to RotationEngine

**Files:**
- Modify: `core/rotation.go` (add scanner fields and methods)

**Step 1: Add scanner fields to RotationEngine**

Update the struct:

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

	// Scanner for continuous endpoint discovery
	scanner         *ipscanner.IPScanner
	endpointChan    chan string
	usedEndpoints   map[string]bool
	usedEndpointsMu sync.Mutex
}
```

**Step 2: Add ipscanner import**

Add to imports:
```go
"github.com/artbred/cloudflare-warp/ipscanner"
```

**Step 3: Update NewRotationEngine to initialize scanner fields**

```go
func NewRotationEngine(ctx context.Context, opts RotationConfig) *RotationEngine {
	ctx, cancel := context.WithCancel(ctx)
	return &RotationEngine{
		ctx:           ctx,
		cancel:        cancel,
		opts:          opts,
		backends:      make([]*Backend, 0, opts.PoolSize),
		nextIndex:     atomic.NewUint32(0),
		endpointChan:  make(chan string, 100),
		usedEndpoints: make(map[string]bool),
	}
}
```

**Step 4: Add method to start background scanner**

```go
// startBackgroundScanner starts a persistent scanner that feeds endpoints to the channel.
func (r *RotationEngine) startBackgroundScanner() error {
	ident, err := cloudflare.LoadOrCreateIdentity()
	if err != nil {
		return fmt.Errorf("failed to load identity: %w", err)
	}

	r.scanner = ipscanner.NewScanner(
		ipscanner.WithContext(r.ctx),
		ipscanner.WithWarpPrivateKey(ident.PrivateKey),
		ipscanner.WithWarpPeerPublicKey(ident.Config.Peers[0].PublicKey),
		ipscanner.WithUseIPv4(r.opts.Scan.V4),
		ipscanner.WithUseIPv6(r.opts.Scan.V6),
		ipscanner.WithMaxDesirableRTT(r.opts.Scan.MaxRTT),
		ipscanner.WithCidrList(network.ScannerPrefixes()),
		ipscanner.WithIPQueueSize(r.opts.PoolSize * 3),
	)

	// Start scanner in background
	go func() {
		if err := r.scanner.Run(); err != nil {
			log.Errorw("Background scanner failed", zap.Error(err))
		}
	}()

	// Feed discovered endpoints to channel
	go r.feedEndpointsFromScanner()

	log.Info("Background scanner started")
	return nil
}

// feedEndpointsFromScanner continuously reads from scanner and feeds unique endpoints to channel.
func (r *RotationEngine) feedEndpointsFromScanner() {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-r.ctx.Done():
			return
		case <-ticker.C:
			ips := r.scanner.GetAvailableIPs()
			for _, ip := range ips {
				endpoint := ip.AddrPort.String()

				r.usedEndpointsMu.Lock()
				if !r.usedEndpoints[endpoint] {
					r.usedEndpoints[endpoint] = true
					r.usedEndpointsMu.Unlock()

					select {
					case r.endpointChan <- endpoint:
						log.Debugw("New endpoint discovered", zap.String("endpoint", endpoint))
					default:
						// Channel full, skip
					}
				} else {
					r.usedEndpointsMu.Unlock()
				}
			}
		}
	}
}

// getNextEndpoint gets the next available endpoint from the scanner.
// Blocks until an endpoint is available or context is cancelled.
func (r *RotationEngine) getNextEndpoint(timeout time.Duration) (string, error) {
	select {
	case <-r.ctx.Done():
		return "", r.ctx.Err()
	case endpoint := <-r.endpointChan:
		return endpoint, nil
	case <-time.After(timeout):
		return "", errors.New("timeout waiting for endpoint from scanner")
	}
}
```

**Step 5: Add network import**

Add to imports:
```go
"github.com/artbred/cloudflare-warp/cloudflare/network"
```

**Step 6: Build to verify syntax**

Run: `go build ./core/`
Expected: May fail due to remaining cache references

**Step 7: Commit**

```bash
git add core/rotation.go
git commit -m "feat(rotation): add background scanner for continuous endpoint discovery

Scanner runs persistently and feeds discovered endpoints to a channel.
Used endpoints are tracked to avoid duplicates."
```

---

## Task 4: Rewrite initializeBackends to Use Scanner

**Files:**
- Modify: `core/rotation.go` (initializeBackends function)

**Step 1: Rewrite initializeBackends**

Replace the entire `initializeBackends` function:

```go
// initializeBackends creates the initial pool of backend wiresocks instances using the scanner.
func (r *RotationEngine) initializeBackends() error {
	// Start the background scanner first
	if err := r.startBackgroundScanner(); err != nil {
		return fmt.Errorf("failed to start background scanner: %w", err)
	}

	// Wait for scanner to find initial endpoints
	log.Infow("Waiting for scanner to discover endpoints...",
		zap.Int("target_size", r.opts.PoolSize))

	minRequired := r.getEffectiveMinBackends()
	portIndex := 0
	maxAttempts := r.opts.PoolSize * 10 // Try many endpoints since some will fail

	for attempt := 0; attempt < maxAttempts && len(r.backends) < r.opts.PoolSize; attempt++ {
		select {
		case <-r.ctx.Done():
			return r.ctx.Err()
		default:
		}

		// Get next endpoint from scanner (wait up to 30s)
		endpoint, err := r.getNextEndpoint(30 * time.Second)
		if err != nil {
			log.Warnw("Timeout waiting for endpoint", zap.Error(err))
			continue
		}

		port := BackendPortStart + portIndex
		portIndex++

		log.Infow("Trying endpoint",
			zap.String("endpoint", endpoint),
			zap.Int("port", port))

		backend, err := r.startBackend(endpoint, port)
		if err != nil {
			log.Warnw("Failed to start backend",
				zap.String("endpoint", endpoint),
				zap.Error(err))
			continue
		}

		r.poolMu.Lock()
		r.backends = append(r.backends, backend)
		r.poolMu.Unlock()

		log.Infow("Backend started successfully",
			zap.String("endpoint", backend.endpoint),
			zap.Int("port", backend.port),
			zap.Int("backends_ready", len(r.backends)))
	}

	if len(r.backends) == 0 {
		return errors.New("no backends started successfully - scanner found no working endpoints")
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

	return nil
}
```

**Step 2: Build to verify**

Run: `go build ./core/`
Expected: May still fail due to replaceBackends cache usage

**Step 3: Commit**

```bash
git add core/rotation.go
git commit -m "refactor(rotation): rewrite initializeBackends to use scanner

No longer uses cache - gets all endpoints from background scanner.
Waits for scanner to discover working endpoints."
```

---

## Task 5: Rewrite replaceBackends to Use Scanner

**Files:**
- Modify: `core/rotation.go` (replaceBackends function)

**Step 1: Rewrite replaceBackends**

Replace the entire `replaceBackends` function:

```go
// replaceBackends attempts to start new backends on the given ports using the scanner.
func (r *RotationEngine) replaceBackends(ports []int) {
	// Check if engine is shutting down
	select {
	case <-r.ctx.Done():
		return
	default:
	}

	log.Infow("Attempting to replace failed backends",
		zap.Int("count", len(ports)))

	for _, port := range ports {
		// Try up to 5 endpoints per port
		for attempt := 0; attempt < 5; attempt++ {
			select {
			case <-r.ctx.Done():
				return
			default:
			}

			// Get next endpoint from scanner (wait up to 10s)
			endpoint, err := r.getNextEndpoint(10 * time.Second)
			if err != nil {
				log.Warnw("No endpoint available for replacement",
					zap.Int("port", port),
					zap.Error(err))
				break
			}

			backend, err := r.startBackend(endpoint, port)
			if err != nil {
				log.Warnw("Failed to start replacement backend",
					zap.String("endpoint", endpoint),
					zap.Int("port", port),
					zap.Error(err))
				continue
			}

			r.poolMu.Lock()
			r.backends = append(r.backends, backend)
			r.poolMu.Unlock()

			log.Infow("Replacement backend started",
				zap.String("endpoint", backend.endpoint),
				zap.Int("port", backend.port))
			break // Success, move to next port
		}
	}
}
```

**Step 2: Update checkAndReplaceUnhealthyBackends to not pass failedEndpoints**

Find the call to `replaceBackends` and change it:

From:
```go
go r.replaceBackends(availablePorts, failedEndpoints)
```

To:
```go
go r.replaceBackends(availablePorts)
```

**Step 3: Remove cache.RecordFailure calls**

Find and remove these lines in `checkAndReplaceUnhealthyBackends`:
```go
r.cache.RecordFailure(backend.endpoint)
```

There should be two occurrences - remove both.

**Step 4: Build to verify**

Run: `go build ./core/`
Expected: SUCCESS

**Step 5: Run tests**

Run: `go test -v ./core/`
Expected: All tests pass

**Step 6: Commit**

```bash
git add core/rotation.go
git commit -m "refactor(rotation): rewrite replaceBackends to use scanner

Replacement backends now come from the continuous scanner,
not from cache. Removed all cache.RecordFailure calls."
```

---

## Task 6: Remove Cache Import and Cleanup

**Files:**
- Modify: `core/rotation.go` (remove cache import)

**Step 1: Remove cache import**

Remove this line from imports:
```go
"github.com/artbred/cloudflare-warp/core/cache"
```

**Step 2: Remove getScannerEndpoints function**

Delete the entire `getScannerEndpoints` function (it's no longer used):

```go
// DELETE THIS ENTIRE FUNCTION
func (r *RotationEngine) getScannerEndpoints() ([]string, error) {
	...
}
```

**Step 3: Update Stop() to stop scanner**

Update the `Stop` function:

```go
// Stop gracefully stops the rotation engine.
func (r *RotationEngine) Stop() {
	log.Info("Stopping rotation engine...")
	r.cancel()

	// Stop scanner
	if r.scanner != nil {
		r.scanner.Stop()
	}

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

**Step 4: Build and test**

Run: `go build ./... && go test -v ./core/`
Expected: Build succeeds, all tests pass

**Step 5: Commit**

```bash
git add core/rotation.go
git commit -m "refactor(rotation): remove cache import and cleanup

Rotation engine no longer depends on cache at all.
All endpoint discovery is done via continuous scanning."
```

---

## Task 7: Update Help Text and Final Testing

**Files:**
- Modify: `cmd/rotate.go` (update command description)

**Step 1: Update command description**

```go
var RotateCmd = &cobra.Command{
	Use:   "rotate",
	Short: "Run WARP proxy with IP rotation and continuous scanning",
	Long: `Run the Cloudflare WARP proxy with IP rotation.
This command starts a SOCKS5 proxy that rotates through multiple WARP endpoints,
giving each new connection a different exit IP address.

The rotation pool is populated by continuous background scanning for working
WARP endpoints. The scanner runs indefinitely, ensuring fresh endpoints are
always available for rotation and replacement of failed backends.`,
	Run: rotate,
}
```

**Step 2: Full build**

Run: `go build -o warp ./`
Expected: SUCCESS

**Step 3: Run all tests**

Run: `go test -race ./...`
Expected: All tests pass (except pre-existing cloudflare/identity_test.go)

**Step 4: Commit**

```bash
git add cmd/rotate.go
git commit -m "docs(rotate): update help text for continuous scanning

Rotate command now uses continuous scanning, not cache-based
endpoint discovery."
```

---

## Summary of Changes

| File | Change |
|------|--------|
| `core/rotation.go` | Remove cache dependency, add background scanner, rewrite initializeBackends and replaceBackends |
| `cmd/rotate.go` | Remove --scan flag (always enabled), remove cache usage, update help text |

## Test Commands

```bash
# Build
go build ./...

# Run tests
go test -v ./core/

# Run with race detector
go test -race ./...

# Manual test
./warp rotate --socks-addr 127.0.0.1:1080 --pool-size 3 --min-backends 2
```
