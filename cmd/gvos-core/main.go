// Command gvos-core is the billing control plane: it exposes the
// event endpoints Kamailio calls (call start/answer/end) plus health.
// Without -dsn it runs fully in memory with demo seed data (dev/test);
// with -dsn it loads accounts/rates from PostgreSQL and persists CDRs,
// ledger and audit rows.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/lixuanqun/gvos-core/internal/api"
	"github.com/lixuanqun/gvos-core/internal/customerdomain/blocklist"
	"github.com/lixuanqun/gvos-core/internal/shared/rds"
	"github.com/lixuanqun/gvos-core/internal/shared/events"
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
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		var err error
		s, err = api.NewPGServer(ctx, dsnVal)
		cancel()
		if err != nil {
			log.Fatalf("postgres mode failed: %v", err)
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
	if spec := os.Getenv("GVOS_BLOCKLIST_SOURCES"); spec != "" {
		feeds := blocklist.ParseFeedSpec(spec)
		if len(feeds) > 0 {
			feedCtx, feedCancel := context.WithCancel(context.Background())
			defer feedCancel()
			blocklist.RunFeedLoop(feedCtx, s.blocklist, feeds, s.blocklist.Snapshot(), 5*time.Minute, 15*time.Minute)
			log.Printf("blocklist feeds on: %d sources", len(feeds))
		}
	}
	// Recording pipeline (R2): on when GVOS_RECORDING_OUTPUT is set;
	// artifacts archive to GVOS_RECORDING_ARCHIVE (LocalFS backend;
	// OSS plugs into the same Storage interface once creds exist).
	recOut := os.Getenv("GVOS_RECORDING_OUTPUT")
	if recOut != "" {
		if u := api.AttachRecording(s, recOut, os.Getenv("GVOS_RECORDING_ARCHIVE")); u != nil {
			pipelineCtx, pipelineCancel := context.WithCancel(context.Background())
			defer pipelineCancel()
			api.StartRecordingPipeline(s, pipelineCtx)
			log.Printf("recording pipeline on (output=%s archive=%s)", recOut, os.Getenv("GVOS_RECORDING_ARCHIVE"))
		}
	}
	// Event bus is auxiliary, not authority: a missing/unreachable
	// broker must not take the control plane down (it reconnects on
	// its own; publishes just log until the broker appears).
	var webhookConsumer *events.Consumer
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
				webhookConsumer = api.StartWebhookDispatcher(s)
			}
		}
	}
	log.Printf("gvos-core listening on %s (workers=%v)", *listen, api.WorkerNames(s))
	var runErr error
	if *tlsCert != "" && *tlsKey != "" {
		log.Printf("TLS enabled (%s)", *tlsCert)
		runErr = http.ListenAndServeTLS(*listen, *tlsCert, *tlsKey, api.MuxFor(s))
	} else {
		runErr = http.ListenAndServe(*listen, api.MuxFor(s))
	}
	if webhookConsumer != nil {
		webhookConsumer.Stop() // drain in-flight webhook deliveries
	}
	api.Shutdown(s)   // drain workers, close the event bus
	_ = rc.Close()  // then the redis authority (rc nil-safe)
	if webhookConsumer != nil {
		webhookConsumer.Stop()
	}
	if runErr != nil {
		log.Fatal(runErr)
	}
}