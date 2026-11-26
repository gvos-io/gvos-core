package api

import (
	"net/http"
	"os"
	"sync"

	"github.com/gvos-io/gvos-core/internal/shared/metrics"
)

// Platform metrics for the Prometheus scrape. Zero-dependency
// exposition (internal/shared/metrics); names follow the gvos_ prefix
// so the dashboard can filter one job cleanly.
//
// Scraping is unauthenticated by design: the port carries no secrets
// (counter/gauge values only) and the standard deployment binds it to
// the internal network. Operators who expose 8080 publicly should gate
// it at the ingress — a note lives in the operations manual.
var (
	mCallsStarted = metrics.Default.Counter("gvos_calls_started_total", "calls accepted through call/start (200s only)")
	mCallsEnded   = metrics.Default.Counter("gvos_calls_ended_total", "calls finalized through call/end (both HTTP and bus faces)")
	mCallsLive    = metrics.Default.Gauge("gvos_calls_live", "calls currently registered in the in-memory manager")
	mRPCSeconds   = metrics.Default.Histogram("gvos_callstart_seconds", "call/start (bill-lock) handler duration",
		[]float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5})
	mCDRDupAcked    = metrics.Default.Counter("gvos_cdr_dup_acked_total", "cdr.raw redeliveries for already-finalized calls (acked idempotently)")
	mNatsPublishErr = metrics.Default.Counter("gvos_nats_publish_errors_total", "NATS JetStream publish failures (before pump retries)")
	mConsumerNak    = metrics.Default.Counter("gvos_consumer_nak_total", "durable-consumer handler failures (NAKs, before redelivery)")
	mRedisErrors    = metrics.Default.Counter("gvos_redis_errors_total", "Redis operation failures (admission/balance chain)")
	mDevCreds       = metrics.Default.Counter("gvos_dev_credentials_total", "requests authenticated with demo/legacy credentials (alert on >0 in prod)")
	mBuildInfo      = metrics.Default.Gauge("gvos_build_info", "process identity (value 1; labels are the point)")
)

var (
	// buildVersion/buildMode are set once at process start so the
	// build_info gauge can carry them without label machinery.
	buildVersion = os.Getenv("GVOS_VERSION")
	buildMode    = os.Getenv("GVOS_MODE_LABEL")
	metricsOnce  sync.Once
)

func initBuildInfo() {
	metricsOnce.Do(func() {
		if buildVersion == "" {
			buildVersion = "dev"
		}
		if buildMode == "" {
			buildMode = "standalone"
		}
		mBuildInfo.Set(1)
	})
}

// handleMetrics serves the Prometheus text exposition. Registered on
// the same mux as the API: the scrape budget is one GET per interval
// and the body is counters only, so no auth dance for the scraper.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	mCallsLive.Set(float64(s.calls.Live()))
	initBuildInfo()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Write([]byte(metrics.Default.Render()))
}
