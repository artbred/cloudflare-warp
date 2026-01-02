package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/shahradelahi/wiresocks"
	"go.uber.org/atomic"
	"go.uber.org/zap"
	"golang.org/x/net/proxy"

	"github.com/artbred/cloudflare-warp/cloudflare"
	"github.com/artbred/cloudflare-warp/core/datadir"
	"github.com/artbred/cloudflare-warp/log"
)

const (
	// BackendPortStart is the starting port for backend wiresocks instances
	BackendPortStart = 40000
	// MinHealthyBackends is the minimum number of healthy backends required to operate
	MinHealthyBackends = 1
)

// getBackendIdentityDir returns the identity directory for a specific backend.
func getBackendIdentityDir(port int) string {
	return filepath.Join(datadir.GetDataDir(), fmt.Sprintf("backend-%d", port))
}

// RotationConfig holds the configuration for the rotation engine.
type RotationConfig struct {
	FrontendAddr     *netip.AddrPort
	PoolSize         int
	MinBackends      int           // Minimum healthy backends required (default: 1)
	DnsAddr          netip.Addr
	Endpoint         string        // WARP endpoint (default: engage.cloudflareclient.com:2408)
	RotationInterval time.Duration // How often to rotate backends (default: 5 minutes)
}

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

	// Start backend rotation
	go r.rotateBackends()

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

	// Wait for the backend SOCKS5 proxy to be ready (allow up to 30 seconds for WireGuard tunnel)
	if err := waitForBackendReady(ctx, port, 30*time.Second); err != nil {
		cancel(err)
		return nil, fmt.Errorf("backend failed to become ready: %w", err)
	}

	return backend, nil
}

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

// waitForBackendReady waits for the backend SOCKS5 proxy to be fully functional.
// This tests actual connectivity through the tunnel, not just that SOCKS5 is listening.
func waitForBackendReady(ctx context.Context, port int, timeout time.Duration) error {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	deadline := time.Now().Add(timeout)
	startTime := time.Now()

	var lastErr error
	attempt := 0
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		attempt++
		// Test full SOCKS5 connection to verify tunnel is routing traffic
		if err := testSocks5Connection(addr, "1.1.1.1:443", 5*time.Second); err == nil {
			elapsed := time.Since(startTime).Round(time.Millisecond)
			log.Debugw("Backend tunnel established",
				zap.Int("port", port),
				zap.Duration("elapsed", elapsed),
				zap.Int("attempts", attempt))
			return nil // Backend is fully functional
		} else {
			lastErr = err
			if attempt%5 == 0 { // Log every 5 attempts (~2.5s)
				log.Debugw("Waiting for backend tunnel",
					zap.Int("port", port),
					zap.Duration("elapsed", time.Since(startTime).Round(time.Second)),
					zap.Error(err))
			}
		}

		time.Sleep(500 * time.Millisecond)
	}

	return fmt.Errorf("timeout waiting for backend on port %d: %v", port, lastErr)
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
			log.Debugw("Selected backend for connection",
				zap.Int("port", backend.port),
				zap.String("exit_ip", backend.exitIP.String()))
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

// monitorBackends periodically checks backend health and replaces failed ones.
func (r *RotationEngine) monitorBackends() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.ctx.Done():
			return
		case <-ticker.C:
			// Only check for crashed backends (context done), not active connectivity
			// Proactive health checks were too aggressive and killed working backends
			r.checkAndReplaceUnhealthyBackends()
		}
	}
}

// checkAndReplaceUnhealthyBackends removes failed backends and attempts to replace them.
func (r *RotationEngine) checkAndReplaceUnhealthyBackends() {
	r.poolMu.Lock()
	defer r.poolMu.Unlock()

	// Find and remove crashed backends
	var healthyBackends []*Backend
	var availablePorts []int

	for _, backend := range r.backends {
		select {
		case <-backend.ctx.Done():
			// Backend context is done, it crashed
			log.Warnw("Backend crashed, removing from pool",
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

// replaceBackends attempts to start new backends on the given ports.
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
		select {
		case <-r.ctx.Done():
			return
		default:
		}

		backend, err := r.startBackend(r.opts.Endpoint, port)
		if err != nil {
			log.Warnw("Failed to start replacement backend",
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
	}
}

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

