# Unique WARP Identities Per Backend Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Give each backend its own unique WARP identity so Cloudflare assigns different exit IPs.

**Architecture:** Instead of all backends sharing one identity (one public key), create N separate identities stored in separate directories. Each backend registers independently with Cloudflare using its own private/public key pair. Cloudflare sees different public keys → assigns different exit IPs.

**Tech Stack:** Go, WireGuard keys, Cloudflare WARP API

---

## Task 1: Add Identity Storage Per Backend

**Files:**
- Modify: `core/rotation.go`
- Modify: `cloudflare/identity.go`

**Step 1: Update LoadOrCreateIdentity to accept a path parameter**

In `cloudflare/identity.go`, modify the function to accept an optional identity directory:

```go
// LoadOrCreateIdentity loads an existing identity or creates a new one.
// If identityDir is empty, uses the default data directory.
func LoadOrCreateIdentity(identityDir string) (*model.Identity, error) {
	if identityDir == "" {
		identityDir = datadir.Dir()
	}

	identity, err := LoadIdentity(identityDir)
	if err != nil {
		log.Warnw("Failed to load existing WARP identity; attempting to create a new one", zap.Error(err))
		log.Info("Initiating creation of a new WARP identity...")
		identity, err = CreateOrUpdateIdentity(identityDir, "")
		if err != nil {
			return nil, err
		}
	}
	return identity, nil
}
```

**Step 2: Update LoadIdentity to use provided path**

```go
// LoadIdentity loads identity from specified directory
func LoadIdentity(identityDir string) (*model.Identity, error) {
	regPath := filepath.Join(identityDir, "reg.json")
	confPath := filepath.Join(identityDir, "conf.json")

	regData, err := os.ReadFile(regPath)
	if err != nil {
		return nil, err
	}
	// ... rest of loading logic
}
```

**Step 3: Update CreateOrUpdateIdentity to use provided path**

```go
func CreateOrUpdateIdentity(identityDir string, licenseKey string) (*model.Identity, error) {
	if identityDir == "" {
		identityDir = datadir.Dir()
	}

	// Ensure directory exists
	if err := os.MkdirAll(identityDir, 0700); err != nil {
		return nil, fmt.Errorf("failed to create identity directory: %w", err)
	}

	regPath := filepath.Join(identityDir, "reg.json")
	confPath := filepath.Join(identityDir, "conf.json")
	// ... rest uses these paths
}
```

**Step 4: Run build to verify syntax**

Run: `go build ./...`
Expected: Build errors (callers need updating)

**Step 5: Commit**

```bash
git add cloudflare/identity.go
git commit -m "refactor(identity): add directory parameter to identity functions"
```

---

## Task 2: Create Per-Backend Identity Directories

**Files:**
- Modify: `core/rotation.go`

**Step 1: Add function to get backend identity directory**

Add this helper function:

```go
// getBackendIdentityDir returns the identity directory for a specific backend.
func getBackendIdentityDir(port int) string {
	return filepath.Join(datadir.Dir(), fmt.Sprintf("backend-%d", port))
}
```

**Step 2: Add filepath import if not present**

```go
import (
	"path/filepath"
)
```

**Step 3: Run build to verify syntax**

Run: `go build ./...`
Expected: PASS

**Step 4: Commit**

```bash
git add core/rotation.go
git commit -m "feat(rotation): add getBackendIdentityDir helper"
```

---

## Task 3: Update startBackend to Use Per-Backend Identity

**Files:**
- Modify: `core/rotation.go` (startBackend function)

**Step 1: Update startBackend to create unique identity per backend**

Replace the identity loading in startBackend:

```go
func (r *RotationEngine) startBackend(endpoint string, port int) (*Backend, error) {
	// Create unique identity directory for this backend
	identityDir := getBackendIdentityDir(port)

	// Load or create identity specific to this backend
	ident, err := cloudflare.LoadOrCreateIdentity(identityDir)
	if err != nil {
		return nil, fmt.Errorf("failed to load identity for backend %d: %w", port, err)
	}

	log.Infow("Using identity for backend",
		zap.Int("port", port),
		zap.String("identity_dir", identityDir))

	// Rest of function remains the same...
	wgConf := GenerateWireguardConfig(ident)
	// ...
}
```

**Step 2: Run build to verify syntax**

Run: `go build ./...`
Expected: PASS

**Step 3: Commit**

```bash
git add core/rotation.go
git commit -m "feat(rotation): use unique identity per backend"
```

---

## Task 4: Remove Redundant Unique IP Retry Logic

**Files:**
- Modify: `core/rotation.go`

**Step 1: Simplify startBackendWithUniqueIP**

Since each backend now has its own identity, Cloudflare will naturally assign different IPs. Simplify the function to just query and store the IP without retry logic:

```go
// startBackendWithUniqueIP starts a backend and queries its exit IP.
// With unique identities per backend, Cloudflare assigns unique IPs naturally.
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

	// Log if duplicate (informational only - shouldn't happen with unique identities)
	if usedIPs[exitIP] {
		log.Warnw("Backend has duplicate exit IP despite unique identity",
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
git commit -m "refactor(rotation): simplify unique IP logic with per-backend identities"
```

---

## Task 5: Update Callers of LoadOrCreateIdentity

**Files:**
- Modify: `cmd/run.go` or wherever else `LoadOrCreateIdentity` is called

**Step 1: Find and update all callers**

Search for usages:
```bash
grep -r "LoadOrCreateIdentity" --include="*.go"
```

For each caller that uses the default behavior, pass empty string:

```go
// Before:
ident, err := cloudflare.LoadOrCreateIdentity()

// After:
ident, err := cloudflare.LoadOrCreateIdentity("")
```

**Step 2: Run build to verify all callers updated**

Run: `go build ./...`
Expected: PASS

**Step 3: Commit**

```bash
git add .
git commit -m "refactor: update LoadOrCreateIdentity callers to use new signature"
```

---

## Task 6: Clean Up Old Backend Identities on Shutdown

**Files:**
- Modify: `core/rotation.go`

**Step 1: Add cleanup function**

```go
// cleanupBackendIdentities removes identity directories for backends that are no longer running.
// Called during graceful shutdown or when cleaning up old backends.
func cleanupBackendIdentity(port int) {
	identityDir := getBackendIdentityDir(port)
	if err := os.RemoveAll(identityDir); err != nil {
		log.Warnw("Failed to cleanup backend identity directory",
			zap.Int("port", port),
			zap.String("dir", identityDir),
			zap.Error(err))
	} else {
		log.Debugw("Cleaned up backend identity directory",
			zap.Int("port", port))
	}
}
```

**Step 2: Call cleanup when backend is cancelled**

In `rotateOneBackend`, after cancelling old backend:

```go
// Cancel old backend
oldBackend.cancel(nil)
// Clean up old backend's identity
go cleanupBackendIdentity(oldBackend.port)
```

**Step 3: Run build to verify syntax**

Run: `go build ./...`
Expected: PASS

**Step 4: Commit**

```bash
git add core/rotation.go
git commit -m "feat(rotation): clean up backend identity directories on rotation"
```

---

## Task 7: Build and Integration Test

**Step 1: Build the binary**

Run: `go build ./...`
Expected: PASS

**Step 2: Run unit tests**

Run: `go test ./core/... -v`
Expected: PASS

**Step 3: Clean existing identities**

Before testing, remove old backend directories:
```bash
rm -rf ~/.local/share/cloudflare-warp/backend-*
```

**Step 4: Start rotation with 3 backends**

Run: `./cloudflare-warp rotate --socks-addr 127.0.0.1:1080 --pool-size 3`

Expected logs:
- "Using identity for backend" x3 with different identity_dir paths
- "Initiating creation of a new WARP identity..." x3 (first run only)
- "Backend exit IP detected" x3 with **DIFFERENT** IPs

**Step 5: Verify unique IPs**

Run multiple requests:
```bash
for i in {1..15}; do curl -x socks5://127.0.0.1:1080 ifconfig.me 2>/dev/null; echo; done
```

Expected: See 3 different IPs rotating (approximately 5 requests per IP with round-robin)

**Step 6: Verify identity directories created**

```bash
ls -la ~/.local/share/cloudflare-warp/backend-*
```

Expected: 3 directories (backend-40000, backend-40001, backend-40002) each with reg.json and conf.json

**Step 7: Commit final state**

```bash
git add .
git commit -m "feat(rotation): complete unique identity per backend implementation"
```

---

## Summary

This implementation:
1. Creates a **unique WARP identity** (unique private/public key) for each backend
2. Each identity is stored in its own directory (`backend-{port}/`)
3. Each identity registers **independently** with Cloudflare
4. Cloudflare sees different public keys → assigns **different exit IPs**
5. Removes the reactive "wait 5 seconds and retry" hack since it's no longer needed
6. Cleans up old identity directories during rotation

**Why this works:** Cloudflare assigns exit IPs based on the WireGuard public key. Different keys = different identities = different IPs.
