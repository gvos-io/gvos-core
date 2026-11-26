// sources.go: dynamic multi-source blocklist feeds (the VOS "dynamic
// multi-source blacklist" module's open equivalent). GVOS_BLOCKLIST_SOURCES
// lists feeds as "tag@url,tag2@url2"; each feed returns one prefix per
// line ("8613" or "8613 deny"), re-fetched on an interval and applied
// as a TTL'd Reload — a stale/dead feed simply stops contributing.
package blocklist

import (
	"context"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// Feed is one external prefix source.
type Feed struct {
	Tag string
	URL string
}

// ParseFeedSpec parses "tag@url" pairs from the env list.
func ParseFeedSpec(spec string) []Feed {
	var out []Feed
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		tag, url, found := strings.Cut(part, "@")
		if !found || tag == "" || url == "" {
			log.Printf("blocklist feed spec %q malformed (want tag@url), skipped", part)
			continue
		}
		out = append(out, Feed{Tag: tag, URL: url})
	}
	return out
}

// fetchFeed pulls one feed's prefix lines (HTTP GET; a feed that
// serves anything else just yields nothing — fail-open by design:
// a dead fraud feed must never take the platform down).
func fetchFeed(ctx context.Context, f Feed, ttl time.Duration) []Entry {
	req, err := http.NewRequestWithContext(ctx, "GET", f.URL, nil)
	if err != nil {
		return nil
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("blocklist feed %s: %v (skipped this round)", f.Tag, err)
		return nil
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil || resp.StatusCode != 200 {
		log.Printf("blocklist feed %s: status %d (skipped)", f.Tag, resp.StatusCode)
		return nil
	}
	expiry := time.Now().Add(ttl)
	var out []Entry
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		kind, scope := "black", "dest"
		fields := strings.Fields(line)
		prefix := fields[0]
		for _, extra := range fields[1:] {
			if extra == "white" || extra == "black" {
				kind = extra
			}
			if extra == "src" {
				scope = "src"
			}
		}
		out = append(out, Entry{
			Kind: kind, Scope: scope, Pattern: prefix,
			Source: f.Tag, ExpiresAt: expiry,
		})
	}
	return out
}

// RefreshAll fetches every feed and Reloads the list with the union
// of dynamic entries (feeds) plus the static set the API owns. The
// static entries survive because Reload receives them explicitly.
func RefreshAll(ctx context.Context, l *List, feeds []Feed, static []Entry, ttl time.Duration) (dynamic int) {
	var all []Entry
	all = append(all, static...)
	for _, f := range feeds {
		entries := fetchFeed(ctx, f, ttl)
		for _, e := range entries {
			e.Source = f.Tag
			all = append(all, e)
		}
	}
	l.Reload(all)
	for _, e := range all {
		if e.Source != "" && e.Source != "api" && e.Source != "static" {
			dynamic++
		}
	}
	return dynamic
}

// RunFeedLoop re-fetches on a ticker until ctx ends (the api layer
// starts it when GVOS_BLOCKLIST_SOURCES is set).
func RunFeedLoop(ctx context.Context, l *List, feeds []Feed, static []Entry, interval, ttl time.Duration) (stop func()) {
	t := time.NewTicker(interval)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				n := RefreshAll(ctx, l, feeds, static, ttl)
				if n > 0 {
					log.Printf("blocklist feeds refreshed: %d dynamic entries", n)
				}
			}
		}
	}()
	return func() { t.Stop(); <-done }
}

// SourceStats counts entries per source (observability).
func SourceStats(l *List) map[string]int {
	out := map[string]int{}
	for _, e := range l.Snapshot() {
		tag := e.Source
		if tag == "" {
			tag = "static"
		}
		out[tag]++
	}
	return out
}
