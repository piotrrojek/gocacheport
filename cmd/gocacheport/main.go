package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/piotrrojek/gocacheport/internal/config"
	"github.com/piotrrojek/gocacheport/internal/prog"
	"github.com/piotrrojek/gocacheport/internal/server"
	"github.com/piotrrojek/gocacheport/internal/store"
	"github.com/piotrrojek/gocacheport/internal/telemetry"
)

var (
	version = "devel"
	commit  = "unknown"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil && !errors.Is(err, flag.ErrHelp) {
		slog.Error("gocacheport failed", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintln(os.Stderr, "Usage: gocacheport <serve|prog|version> [flags]\n\nserve: filesystem-backed HTTP cache\nprog: Go GOCACHEPROG helper\nversion: print build information")
		return nil
	}
	switch args[0] {
	case "serve":
		return serve(ctx, args[1:])
	case "prog":
		return cacheProgram(ctx, args[1:])
	case "version":
		fmt.Printf("gocacheport %s (%s) %s %s/%s\n", version, commit, runtime.Version(), runtime.GOOS, runtime.GOARCH)
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

type commonFlags struct {
	tokenFile   string
	logLevel    string
	tracing     bool
	sampleRatio float64
}

func (common *commonFlags) register(flags *flag.FlagSet) {
	flags.StringVar(&common.tokenFile, "token-file", "", "bearer token file (otherwise GOCACHEPORT_TOKEN)")
	flags.StringVar(&common.logLevel, "log-level", "info", "slog level: debug, info, warn, error")
	flags.BoolVar(&common.tracing, "tracing", false, "export OTLP/HTTP traces using OTEL_EXPORTER_OTLP_* settings")
	flags.Float64Var(&common.sampleRatio, "trace-sample-ratio", 0.1, "root trace sampling ratio, from zero to one")
}

func (common commonFlags) initialize(ctx context.Context, service string) (*slog.Logger, string, func(), error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(common.logLevel)); err != nil {
		return nil, "", nil, errors.New("invalid log level")
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level})).With("service", service)
	slog.SetDefault(logger)
	token, err := config.Token(common.tokenFile)
	if err != nil {
		return nil, "", nil, err
	}
	shutdown, err := telemetry.Setup(ctx, service, version, common.tracing, common.sampleRatio)
	if err != nil {
		return nil, "", nil, fmt.Errorf("configure tracing: %w", err)
	}
	cleanup := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdown(ctx); err != nil {
			logger.Warn("trace exporter shutdown failed", "error", err)
		}
	}
	return logger, token, cleanup, nil
}

func serve(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	var storage store.Config
	flags.StringVar(&storage.Directory, "dir", "", "required cache directory, owned by this server")
	flags.Int64Var(&storage.DiskBytes, "disk-bytes", 10<<30, "maximum committed record bytes")
	flags.IntVar(&storage.MaxEntries, "max-entries", 100000, "maximum committed records and inventory entries")
	flags.Int64Var(&storage.MaxObjectBytes, "max-object-bytes", 128<<20, "maximum artifact payload bytes")
	flags.Int64Var(&storage.MemoryBytes, "memory-bytes", 128<<20, "memory-tier byte budget; zero disables it")
	flags.Int64Var(&storage.MemoryObjectBytes, "memory-object-bytes", 1<<20, "largest artifact retained in memory")
	flags.DurationVar(&storage.TTL, "ttl", 0, "artifact TTL since creation; zero disables expiry")
	listen := flags.String("listen", ":8080", "artifact HTTP listen address")
	adminListen := flags.String("admin-listen", ":9090", "health and Prometheus listen address")
	maxConcurrent := flags.Int("max-concurrent", 32, "maximum concurrent artifact requests")
	requestTimeout := flags.Duration("request-timeout", 2*time.Minute, "HTTP read/write timeout")
	shutdownTimeout := flags.Duration("shutdown-timeout", 30*time.Second, "graceful shutdown deadline")
	profiling := flags.Bool("pprof", false, "enable pprof on the administrative listener")
	var common commonFlags
	common.register(flags)
	if err := config.Parse(flags, args); err != nil {
		return err
	}
	if *requestTimeout <= 0 || *shutdownTimeout <= 0 {
		return errors.New("HTTP and shutdown timeouts must be positive")
	}
	logger, token, cleanup, err := common.initialize(ctx, "gocacheport")
	if err != nil {
		return err
	}
	defer cleanup()
	cache, err := store.Open(storage)
	if err != nil {
		return err
	}
	defer cache.Close()
	service, err := server.New(cache, server.Config{
		Token: token, MaxConcurrent: *maxConcurrent, MaxObjectBytes: storage.MaxObjectBytes,
		Pprof: *profiling, Logger: logger, Version: version,
	})
	if err != nil {
		return err
	}
	dataListener, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	defer dataListener.Close()
	adminListener, err := net.Listen("tcp", *adminListen)
	if err != nil {
		return err
	}
	defer adminListener.Close()
	newHTTP := func(handler http.Handler) *http.Server {
		return &http.Server{
			Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: *requestTimeout,
			WriteTimeout: *requestTimeout, IdleTimeout: time.Minute, MaxHeaderBytes: 16 << 10,
			ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelError),
		}
	}
	dataHTTP, adminHTTP := newHTTP(service.Handler()), newHTTP(service.AdminHandler())
	failures := make(chan error, 2)
	var background sync.WaitGroup
	background.Go(func() { failures <- dataHTTP.Serve(dataListener) })
	background.Go(func() { failures <- adminHTTP.Serve(adminListener) })
	janitorContext, stopJanitor := context.WithCancel(ctx)
	background.Go(func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-janitorContext.Done():
				return
			case <-ticker.C:
				if err := cache.Sweep(); err != nil {
					logger.Warn("cache retention sweep failed", "error", err)
				}
			}
		}
	})
	logger.Info("cache server ready", "listen", dataListener.Addr().String(), "admin_listen", adminListener.Addr().String(),
		"directory", storage.Directory, "disk_bytes", storage.DiskBytes, "memory_bytes", storage.MemoryBytes,
		"authenticated", token != "", "tracing", common.tracing, "version", version)
	select {
	case <-ctx.Done():
	case err = <-failures:
	}
	service.Drain()
	stopJanitor()
	shutdownContext, cancel := context.WithTimeout(context.Background(), *shutdownTimeout)
	defer cancel()
	if shutdownErr := dataHTTP.Shutdown(shutdownContext); shutdownErr != nil {
		logger.Warn("graceful artifact shutdown timed out", "error", shutdownErr)
		dataHTTP.Close()
	}
	if shutdownErr := adminHTTP.Shutdown(shutdownContext); shutdownErr != nil {
		adminHTTP.Close()
	}
	background.Wait()
	logger.Info("cache server stopped")
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func cacheProgram(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("prog", flag.ContinueOnError)
	var settings prog.Config
	flags.StringVar(&settings.URL, "url", "http://127.0.0.1:8080", "remote cache base URL")
	flags.StringVar(&settings.Directory, "local-dir", "", "private staging parent; default creates a session directly in the OS temporary directory")
	flags.DurationVar(&settings.Timeout, "timeout", 30*time.Second, "per-request remote timeout")
	flags.DurationVar(&settings.ConnectTimeout, "connect-timeout", time.Second, "remote connection and TLS handshake timeout")
	flags.DurationVar(&settings.HeaderTimeout, "header-timeout", time.Second, "remote response header timeout")
	flags.IntVar(&settings.Workers, "workers", 16, "maximum concurrent remote requests")
	flags.Int64Var(&settings.MaxObjectBytes, "max-object-bytes", 128<<20, "maximum downloaded or remotely uploaded artifact bytes")
	flags.BoolVar(&settings.Strict, "strict", false, "report remote failures to Go instead of falling back locally")
	var common commonFlags
	common.register(flags)
	if err := config.Parse(flags, args); err != nil {
		return err
	}
	logger, token, cleanup, err := common.initialize(ctx, "gocacheport-prog")
	if err != nil {
		return err
	}
	defer cleanup()
	settings.Token, settings.Logger = token, logger
	return prog.Run(ctx, os.Stdin, os.Stdout, settings)
}
