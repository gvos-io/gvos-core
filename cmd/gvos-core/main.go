// Command gvos-core is the billing control plane: it exposes the
// event endpoints Kamailio calls (call start/answer/end) plus health.
// Without -dsn it runs fully in memory with demo seed data (dev/test);
// with -dsn it loads accounts/rates from PostgreSQL and persists CDRs,
// ledger and audit rows.
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gvos-io/gvos-core/internal/api"
	"github.com/gvos-io/gvos-core/internal/shared/events"
	"github.com/gvos-io/gvos-core/internal/shared/rds"
)

func main() {
	listen := flag.String("listen", ":8080", "HTTP listen address")
	dsn := flag.String("dsn", "", "PostgreSQL DSN; empty = in-memory demo mode")
	redisURL := flag.String("redis", "", "Redis URL (runtime-state authority); empty = off, memory authoritative")
	natsURL := flag.String("nats", "", "NATS server URL; empty = event bus off")
	webhook := flag.String("webhook", "", "Webhook endpoint URL (cdr.completed events; requires -nats)")
	tlsCert := flag.String("tls-cert", os.Getenv("GVOS_TLS_CERT"), "TLS cert (enables HTTPS)")
	tlsKey := flag.String("tls-key", os.Getenv("GVOS_TLS_KEY"), "TLS key")
	flag.Parse()

	// Compose/deploy environments pass the DSN via env; the flag wins
	// when both are set.
	dsnVal := *dsn
	if dsnVal == "" {
		dsnVal = os.Getenv("GVOS_DB_DSN")
	}
	if dsnVal == "" {
		// k8s form: secretKeyRef optional leaves the var unset when
		// gvos-secrets/db-dsn is absent — the manifest's explicit
		// dev fallback takes over (deploy/k8s/03-gv-core.yaml).
		dsnVal = os.Getenv("GVOS_DB_DSN_FALLBACK")
	}
	redisVal := *redisURL
	if redisVal == "" {
		redisVal = os.Getenv("GVOS_REDIS_URL")
	}
	natsVal := *natsURL
	if natsVal == "" {
		natsVal = os.Getenv("GVOS_NATS_URL")
	}
	webhookVal := *webhook
	if webhookVal == "" {
		webhookVal = os.Getenv("GVOS_WEBHOOK_URL")
	}

	var rc *rds.Client
	if redisVal != "" {
		var err error
		rc, err = rds.New(redisVal)
		if err != nil {
			log.Fatalf("redis configured but unreachable (%v) — the money authority does not start degraded", err)
		}
	}
	var s *api.Server
	if dsnVal != "" {
		// F-7: a PG blip at boot (maintenance window, standby
		// promotion, cluster restart) used to Fatal the process and
		// hand the outage to the restart policy. Retry with capped
		// backoff instead — boot is not the money path; the pool
		// itself reconnects post-New once alive.
		var err error
		for attempt := 1; ; attempt++ {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			s, err = api.NewPGServer(ctx, dsnVal)
			cancel()
			if err == nil {
				break
			}
			if attempt >= 20 {
				log.Fatalf("postgres mode failed after %d attempts: %v", attempt, err)
			}
			backoff := time.Duration(attempt) * 3 * time.Second
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
			log.Printf("postgres unreachable (attempt %d, retry in %s): %v", attempt, backoff, err)
			time.Sleep(backoff)
		}
	} else {
		s = api.NewServer()
	}
	// First admin API key (Phase B): derived from GVOS_ADMIN_TOKEN or
	// generated + printed once. Runs before the HTTP server starts.
	api.BootstrapKey(s)
	if rc != nil {
		api.AttachRedis(s, rc)
	}
	// Dynamic blocklist feeds (VOS multi-source blacklist equivalent):
	// GVOS_BLOCKLIST_SOURCES="tag@url,..." re-fetches on an interval
	// and applies TTL'd entries over the static API-owned set.
	// Outbound webhooks (Phase D): GVOS_WEBHOOK_EVENTS toggles the
	// dispatcher (subscriptions live in PG 011; delivery signs with
	// the stored HMAC secret and retries on the backoff schedule).
	if os.Getenv("GVOS_WEBHOOK_EVENTS") == "1" {
		if api.StartWebhookFanout(s) {
			log.Printf("webhook fanout on (subscriptions from PG)")
		}
	}
	if spec := os.Getenv("GVOS_BLOCKLIST_SOURCES"); spec != "" {
		feedCtx, feedCancel := context.WithCancel(context.Background())
		defer feedCancel()
		api.AttachBlocklistFeeds(s, spec, feedCtx)
	}
	// Recording pipeline (R2/R3): on when GVOS_RECORDING_OUTPUT is set.
	// Archive: LocalFS (GVOS_RECORDING_ARCHIVE) or S3/MinIO/OSS via
	// GVOS_RECORDING_S3_ENDPOINT + _AK/_SK/_BUCKET (presigned play).
	recOut := os.Getenv("GVOS_RECORDING_OUTPUT")
	if recOut != "" {
		if u := api.AttachRecording(s, recOut, os.Getenv("GVOS_RECORDING_ARCHIVE")); u != nil {
			pipelineCtx, pipelineCancel := context.WithCancel(context.Background())
			defer pipelineCancel()
			api.StartRecordingPipeline(s, pipelineCtx)
			log.Printf("recording pipeline on (output=%s backend env S3=%v)", recOut, os.Getenv("GVOS_RECORDING_S3_ENDPOINT") != "")
		}
	}
	// Event bus is auxiliary, not authority: a missing/unreachable
	// broker must not take the control plane down (it reconnects on
	// its own; publishes just log until the broker appears).
	if natsVal != "" {
		if pub, err := events.New(natsVal); err != nil {
			log.Printf("NATS event bus disabled (connect failed: %v)", err)
		} else {
			api.AttachBus(s, pub, webhookVal)
			api.StartAsyncWorkers(s)
			api.StartCfgReloadSubscriber(s)
			api.StartCallSvc(s)
			api.StartStatsSvc(s)
			api.StartRegConsumer(s)
			api.StartCdrRawConsumer(s)
			api.StartCfgRebroadcaster(s)
			if webhookVal != "" {
				// The legacy single-URL dispatcher: a consumer whose
				// lifecycle api.Shutdown already drains with the other
				// workers.
				api.StartWebhookDispatcher(s)
			}
		}
	}
	log.Printf("gvos-core listening on %s (workers=%v)", *listen, api.WorkerNames(s))
	handler := api.MuxFor(s)
	// Timeouts: none of the faces are slow endpoints (the bill-lock RPC
	// answers in milliseconds), so a slowloris-style open connection is
	// pure attack surface — the previous bare ListenAndServe never
	// timed out at all. Long-lived faces are exempt by size:
	// ReadTimeout would kill the SSE stream mid-push, so only the
	// header and idle halves are bounded there.
	srv := &http.Server{
		Addr:              *listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	if *tlsCert != "" && *tlsKey != "" {
		srv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		log.Printf("TLS enabled (%s)", *tlsCert)
	}

	// Graceful shutdown: SIGINT/SIGTERM stop taking calls, in-flight
	// requests get a bounded drain, then the workers drain and the bus
	// flushes. (And the webhook consumer stops exactly once — the old
	// exit path called Stop twice and panicked.)
	var runErr error
	serveErr := make(chan error, 1)
	go func() {
		if *tlsCert != "" && *tlsKey != "" {
			serveErr <- srv.ListenAndServeTLS(*tlsCert, *tlsKey)
		} else {
			serveErr <- srv.ListenAndServe()
		}
	}()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	select {
	case err := <-serveErr:
		runErr = err
	case sig := <-stop:
		log.Printf("shutting down on %s", sig)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("HTTP shutdown: %v", err)
		}
	}

	api.Shutdown(s) // drain workers, close the event bus
	_ = rc.Close()  // then the redis authority (rc nil-safe)
	if runErr != nil && runErr != http.ErrServerClosed {
		log.Fatal(runErr)
	}
}
