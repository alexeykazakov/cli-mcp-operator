package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/codeready-toolchain/cli-mcp-operator/pkg/proxy"
	"github.com/codeready-toolchain/cli-mcp-operator/pkg/version"
)

const shutdownTimeout = 15 * time.Second

func main() {
	fmt.Fprintf(os.Stderr, "cli-mcp-proxy %s (built %s)\n", version.Commit, version.BuildTime)
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "cli-mcp-proxy: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("cli-mcp-proxy", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", "", "path to proxy route JSON")
	caCertPath := fs.String("ca-cert", "", "path to MITM CA certificate PEM")
	caKeyPath := fs.String("ca-key", "", "path to MITM CA private key PEM")
	listen := fs.String("listen", ":8080", "listen address")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *configPath == "" || *caCertPath == "" || *caKeyPath == "" {
		return errors.New("--config, --ca-cert, and --ca-key are required")
	}
	return serve(*configPath, *caCertPath, *caKeyPath, *listen)
}

func serve(configPath, caCertPath, caKeyPath, listen string) error {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg, err := proxy.LoadConfig(configPath)
	if err != nil {
		return err
	}
	caCert, err := os.ReadFile(caCertPath)
	if err != nil {
		return fmt.Errorf("read CA cert: %w", err)
	}
	caKey, err := os.ReadFile(caKeyPath)
	if err != nil {
		return fmt.Errorf("read CA key: %w", err)
	}
	proxySrv, err := proxy.NewServer(cfg, caCert, caKey, logger)
	if err != nil {
		return err
	}
	httpSrv := &http.Server{
		Addr:              listen,
		Handler:           proxySrv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	errCh := make(chan error, 1)
	go func() {
		logger.Info("starting proxy", "addr", listen)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case sig := <-sigCh:
		logger.Info("received signal, shutting down", "signal", sig.String())
	case err := <-errCh:
		if err != nil {
			return err
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	proxySrv.CloseIdleConnections()
	if err := httpSrv.Shutdown(ctx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	logger.Info("proxy server stopped")
	return nil
}
