package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/shahradelahi/wiresocks"
	"go.uber.org/atomic"
	"go.uber.org/zap"

	"github.com/artbred/cloudflare-warp/cloudflare"
	"github.com/artbred/cloudflare-warp/cloudflare/network"
	"github.com/artbred/cloudflare-warp/ipscanner"
	"github.com/artbred/cloudflare-warp/log"
)

const (
	// BackendPortStart is the starting port for backend wiresocks instances
	BackendPortStart = 40000
	// MinHealthyBackends is the minimum number of healthy backends required to operate
	MinHealthyBackends = 1
)

// RotationConfig holds the configuration for the rotation engine.
type RotationConfig struct {
	FrontendAddr *netip.AddrPort
	PoolSize     int
	MinBackends  int // Minimum healthy backends required (default: 1)
	DnsAddr      netip.Addr
	Scan         ScanOptions // No longer optional - scanning is always enabled
}

// Backend represents a single wiresocks instance with its endpoint.
type Backend struct {
	endpoint  string
	port      int
	ws        *wiresocks.WireSocks
	ctx       context.Context
	cancel    context.CancelCauseFunc
	healthy   *atomic.Bool
	startedAt time.Time
}

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

// NewRotationEngine creates a new rotation engine.
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

// Run starts the rotation engine with all backends and the frontend proxy.
func (r *RotationEngine) Run() error {
	// Initialize backend pool
	if err := r.initializeBackends(); err != nil {
		return fmt.Errorf("failed to initialize backends: %w", err)
	}

	minRequired := r.getEffectiveMinBackends()
	if len(r.backends) < minRequired {
		return fmt.Errorf("not enough healthy backends: need at least %d, have %d", minRequired, len(r.backends))
	}

	log.Infow("Backend pool initialized", zap.Int("count", len(r.backends)))

	// Start backend health monitor
	go r.monitorBackends()

	// Create and start frontend proxy
	r.frontend = NewFrontendProxy(r.ctx, *r.opts.FrontendAddr, r.getNextBackend)

	log.Infow("Starting frontend SOCKS5 proxy", zap.Stringer("addr", r.opts.FrontendAddr))

	if err := r.frontend.Run(); err != nil {
		return fmt.Errorf("frontend proxy failed: %w", err)
	}

	return nil
}

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

// startBackend creates and starts a single wiresocks backend instance.
func (r *RotationEngine) startBackend(endpoint string, port int) (*Backend, error) {
	// Load identity
	ident, err := cloudflare.LoadOrCreateIdentity()
	if err != nil {
		return nil, fmt.Errorf("failed to load identity: %w", err)
	}

	// Generate WireGuard config
	conf := GenerateWireguardConfig(ident)
	conf.Interface.DNS = []netip.Addr{r.opts.DnsAddr}

	// Set endpoint for this backend
	for i, peer := range conf.Peers {
		peer.Endpoint = endpoint
		peer.KeepAlive = 5
		conf.Peers[i] = peer
	}

	// Create backend context
	ctx, cancel := context.WithCancelCause(r.ctx)

	// Create SOCKS bind address for this backend
	bindAddr := netip.MustParseAddrPort(fmt.Sprintf("127.0.0.1:%d", port))
	proxyOpts := wiresocks.ProxyConfig{
		SocksBindAddr: &bindAddr,
	}

	// Create wiresocks instance
	ws, err := wiresocks.NewWireSocks(
		wiresocks.WithContext(ctx),
		wiresocks.WithWireguardConfig(&conf),
		wiresocks.WithProxyConfig(&proxyOpts),
	)
	if err != nil {
		cancel(err)
		return nil, fmt.Errorf("failed to create wiresocks: %w", err)
	}

	backend := &Backend{
		endpoint:  endpoint,
		port:      port,
		ws:        ws,
		ctx:       ctx,
		cancel:    cancel,
		healthy:   atomic.NewBool(true),
		startedAt: time.Now(),
	}

	// Start wiresocks in background
	go func() {
		if err := ws.Run(); err != nil {
			log.Errorw("Backend wiresocks failed",
				zap.String("endpoint", endpoint),
				zap.Int("port", port),
				zap.Error(err))
			backend.healthy.Store(false)
			cancel(err)
		}
	}()

	// Wait for the backend SOCKS5 proxy to be ready (allow up to 8 seconds)
	if err := waitForBackendReady(ctx, port, 8*time.Second); err != nil {
		cancel(err)
		return nil, fmt.Errorf("backend failed to become ready: %w", err)
	}

	return backend, nil
}

// waitForBackendReady waits for the backend SOCKS5 proxy to be functional by testing actual connectivity.
func waitForBackendReady(ctx context.Context, port int, timeout time.Duration) error {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// Try a full SOCKS5 connection to 1.1.1.1:443 (Cloudflare DNS)
		if err := testSocks5Connection(addr, "1.1.1.1:443", 3*time.Second); err == nil {
			return nil // Backend is fully functional
		}

		time.Sleep(300 * time.Millisecond)
	}

	return fmt.Errorf("timeout waiting for backend on port %d", port)
}

// testSocks5Connection tests if a SOCKS5 proxy can actually route traffic.
func testSocks5Connection(proxyAddr, targetAddr string, timeout time.Duration) error {
	conn, err := net.DialTimeout("tcp", proxyAddr, timeout)
	if err != nil {
		return err
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(timeout))

	// SOCKS5 greeting: version 5, 1 auth method, no auth
	if _, err := conn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		return err
	}

	// Read greeting response
	resp := make([]byte, 2)
	if _, err := io.ReadFull(conn, resp); err != nil {
		return err
	}
	if resp[0] != 0x05 || resp[1] != 0x00 {
		return fmt.Errorf("invalid SOCKS5 greeting response")
	}

	// Parse target address
	host, portStr, err := net.SplitHostPort(targetAddr)
	if err != nil {
		return err
	}
	portNum, _ := net.LookupPort("tcp", portStr)

	// Build SOCKS5 connect request
	// Version(1) + Cmd(1) + Reserved(1) + AddrType(1) + Addr(4 for IPv4) + Port(2)
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("invalid IP address")
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return fmt.Errorf("only IPv4 supported for test")
	}

	req := []byte{
		0x05,       // Version
		0x01,       // CONNECT command
		0x00,       // Reserved
		0x01,       // IPv4 address type
		ip4[0], ip4[1], ip4[2], ip4[3], // IP address
		byte(portNum >> 8), byte(portNum & 0xff), // Port
	}

	if _, err := conn.Write(req); err != nil {
		return err
	}

	// Read connect response (at least 10 bytes for IPv4)
	respBuf := make([]byte, 10)
	if _, err := io.ReadFull(conn, respBuf); err != nil {
		return err
	}

	if respBuf[0] != 0x05 {
		return fmt.Errorf("invalid SOCKS5 version in response")
	}
	if respBuf[1] != 0x00 {
		return fmt.Errorf("SOCKS5 connect failed with code %d", respBuf[1])
	}

	// Success - we were able to connect through the tunnel
	return nil
}

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

// getEffectiveMinBackends returns the minimum required backends, defaulting to MinHealthyBackends constant.
func (r *RotationEngine) getEffectiveMinBackends() int {
	if r.opts.MinBackends < 1 {
		return MinHealthyBackends
	}
	return r.opts.MinBackends
}

// hasMinimumBackendsLocked checks minimum backends without acquiring lock (caller must hold lock).
func (r *RotationEngine) hasMinimumBackendsLocked() bool {
	minRequired := r.getEffectiveMinBackends()
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

// hasMinimumBackends returns true if we have at least MinBackends healthy backends.
func (r *RotationEngine) hasMinimumBackends() bool {
	r.poolMu.RLock()
	defer r.poolMu.RUnlock()
	return r.hasMinimumBackendsLocked()
}

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

// checkAndReplaceUnhealthyBackends removes failed backends and attempts to replace them.
func (r *RotationEngine) checkAndReplaceUnhealthyBackends() {
	r.poolMu.Lock()
	defer r.poolMu.Unlock()

	// Find and remove unhealthy backends
	var healthyBackends []*Backend
	var availablePorts []int

	for _, backend := range r.backends {
		select {
		case <-backend.ctx.Done():
			// Backend context is done, it failed
			log.Warnw("Backend failed, removing from pool",
				zap.String("endpoint", backend.endpoint),
				zap.Int("port", backend.port))
			availablePorts = append(availablePorts, backend.port)
		default:
			if backend.healthy.Load() {
				healthyBackends = append(healthyBackends, backend)
			} else {
				log.Warnw("Backend marked unhealthy, removing from pool",
					zap.String("endpoint", backend.endpoint),
					zap.Int("port", backend.port))
				availablePorts = append(availablePorts, backend.port)
				backend.cancel(errors.New("marked unhealthy"))
			}
		}
	}

	r.backends = healthyBackends

	// Warn if below minimum
	if !r.hasMinimumBackendsLocked() {
		log.Errorw("CRITICAL: Below minimum healthy backends",
			zap.Int("healthy", len(healthyBackends)),
			zap.Int("minimum_required", r.getEffectiveMinBackends()))
	}

	// Try to replace failed backends
	if len(availablePorts) > 0 {
		go r.replaceBackends(availablePorts)
	}
}

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

// getScannerEndpoints runs the IP scanner to get fresh endpoints.
func (r *RotationEngine) getScannerEndpoints() ([]string, error) {
	ident, err := cloudflare.LoadOrCreateIdentity()
	if err != nil {
		return nil, fmt.Errorf("failed to load identity: %w", err)
	}

	r.opts.Scan.PrivateKey = ident.PrivateKey
	r.opts.Scan.PublicKey = ident.Config.Peers[0].PublicKey

	res, err := RunScan(r.ctx, r.opts.Scan)
	if err != nil {
		return nil, err
	}

	endpoints := make([]string, len(res))
	for i, ipInfo := range res {
		endpoints[i] = ipInfo.AddrPort.String()
	}

	log.Infow("Scanner found endpoints", zap.Int("count", len(endpoints)))
	return endpoints, nil
}
