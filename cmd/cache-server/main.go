package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/cre4ture/bazel-github-actions-cache-v2/internal/cache"
	cacheserver "github.com/cre4ture/bazel-github-actions-cache-v2/internal/server"
)

var version = "dev"

type readyInfo struct {
	URL      string `json:"url"`
	StatsURL string `json:"stats_url"`
	PID      int    `json:"pid"`
	Version  string `json:"version"`
}

func main() {
	if err := run(); err != nil {
		log.Printf("fatal: %v", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		port             = flag.Int("port", 0, "loopback TCP port; zero selects a dynamic port")
		cacheDir         = flag.String("cache-dir", "", "local spool directory")
		keyPrefix        = flag.String("key-prefix", "bazel-http-v1", "GitHub cache key prefix")
		writeEnabled     = flag.Bool("write-enabled", false, "publish validated uploads")
		failOpen         = flag.Bool("fail-open", true, "degrade backend errors to misses/success")
		maxBlobSize      = flag.Int64("max-blob-size", 512*1024*1024, "maximum object size in bytes")
		maxConcurrent    = flag.Int("max-concurrent", 4, "maximum concurrent backend operations")
		uploadsPerMinute = flag.Int("uploads-per-minute", 180, "maximum GitHub cache uploads per minute")
		backendTimeout   = flag.Duration("backend-timeout", 5*time.Minute, "timeout per backend operation")
		readyFile        = flag.String("ready-file", "", "write startup metadata to this file")
		statsFile        = flag.String("stats-file", "", "write final statistics to this file")
		showVersion      = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return nil
	}
	if *readyFile == "" {
		return fmt.Errorf("--ready-file is required")
	}

	dir, err := cacheserver.SafeCacheDir(*cacheDir)
	if err != nil {
		return fmt.Errorf("prepare spool directory: %w", err)
	}
	defer os.RemoveAll(dir)

	backend, err := cache.NewActionsBackend(*backendTimeout)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(*port)))
	if err != nil {
		return fmt.Errorf("listen on loopback: %w", err)
	}
	defer listener.Close()
	address := listener.Addr().(*net.TCPAddr)
	baseURL := "http://127.0.0.1:" + strconv.Itoa(address.Port)

	shutdownRequested := make(chan struct{}, 1)
	requestShutdown := func() {
		select {
		case shutdownRequested <- struct{}{}:
		default:
		}
	}
	logger := log.New(os.Stderr, "bazel-gha-cache: ", log.LstdFlags|log.LUTC)
	srv, err := cacheserver.New(cacheserver.Config{
		Backend:          backend,
		CacheDir:         dir,
		KeyPrefix:        *keyPrefix,
		WriteEnabled:     *writeEnabled,
		FailOpen:         *failOpen,
		MaxBlobSize:      *maxBlobSize,
		MaxConcurrent:    *maxConcurrent,
		UploadsPerMinute: *uploadsPerMinute,
		BackendTimeout:   *backendTimeout,
		ShutdownToken:    os.Getenv("BAZEL_GHA_CACHE_SHUTDOWN_TOKEN"),
		Shutdown:         requestShutdown,
		Logger:           logger,
	})
	if err != nil {
		return err
	}
	_ = os.Unsetenv("BAZEL_GHA_CACHE_SHUTDOWN_TOKEN")

	httpServer := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       10 * time.Minute,
		WriteTimeout:      10 * time.Minute,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    16 * 1024,
	}
	serveErr := make(chan error, 1)
	go func() {
		err := httpServer.Serve(listener)
		if err != nil && err != http.ErrServerClosed {
			serveErr <- err
		}
		close(serveErr)
	}()

	ready := readyInfo{
		URL:      baseURL,
		StatsURL: baseURL + "/stats",
		PID:      os.Getpid(),
		Version:  version,
	}
	data, err := json.Marshal(ready)
	if err != nil {
		return err
	}
	if err := os.WriteFile(*readyFile, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write ready file: %w", err)
	}
	logger.Printf("ready on %s (write=%t, fail_open=%t)", baseURL, *writeEnabled, *failOpen)

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	select {
	case sig := <-signals:
		logger.Printf("received signal %s", sig)
	case <-shutdownRequested:
		logger.Printf("shutdown requested")
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("serve: %w", err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(ctx); err != nil {
		logger.Printf("graceful shutdown failed: %v", err)
		_ = httpServer.Close()
	}
	stats := srv.Snapshot()
	if err := cacheserver.WriteStatsFile(*statsFile, stats); err != nil {
		logger.Printf("write stats: %v", err)
	}
	logger.Printf("stopped: %s", strings.TrimSpace(string(stats.JSON())))
	return nil
}
