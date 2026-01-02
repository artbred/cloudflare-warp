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

	"github.com/shahradelahi/cloudflare-warp/core"
	"github.com/shahradelahi/cloudflare-warp/core/cache"
	"github.com/shahradelahi/cloudflare-warp/log"
)

var RotateCmd = &cobra.Command{
	Use:   "rotate",
	Short: "Run WARP proxy with IP rotation",
	Long: `Run the Cloudflare WARP proxy with IP rotation.
This command starts a SOCKS5 proxy that rotates through multiple WARP endpoints,
giving each new connection a different exit IP address.

The rotation pool is populated from cached endpoints (run 'warp scanner' first to populate the cache).
Alternatively, use --scan to discover endpoints on startup.`,
	Run: rotate,
}

func init() {
	RotateCmd.Flags().String("socks-addr", "", "Socks5 proxy bind address (required, e.g., 127.0.0.1:1080).")
	RotateCmd.Flags().Int("pool-size", 10, "Number of endpoints in the rotation pool (default: 10).")
	RotateCmd.Flags().Int("min-backends", 1, "Minimum healthy backends required to operate (default: 1).")
	RotateCmd.Flags().String("dns", "1.1.1.1", "DNS server address to use.")
	RotateCmd.Flags().Bool("scan", false, "Scan for endpoints on startup if cache is insufficient.")
	RotateCmd.Flags().Duration("scan-rtt", 1000*time.Millisecond, "Scanner RTT limit for endpoint selection (e.g., 1000ms).")
	RotateCmd.Flags().Bool("4", false, "Use IPv4 for endpoint selection (scanner mode).")
	RotateCmd.Flags().Bool("6", false, "Use IPv6 for endpoint selection (scanner mode).")

	viper.BindPFlag("rotate-socks-addr", RotateCmd.Flags().Lookup("socks-addr"))
	viper.BindPFlag("rotate-pool-size", RotateCmd.Flags().Lookup("pool-size"))
	viper.BindPFlag("rotate-min-backends", RotateCmd.Flags().Lookup("min-backends"))
	viper.BindPFlag("rotate-dns", RotateCmd.Flags().Lookup("dns"))
	viper.BindPFlag("rotate-scan", RotateCmd.Flags().Lookup("scan"))
	viper.BindPFlag("rotate-scan-rtt", RotateCmd.Flags().Lookup("scan-rtt"))
	viper.BindPFlag("rotate-4", RotateCmd.Flags().Lookup("4"))
	viper.BindPFlag("rotate-6", RotateCmd.Flags().Lookup("6"))
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

	// Build config
	opts := core.RotationConfig{
		FrontendAddr: &socksAddr,
		PoolSize:     poolSize,
		MinBackends:  minBackends,
		DnsAddr:      dnsAddr,
	}

	// Check cache for available endpoints
	c := cache.NewCache()
	availableEndpoints := c.GetAllEndpoints()

	if len(availableEndpoints) < poolSize {
		if viper.GetBool("rotate-scan") {
			log.Infow("Cache has insufficient endpoints, scanner mode enabled",
				zap.Int("available", len(availableEndpoints)),
				zap.Int("needed", poolSize))
			opts.Scan = &core.ScanOptions{
				V4:     useV4,
				V6:     useV6,
				MaxRTT: viper.GetDuration("rotate-scan-rtt"),
			}
		} else {
			log.Warnw("Cache has insufficient endpoints for requested pool size",
				zap.Int("available", len(availableEndpoints)),
				zap.Int("needed", poolSize))
			log.Info("Run 'warp scanner' first to populate the cache, or use --scan flag")

			if len(availableEndpoints) == 0 {
				rotateFatal(errors.New("no cached endpoints available; run 'warp scanner' first or use --scan"))
			}

			// Adjust pool size to available endpoints
			opts.PoolSize = len(availableEndpoints)
			log.Infow("Adjusted pool size to available endpoints", zap.Int("pool-size", opts.PoolSize))
		}
	}

	log.Infow("Starting rotation proxy",
		zap.String("socks-addr", socksAddr.String()),
		zap.Int("pool-size", opts.PoolSize))

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
