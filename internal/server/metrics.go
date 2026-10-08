package server

import (
	"fmt"
	"io"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Buckets do histograma de latência, em segundos.
var latencyBuckets = []float64{0.0005, 0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1}

// Metrics acumula contadores e exporta no formato de texto do Prometheus.
type Metrics struct {
	created     atomic.Int64
	redirects   atomic.Int64
	rateLimited atomic.Int64

	mu       sync.Mutex
	requests map[reqKey]int64

	latCounts [9]atomic.Int64 // len(latencyBuckets)+1 (último = +Inf)
	latSumNs  atomic.Int64
	latTotal  atomic.Int64
}

type reqKey struct {
	method string
	code   int
}

func newMetrics() *Metrics { return &Metrics{requests: make(map[reqKey]int64)} }

func (m *Metrics) observe(method string, status int, d time.Duration) {
	m.mu.Lock()
	m.requests[reqKey{method, status}]++
	m.mu.Unlock()

	sec := d.Seconds()
	idx := len(latencyBuckets)
	for i, b := range latencyBuckets {
		if sec <= b {
			idx = i
			break
		}
	}
	m.latCounts[idx].Add(1)
	m.latSumNs.Add(d.Nanoseconds())
	m.latTotal.Add(1)
}

// WritePrometheus escreve as métricas; linksGauge é o número atual de links.
func (m *Metrics) WritePrometheus(w io.Writer, linksGauge int64) {
	fmt.Fprintf(w, "# HELP shortener_links Links guardados no momento.\n# TYPE shortener_links gauge\nshortener_links %d\n", linksGauge)
	fmt.Fprintf(w, "# HELP shortener_links_created_total Links criados.\n# TYPE shortener_links_created_total counter\nshortener_links_created_total %d\n", m.created.Load())
	fmt.Fprintf(w, "# HELP shortener_redirects_total Redirecionamentos feitos.\n# TYPE shortener_redirects_total counter\nshortener_redirects_total %d\n", m.redirects.Load())
	fmt.Fprintf(w, "# HELP shortener_rate_limited_total Requisições barradas pelo rate limit.\n# TYPE shortener_rate_limited_total counter\nshortener_rate_limited_total %d\n", m.rateLimited.Load())

	fmt.Fprint(w, "# HELP http_requests_total Requisições HTTP por método e status.\n# TYPE http_requests_total counter\n")
	m.mu.Lock()
	keys := make([]reqKey, 0, len(m.requests))
	for k := range m.requests {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].method != keys[j].method {
			return keys[i].method < keys[j].method
		}
		return keys[i].code < keys[j].code
	})
	for _, k := range keys {
		fmt.Fprintf(w, "http_requests_total{method=%q,code=\"%d\"} %d\n", k.method, k.code, m.requests[k])
	}
	m.mu.Unlock()

	fmt.Fprint(w, "# HELP http_request_duration_seconds Latência das requisições.\n# TYPE http_request_duration_seconds histogram\n")
	var cum int64
	for i, b := range latencyBuckets {
		cum += m.latCounts[i].Load()
		fmt.Fprintf(w, "http_request_duration_seconds_bucket{le=\"%g\"} %d\n", b, cum)
	}
	cum += m.latCounts[len(latencyBuckets)].Load()
	fmt.Fprintf(w, "http_request_duration_seconds_bucket{le=\"+Inf\"} %d\n", cum)
	fmt.Fprintf(w, "http_request_duration_seconds_sum %g\n", float64(m.latSumNs.Load())/1e9)
	fmt.Fprintf(w, "http_request_duration_seconds_count %d\n", m.latTotal.Load())
}
