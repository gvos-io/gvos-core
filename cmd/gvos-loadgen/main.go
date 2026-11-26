// Command gvos-loadgen is the controlled-CPS billing load generator
// (the tool behind the capacity reports — committed so the capacity
// narrative is reproducible). It drives the call/start + call/end
// HTTP face at a fixed call rate, records start-latency percentiles,
// and reports success/failure counts.
//
// Usage:
//
//	go run ./cmd/gvos-loadgen -target http://host:8080 -cps 100 -duration 30s
//	  -token <internal-token> -from 1000 -to 8613800138000
//
// Multiple -target values round-robin across gv-core replicas.
package main

import (
	"flag"
	"fmt"
	"math/rand"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type stats struct {
	mu        sync.Mutex
	latencies []float64 // ms
	ok, fail  atomic.Int64
}

func (s *stats) record(ms float64, ok bool) {
	s.mu.Lock()
	s.latencies = append(s.latencies, ms)
	s.mu.Unlock()
	if ok {
		s.ok.Add(1)
	} else {
		s.fail.Add(1)
	}
}

func (s *stats) report(elapsed time.Duration) {
	s.mu.Lock()
	lat := append([]float64(nil), s.latencies...)
	s.mu.Unlock()
	sort.Float64s(lat)
	pick := func(p float64) float64 {
		if len(lat) == 0 {
			return 0
		}
		i := int(float64(len(lat)-1) * p)
		return lat[i]
	}
	fmt.Printf("elapsed=%s calls=%d ok=%d fail=%d cps=%.1f\n",
		elapsed.Round(time.Millisecond), s.ok.Load()+s.fail.Load(), s.ok.Load(), s.fail.Load(),
		float64(s.ok.Load()+s.fail.Load())/elapsed.Seconds())
	fmt.Printf("start-latency ms: p50=%.1f p95=%.1f p99=%.1f max=%.1f\n",
		pick(0.50), pick(0.95), pick(0.99), lat[len(lat)-1])
}

func main() {
	targets := flag.String("target", "http://127.0.0.1:28080", "comma-separated gv-core base URLs")
	cps := flag.Int("cps", 100, "calls per second")
	duration := flag.Duration("duration", 30*time.Second, "test duration")
	token := flag.String("token", "dev-internal-token", "X-GVOS-Internal token")
	from := flag.String("from", "1000", "from_user")
	to := flag.String("to", "8613800138000", "to_user")
	flag.Parse()

	tgt := strings.Split(*targets, ",")
	for i := range tgt {
		tgt[i] = strings.TrimSpace(tgt[i])
	}
	st := &stats{}
	client := &http.Client{Timeout: 10 * time.Second}

	deadline := time.Now().Add(*duration)
	interval := time.Second / time.Duration(*cps)
	var inFlight sync.WaitGroup
	var sent atomic.Int64

	for time.Now().Before(deadline) {
		start := time.Now()
		// Fire one call (start + end pair) against the next replica.
		t := tgt[int(sent.Load())%len(tgt)]
		sent.Add(1)
		inFlight.Add(1)
		go func(t string) {
			defer inFlight.Done()
			callID := fmt.Sprintf("lg-%d-%d", time.Now().UnixNano(), rand.Intn(100000))
			t0 := time.Now()
			ok := true
			req, _ := http.NewRequest("POST", t+"/v1/call/start",
				strings.NewReader(fmt.Sprintf(`{"call_id":"%s","from_user":"%s","to_user":"%s"}`, callID, *from, *to)))
			req.Header.Set("X-GVOS-Internal", *token)
			req.Header.Set("Content-Type", "application/json")
			resp, err := client.Do(req)
			if err != nil || resp.StatusCode != 200 {
				ok = false
				if resp != nil {
					resp.Body.Close()
				}
			} else {
				resp.Body.Close()
				req2, _ := http.NewRequest("POST", t+"/v1/call/end",
					strings.NewReader(fmt.Sprintf(`{"call_id":"%s","duration":1,"release":"normal"}`, callID)))
				req2.Header.Set("X-GVOS-Internal", *token)
				req2.Header.Set("Content-Type", "application/json")
				resp2, err2 := client.Do(req2)
				if err2 != nil || resp2.StatusCode != 200 {
					ok = false
				}
				if resp2 != nil {
					resp2.Body.Close()
				}
			}
			st.record(float64(time.Since(t0).Microseconds())/1000.0, ok)
		}(t)
		// Pace to the target rate.
		sleep := interval - time.Since(start)
		if sleep > 0 {
			time.Sleep(sleep)
		}
	}
	inFlight.Wait()
	st.report(*duration)
}
