package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/piotrrojek/gocacheport/internal/store"
)

const (
	OutputHeader   = "X-Gocacheport-Output-ID"
	TimeHeader     = "X-Gocacheport-Created"
	OverloadHeader = "X-Gocacheport-Overloaded"
)

type Config struct {
	Token          string
	MaxConcurrent  int
	MaxObjectBytes int64
	Pprof          bool
	Logger         *slog.Logger
	Version        string
}

type Server struct {
	cache    *store.Store
	config   Config
	metrics  metrics
	active   chan struct{}
	draining atomic.Bool
	data     http.Handler
	admin    http.Handler
}

func New(cache *store.Store, config Config) (*Server, error) {
	if config.MaxConcurrent < 1 || config.MaxObjectBytes < 1 || config.Logger == nil {
		return nil, errors.New("invalid HTTP service configuration")
	}
	service := &Server{cache: cache, config: config, active: make(chan struct{}, config.MaxConcurrent)}
	service.metrics = newMetrics(cache, config.Version)
	data := http.NewServeMux()
	data.HandleFunc("GET /v1/cache/{action}", service.get)
	data.HandleFunc("PUT /v1/cache/{action}", service.put)
	service.data = otelhttp.NewHandler(service.instrument(service.authorize(service.limit(data))), "cache",
		otelhttp.WithSpanNameFormatter(func(_ string, request *http.Request) string {
			return request.Method + " /v1/cache/{action}"
		}))
	admin := http.NewServeMux()
	admin.HandleFunc("GET /livez", func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
		io.WriteString(writer, "ok\n")
	})
	admin.HandleFunc("GET /readyz", func(writer http.ResponseWriter, _ *http.Request) {
		if service.draining.Load() || service.cache.Ready() != nil {
			http.Error(writer, "not ready", http.StatusServiceUnavailable)
			return
		}
		io.WriteString(writer, "ok\n")
	})
	admin.Handle("GET /metrics", service.metrics.handler)
	if config.Pprof {
		admin.HandleFunc("GET /debug/pprof/", pprof.Index)
		admin.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
		admin.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
		admin.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
		admin.HandleFunc("GET /debug/pprof/trace", pprof.Trace)
	}
	service.admin = admin
	return service, nil
}

func (service *Server) Handler() http.Handler      { return service.data }
func (service *Server) AdminHandler() http.Handler { return service.admin }
func (service *Server) Drain()                     { service.draining.Store(true) }

func (service *Server) get(writer http.ResponseWriter, request *http.Request) {
	key := request.PathValue("action")
	if !store.ValidID(key) {
		http.Error(writer, "action must be a lowercase SHA-256 ID", http.StatusBadRequest)
		return
	}
	artifact, err := service.cache.Get(key)
	if errors.Is(err, store.ErrMiss) {
		trace.SpanFromContext(request.Context()).SetAttributes(attribute.String("cache.result", "miss"))
		http.NotFound(writer, request)
		return
	}
	if err != nil {
		service.storageError(writer, err)
		return
	}
	defer artifact.Body.Close()
	trace.SpanFromContext(request.Context()).SetAttributes(attribute.String("cache.tier", artifact.Tier))
	writer.Header().Set("Content-Type", "application/octet-stream")
	writer.Header().Set("Content-Length", strconv.FormatInt(artifact.Size, 10))
	writer.Header().Set(OutputHeader, hex.EncodeToString(artifact.OutputID[:]))
	writer.Header().Set(TimeHeader, artifact.Created.Format(time.RFC3339Nano))
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	if request.Method == http.MethodHead {
		return
	}
	if _, err := io.CopyN(writer, artifact.Body, artifact.Size); err != nil {
		service.config.Logger.Warn("artifact stream interrupted", "error", err)
	}
}

func (service *Server) put(writer http.ResponseWriter, request *http.Request) {
	key := request.PathValue("action")
	output, err := hex.DecodeString(request.Header.Get(OutputHeader))
	if !store.ValidID(key) || err != nil || len(output) != sha256.Size {
		http.Error(writer, "valid action and output SHA-256 IDs are required", http.StatusBadRequest)
		return
	}
	if request.ContentLength < 0 {
		http.Error(writer, "Content-Length is required", http.StatusLengthRequired)
		return
	}
	if request.ContentLength > service.config.MaxObjectBytes {
		http.Error(writer, "artifact exceeds cache limit", http.StatusRequestEntityTooLarge)
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, service.config.MaxObjectBytes)
	_, err = service.cache.Put(request.Context(), key, [sha256.Size]byte(output), request.ContentLength, request.Body)
	if errors.Is(err, store.ErrIntegrity) {
		http.Error(writer, "artifact size or SHA-256 mismatch", http.StatusBadRequest)
		return
	}
	if errors.Is(err, store.ErrTooLarge) {
		http.Error(writer, "artifact exceeds cache limit", http.StatusRequestEntityTooLarge)
		return
	}
	if err != nil {
		service.storageError(writer, err)
		return
	}
	service.metrics.uploaded.Add(float64(request.ContentLength))
	writer.WriteHeader(http.StatusNoContent)
}

func (service *Server) storageError(writer http.ResponseWriter, err error) {
	service.config.Logger.Error("cache storage operation failed", "error", err)
	http.Error(writer, "cache storage unavailable", http.StatusInternalServerError)
}

func (service *Server) authorize(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if service.config.Token != "" {
			expected := sha256.Sum256([]byte("Bearer " + service.config.Token))
			actual := sha256.Sum256([]byte(request.Header.Get("Authorization")))
			if subtle.ConstantTimeCompare(expected[:], actual[:]) != 1 {
				writer.Header().Set("WWW-Authenticate", "Bearer")
				http.Error(writer, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		next.ServeHTTP(writer, request)
	})
}

func (service *Server) limit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if service.draining.Load() {
			http.Error(writer, "draining", http.StatusServiceUnavailable)
			return
		}
		select {
		case service.active <- struct{}{}:
			defer func() { <-service.active }()
			service.metrics.inflight.Inc()
			defer service.metrics.inflight.Dec()
			next.ServeHTTP(writer, request)
		default:
			writer.Header().Set("Retry-After", "1")
			writer.Header().Set(OverloadHeader, "true")
			http.Error(writer, "cache concurrency limit reached", http.StatusServiceUnavailable)
		}
	})
}

type responseWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (writer *responseWriter) WriteHeader(status int) {
	if writer.status != 0 {
		return
	}
	writer.status = status
	writer.ResponseWriter.WriteHeader(status)
}

func (writer *responseWriter) Write(body []byte) (int, error) {
	if writer.status == 0 {
		writer.WriteHeader(http.StatusOK)
	}
	written, err := writer.ResponseWriter.Write(body)
	writer.bytes += int64(written)
	return written, err
}

func (writer *responseWriter) Unwrap() http.ResponseWriter { return writer.ResponseWriter }

func (service *Server) instrument(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		started := time.Now()
		response := &responseWriter{ResponseWriter: writer}
		next.ServeHTTP(response, request)
		if response.status == 0 {
			response.status = http.StatusOK
		}
		elapsed := time.Since(started)
		span := trace.SpanFromContext(request.Context())
		span.SetAttributes(attribute.String("http.route", strings.TrimPrefix(request.Pattern, request.Method+" ")))
		if response.status >= 500 {
			span.SetStatus(codes.Error, http.StatusText(response.status))
		}
		service.metrics.observe(request.Method, response.status, elapsed, response.bytes)
		level := slog.LevelDebug
		if response.status >= 500 {
			level = slog.LevelWarn
		}
		attributes := []any{"method", request.Method, "route", request.Pattern, "status", response.status, "duration", elapsed, "bytes", response.bytes}
		if span.SpanContext().IsValid() {
			attributes = append(attributes, "trace_id", span.SpanContext().TraceID().String(), "span_id", span.SpanContext().SpanID().String())
		}
		service.config.Logger.Log(request.Context(), level, "cache request", attributes...)
	})
}
