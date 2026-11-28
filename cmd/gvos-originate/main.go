// gvos-originate: the outboard UAC dialer (M11 F1 edge side). Consumes
// gvos.call.originate (durable queue — multiple instances share the
// workload, a down dialer holds instructions in the stream) and issues
// the SIP signaling toward the edge; billing/settlement ride the
// INVITEs the edge sees through the normal faces.
//
//   GVOS_ORIG_NATS_URL      NATS endpoint (default nats://127.0.0.1:4222)
//   GVOS_ORIG_SIP_ADDR      edge host:port (required)
//   GVOS_ORIG_SIP_HOST      announced Via/Contact host (default 127.0.0.1)
//   GVOS_ORIG_SIP_PORT      local UDP port (default 5070)
//   GVOS_ORIG_REALM         SIP domain (default gvos.local)
package main

import (
	"log"
	"os"
	"strconv"

	"github.com/gvos-io/gvos-core/internal/originate"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("ORIG ")
	natsURL := os.Getenv("GVOS_ORIG_NATS_URL")
	if natsURL == "" {
		natsURL = "nats://127.0.0.1:4222"
	}
	port := 5070
	if v := os.Getenv("GVOS_ORIG_SIP_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			port = n
		}
	}
	opts := originate.Options{
		NATSURL:      natsURL,
		SIPAddr:      os.Getenv("GVOS_ORIG_SIP_ADDR"),
		SIPLocalHost: os.Getenv("GVOS_ORIG_SIP_HOST"),
		SIPLocalPort: port,
		Realm:        os.Getenv("GVOS_ORIG_REALM"),
	}
	if opts.SIPAddr == "" {
		log.Fatal("GVOS_ORIG_SIP_ADDR required (edge host:port)")
	}
	if opts.SIPLocalHost == "" {
		opts.SIPLocalHost = "127.0.0.1"
	}
	if opts.Realm == "" {
		opts.Realm = "gvos.local"
	}
	if _, err := originate.Start(opts); err != nil {
		log.Fatal(err)
	}
	log.Printf("dialer running; kill to stop")
	select {} // stateless worker — the kill path is the shutdown path
}
