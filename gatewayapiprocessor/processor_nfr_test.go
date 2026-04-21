package gatewayapiprocessor

import (
	"context"
	"fmt"
	"os"
	"sort"
	"testing"
	"time"
)

// NFR-1 threshold-assertion gate — PRD ISI-690 §6 NFR-1.
//
// These are regular Go tests (not benchmarks) that measure enrichment cost
// directly via time.Since and fail the build if the processor violates the
// performance budget. Benchmarks in processor_bench_test.go remain as
// reporting-only probes; this file is the hard gate that CI asserts against.
//
// Budgets (PRD NFR-1):
//   - Enrichment latency p95 ≤ 100µs per record at 10k-route cache.
//   - Throughput regression ≤ 5% at 10k spans/sec when processor is enabled.
//     Operationalized here as: sustained single-core enrichment throughput
//     ≥ 9500 records/sec at 10k-route cache. At 10k-rps pipeline load the
//     processor MUST be capable of absorbing the full signal without becoming
//     the bottleneck; 9500 rps leaves the 5% regression budget entirely on
//     the pipeline side.
//
// The gate is skipped under `-short` and when NFR1_SKIP=1 is set, to keep
// fast dev loops green. CI runs the full suite without -short, so the gate
// fires on every PR before merge to main.

const (
	nfr1RouteCacheSize    = 10_000
	nfr1LatencySamples    = 5_000
	nfr1LatencyBudget     = 100 * time.Microsecond
	nfr1ThroughputBudget  = 9_500   // records/sec, 5% regression budget off 10k
	nfr1ThroughputSeconds = 1
	nfr1ThroughputBatch   = 100
	nfr1WarmupIterations  = 500
)

func skipNFR1(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("NFR-1 gate skipped under -short")
	}
	if os.Getenv("NFR1_SKIP") == "1" {
		t.Skip("NFR-1 gate skipped (NFR1_SKIP=1)")
	}
}

// TestNFR1_EnrichmentLatency_p95_10kRoutes_LE_100us is the per-record latency
// gate. It stamps `nfr1LatencySamples` single-span traces through the enrichment
// hot path at a 10k-route cache and asserts p95 ≤ 100µs.
func TestNFR1_EnrichmentLatency_p95_10kRoutes_LE_100us(t *testing.T) {
	skipNFR1(t)

	tp := newBenchProcessors(t, seedLookup(nfr1RouteCacheSize))
	ctx := context.Background()

	// Warm up the fast path + allocator so the first-iteration cold cost
	// doesn't skew the p95 for the measured window.
	for i := 0; i < nfr1WarmupIterations; i++ {
		td := singleSpanWith(map[string]string{
			"route_name": fmt.Sprintf("httproute/demo/api-%d/rule/0/match/0", i%nfr1RouteCacheSize),
		})
		_ = tp.traces.ConsumeTraces(ctx, td)
	}

	durations := make([]time.Duration, nfr1LatencySamples)
	for i := 0; i < nfr1LatencySamples; i++ {
		td := singleSpanWith(map[string]string{
			"route_name": fmt.Sprintf("httproute/demo/api-%d/rule/0/match/0", i%nfr1RouteCacheSize),
		})
		start := time.Now()
		_ = tp.traces.ConsumeTraces(ctx, td)
		durations[i] = time.Since(start)
	}

	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	p50 := durations[nfr1LatencySamples/2]
	p95 := durations[int(float64(nfr1LatencySamples)*0.95)]
	p99 := durations[int(float64(nfr1LatencySamples)*0.99)]

	t.Logf("NFR-1 enrichment latency @ %d-route cache: p50=%s p95=%s p99=%s (budget p95 ≤ %s, %d samples)",
		nfr1RouteCacheSize, p50, p95, p99, nfr1LatencyBudget, nfr1LatencySamples)

	if p95 > nfr1LatencyBudget {
		t.Fatalf("NFR-1 violation: p95 enrichment latency %s > %s budget at %d-route cache",
			p95, nfr1LatencyBudget, nfr1RouteCacheSize)
	}
}

// TestNFR1_Throughput_10kRoutesCache_GE_9500rps is the throughput gate. It
// runs the processor for `nfr1ThroughputSeconds` and asserts sustained
// single-core throughput ≥ 9500 records/sec at a 10k-route cache.
func TestNFR1_Throughput_10kRoutesCache_GE_9500rps(t *testing.T) {
	skipNFR1(t)

	tp := newBenchProcessors(t, seedLookup(nfr1RouteCacheSize))
	ctx := context.Background()

	// Prime the pipeline so initialization work doesn't count against
	// the throughput window.
	for i := 0; i < 10; i++ {
		_ = tp.traces.ConsumeTraces(ctx, tracesWithNRoutes(nfr1ThroughputBatch))
	}

	window := time.Duration(nfr1ThroughputSeconds) * time.Second
	deadline := time.Now().Add(window)
	start := time.Now()

	records := 0
	for time.Now().Before(deadline) {
		_ = tp.traces.ConsumeTraces(ctx, tracesWithNRoutes(nfr1ThroughputBatch))
		records += nfr1ThroughputBatch
	}
	elapsed := time.Since(start)
	rps := float64(records) / elapsed.Seconds()

	t.Logf("NFR-1 throughput @ %d-route cache: %.0f rps over %s (%d records, budget ≥ %d rps)",
		nfr1RouteCacheSize, rps, elapsed.Round(time.Millisecond), records, nfr1ThroughputBudget)

	if rps < nfr1ThroughputBudget {
		t.Fatalf("NFR-1 violation: sustained throughput %.0f rps < %d rps budget at %d-route cache",
			rps, nfr1ThroughputBudget, nfr1RouteCacheSize)
	}
}

// TestNFR1_MetricsEnrichmentLatency_p95_10kRoutes_LE_100us mirrors the trace
// gate on the metrics pipeline. The stripping codepath is different (cardinality
// guard runs on metrics only), so both paths need their own budget assertion.
func TestNFR1_MetricsEnrichmentLatency_p95_10kRoutes_LE_100us(t *testing.T) {
	skipNFR1(t)

	tp := newBenchProcessors(t, seedLookup(nfr1RouteCacheSize))
	ctx := context.Background()

	for i := 0; i < nfr1WarmupIterations; i++ {
		md := singleSumMetricWith(map[string]string{
			"route_name": fmt.Sprintf("httproute/demo/api-%d/rule/0/match/0", i%nfr1RouteCacheSize),
		})
		_ = tp.metrics.ConsumeMetrics(ctx, md)
	}

	durations := make([]time.Duration, nfr1LatencySamples)
	for i := 0; i < nfr1LatencySamples; i++ {
		md := singleSumMetricWith(map[string]string{
			"route_name": fmt.Sprintf("httproute/demo/api-%d/rule/0/match/0", i%nfr1RouteCacheSize),
		})
		start := time.Now()
		_ = tp.metrics.ConsumeMetrics(ctx, md)
		durations[i] = time.Since(start)
	}

	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	p50 := durations[nfr1LatencySamples/2]
	p95 := durations[int(float64(nfr1LatencySamples)*0.95)]
	p99 := durations[int(float64(nfr1LatencySamples)*0.99)]

	t.Logf("NFR-1 metrics enrichment latency @ %d-route cache: p50=%s p95=%s p99=%s (budget p95 ≤ %s, %d samples)",
		nfr1RouteCacheSize, p50, p95, p99, nfr1LatencyBudget, nfr1LatencySamples)

	if p95 > nfr1LatencyBudget {
		t.Fatalf("NFR-1 violation: p95 metrics enrichment latency %s > %s budget at %d-route cache",
			p95, nfr1LatencyBudget, nfr1RouteCacheSize)
	}
}
