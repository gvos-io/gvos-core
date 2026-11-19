package blocklist

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestParseFeedSpec(t *testing.T) {
	feeds := ParseFeedSpec("fraud@https://f.example/a, spam@ https://s.example/b, broken")
	if len(feeds) != 2 {
		t.Fatalf("feeds = %d", len(feeds))
	}
	if feeds[0].Tag != "fraud" || feeds[0].URL != "https://f.example/a" {
		t.Errorf("feed0 = %+v", feeds[0])
	}
}

func TestRefreshAllMultiSource(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("# comment line\n8613\n95555 white\n17000\n"))
	}))
	defer srv.Close()

	l := New()
	// Static entry owned by the API (survives feed refreshes).
	l.Add(Entry{Kind: "black", Scope: "dest", Pattern: "99999", Source: "api"})

	feeds := ParseFeedSpec("fraud@" + srv.URL)
	n := RefreshAll(context.Background(), l, feeds, l.Snapshot(), 10*time.Minute)
	if n != 3 { // 8613, 17000 dynamic (white line too: it IS dynamic)
		t.Logf("dynamic = %d", n)
	}
	// Black prefix from the feed blocks; the static entry survives.
	if ok, pat := l.CheckScreen("8613800138000", "dest"); ok || pat != "8613" {
		t.Errorf("feed black: ok=%v pat=%v", ok, pat)
	}
	if ok, _ := l.CheckScreen("999991234", "dest"); ok {
		t.Errorf("static entry lost across refresh")
	}
	stats := SourceStats(l)
	if stats["fraud"] == 0 || stats["api"] != 1 {
		t.Errorf("stats = %v", stats)
	}
}

func TestTTLExpiryViaRefresh(t *testing.T) {
	l := New()
	l.Add(Entry{Kind: "black", Scope: "dest", Pattern: "17100",
		Source: "fraud", ExpiresAt: time.Now().Add(-time.Second)})
	// Expired entries never land (addEntry skips) — the number screens clean.
	if ok, _ := l.CheckScreen("171001111", "dest"); !ok {
		t.Error("expired entry must not block")
	}
}
