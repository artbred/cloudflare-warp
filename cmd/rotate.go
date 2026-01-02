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
	Short: "Run WARP proxy with IP rotation",
	Long: `Run the Cloudflare WARP proxy with IP rotation.
This command starts a SOCKS5 proxy that rotates through multiple WARP backends,
giving each new connection a different exit IP address.

Backends connect to engage.cloudflareclient.com:2408 and are periodically
rotated in the background to refresh connections.`,
	Run: rotate,
}

func init() {
	RotateCmd.Flags().String("socks-addr", "", "Socks5 proxy bind address (required, e.g., 127.0.0.1:1080).")
	RotateCmd.Flags().Int("pool-size", 10, "Number of backends in the rotation pool (default: 10).")
	RotateCmd.Flags().Int("min-backends", 1, "Minimum healthy backends required to operate (default: 1).")
	RotateCmd.Flags().String("dns", "1.1.1.1", "DNS server address to use.")
	RotateCmd.Flags().String("endpoint", "", "WARP endpoint (default: engage.cloudflareclient.com:2408).")
	RotateCmd.Flags().Duration("rotation-interval", 5*time.Minute, "How often to rotate backends (default: 5m).")

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

	// Build config
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
		zap.Int("pool-size", opts.PoolSize),
		zap.Int("min-backends", opts.MinBackends),
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
