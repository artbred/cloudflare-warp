package core

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/shahradelahi/cloudflare-warp/log"
)

const (
	// Connection timeouts
	dialTimeout     = 5 * time.Second
	handshakeTimout = 10 * time.Second

	// Max retries for backend connection
	maxBackendRetries = 3
)

// FrontendProxy is a SOCKS5 proxy that distributes connections across backend pools.
type FrontendProxy struct {
	bindAddr       netip.AddrPort
	getNextBackend func() *Backend
	listener       net.Listener
	ctx            context.Context
	cancel         context.CancelFunc
	wg             sync.WaitGroup
}

// NewFrontendProxy creates a new frontend proxy.
func NewFrontendProxy(ctx context.Context, bindAddr netip.AddrPort, getNextBackend func() *Backend) *FrontendProxy {
	ctx, cancel := context.WithCancel(ctx)
	return &FrontendProxy{
		bindAddr:       bindAddr,
		getNextBackend: getNextBackend,
		ctx:            ctx,
		cancel:         cancel,
	}
}

// Run starts the frontend proxy and blocks until the context is cancelled.
func (f *FrontendProxy) Run() error {
	listener, err := net.Listen("tcp", f.bindAddr.String())
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", f.bindAddr.String(), err)
	}
	f.listener = listener

	log.Infow("Frontend proxy listening", zap.String("addr", f.bindAddr.String()))

	// Accept connections in a loop
	go f.acceptLoop()

	// Wait for context cancellation
	<-f.ctx.Done()

	// Close listener to stop accepting new connections
	f.listener.Close()

	// Wait for active connections to finish (with timeout)
	done := make(chan struct{})
	go func() {
		f.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		log.Info("All frontend connections closed gracefully")
	case <-time.After(30 * time.Second):
		log.Warn("Timeout waiting for frontend connections to close")
	}

	return nil
}

// acceptLoop accepts incoming connections and handles them.
func (f *FrontendProxy) acceptLoop() {
	for {
		conn, err := f.listener.Accept()
		if err != nil {
			select {
			case <-f.ctx.Done():
				return
			default:
				log.Errorw("Failed to accept connection", zap.Error(err))
				continue
			}
		}

		f.wg.Add(1)
		go func() {
			defer f.wg.Done()
			f.handleConnection(conn)
		}()
	}
}

// handleConnection handles a single client connection by proxying it to a backend.
func (f *FrontendProxy) handleConnection(clientConn net.Conn) {
	defer clientConn.Close()

	// Set deadline for initial handshake
	clientConn.SetDeadline(time.Now().Add(handshakeTimout))

	// Try multiple backends if needed
	for attempt := 0; attempt < maxBackendRetries; attempt++ {
		backend := f.getNextBackend()
		if backend == nil {
			log.Errorw("No backends available")
			return
		}

		log.Debugw("Routing connection to backend",
			zap.String("endpoint", backend.endpoint),
			zap.Int("port", backend.port),
			zap.Int("attempt", attempt+1))

		err := f.proxyToBackend(clientConn, backend)
		if err == nil {
			return // Success
		}

		// Check if it's a context cancellation (shutdown)
		select {
		case <-f.ctx.Done():
			return
		default:
		}

		log.Warnw("Backend connection failed, trying next",
			zap.String("endpoint", backend.endpoint),
			zap.Int("port", backend.port),
			zap.Int("attempt", attempt+1),
			zap.Error(err))

		// Mark backend as potentially unhealthy after repeated failures
		if attempt >= 1 {
			backend.healthy.Store(false)
		}
	}

	log.Warnw("All backend connection attempts failed")
}

// proxyToBackend establishes a connection to the backend and proxies data bidirectionally.
func (f *FrontendProxy) proxyToBackend(clientConn net.Conn, backend *Backend) error {
	backendAddr := fmt.Sprintf("127.0.0.1:%d", backend.port)

	// Connect to backend SOCKS5 proxy
	backendConn, err := net.DialTimeout("tcp", backendAddr, dialTimeout)
	if err != nil {
		return fmt.Errorf("failed to dial backend %s: %w", backendAddr, err)
	}
	defer backendConn.Close()

	// Clear the deadline after successful connection
	clientConn.SetDeadline(time.Time{})

	// Bidirectional copy
	errChan := make(chan error, 2)

	go func() {
		_, err := io.Copy(backendConn, clientConn)
		errChan <- err
	}()

	go func() {
		_, err := io.Copy(clientConn, backendConn)
		errChan <- err
	}()

	// Wait for either direction to complete or context cancellation
	select {
	case <-f.ctx.Done():
		return f.ctx.Err()
	case err := <-errChan:
		// One direction finished, return (the other will finish when connection closes)
		return err
	}
}

// Stop stops the frontend proxy.
func (f *FrontendProxy) Stop() {
	f.cancel()
	if f.listener != nil {
		f.listener.Close()
	}
}
