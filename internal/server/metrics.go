package server

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/piotrrojek/gocacheport/internal/store"
)

type metrics struct {
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
	bytes    *prometheus.CounterVec
	uploaded prometheus.Counter
	inflight prometheus.Gauge
	handler  http.Handler
}

func newMetrics(cache *store.Store, version string) metrics {
	registry := prometheus.NewRegistry()
	metrics := metrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gocacheport_http_requests_total", Help: "Artifact HTTP requests by method and status class.",
		}, []string{"method", "status_class"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "gocacheport_http_request_duration_seconds", Help: "Artifact HTTP request duration.",
			Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 5, 30},
		}, []string{"method", "status_class"}),
		bytes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gocacheport_http_response_bytes_total", Help: "Artifact HTTP response bytes.",
		}, []string{"method", "status_class"}),
		uploaded: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "gocacheport_uploaded_bytes_total", Help: "Successfully committed artifact payload bytes.",
		}),
		inflight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "gocacheport_http_inflight", Help: "Currently admitted artifact HTTP requests.",
		}),
	}
	registry.MustRegister(metrics.requests, metrics.duration, metrics.bytes, metrics.uploaded, metrics.inflight,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	counters := []struct {
		name string
		help string
		read func(store.Stats) uint64
	}{
		{"memory_hits", "Artifact hits served from memory.", func(stats store.Stats) uint64 { return stats.MemoryHits }},
		{"disk_hits", "Artifact hits served from verified disk records.", func(stats store.Stats) uint64 { return stats.DiskHits }},
		{"misses", "Artifact cache misses.", func(stats store.Stats) uint64 { return stats.Misses }},
		{"writes", "Successfully committed artifact uploads.", func(stats store.Stats) uint64 { return stats.Writes }},
		{"evictions", "Disk capacity and background TTL evictions.", func(stats store.Stats) uint64 { return stats.Evictions }},
		{"memory_evictions", "Artifacts evicted from the memory tier.", func(stats store.Stats) uint64 { return stats.MemoryEvictions }},
		{"corruptions", "Invalid stored records detected.", func(stats store.Stats) uint64 { return stats.Corruptions }},
	}
	for _, counter := range counters {
		registry.MustRegister(prometheus.NewCounterFunc(prometheus.CounterOpts{
			Name: "gocacheport_" + counter.name + "_total", Help: counter.help,
		}, func() float64 { return float64(counter.read(cache.Snapshot())) }))
	}
	gauges := []struct {
		name string
		help string
		read func(store.Stats) int64
	}{
		{"disk_bytes", "Committed record bytes, excluding temporary uploads and filesystem overhead.", func(stats store.Stats) int64 { return stats.DiskBytes }},
		{"disk_entries", "Committed artifact records.", func(stats store.Stats) int64 { return stats.DiskEntries }},
		{"memory_bytes", "Memory-tier charged bytes, including per-entry allowance.", func(stats store.Stats) int64 { return stats.MemoryBytes }},
		{"memory_entries", "Memory-tier artifacts.", func(stats store.Stats) int64 { return stats.MemoryEntries }},
	}
	for _, gauge := range gauges {
		registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "gocacheport_" + gauge.name, Help: gauge.help,
		}, func() float64 { return float64(gauge.read(cache.Snapshot())) }))
	}
	build := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "gocacheport_build_info", Help: "gocacheport build version.",
	}, []string{"version"})
	build.WithLabelValues(version).Set(1)
	registry.MustRegister(build)
	metrics.handler = promhttp.HandlerFor(registry, promhttp.HandlerOpts{EnableOpenMetrics: true})
	return metrics
}

func (metrics *metrics) observe(method string, status int, elapsed time.Duration, bytes int64) {
	if method == http.MethodHead {
		method = http.MethodGet
	}
	if method != http.MethodGet && method != http.MethodPut {
		method = "OTHER"
	}
	statusClass := strconv.Itoa(status/100) + "xx"
	metrics.requests.WithLabelValues(method, statusClass).Inc()
	metrics.duration.WithLabelValues(method, statusClass).Observe(elapsed.Seconds())
	metrics.bytes.WithLabelValues(method, statusClass).Add(float64(bytes))
}
