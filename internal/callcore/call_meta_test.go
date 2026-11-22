package callcore

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/gvos-io/gvos-core/internal/billingdomain/account"
	"github.com/gvos-io/gvos-core/internal/billingdomain/money"
	"github.com/gvos-io/gvos-core/internal/billingdomain/rate"
	"github.com/gvos-io/gvos-core/internal/shared/rds"
)

func TestRemoteSettleFromMeta(t *testing.T) {
	mr := miniredis.RunT(t)
	c, err := rds.New("redis://" + mr.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	store := account.NewStore()
	store.Put(&account.Account{ID: "u1", Credit: money.MustFromYuan(100), RateGroup: "g"})
	bal := account.NewBalanceR(store, c)
	if err := bal.Seed(context.Background()); err != nil {
		t.Fatal(err)
	}

	owner := NewManager(store, nil, time.Second)
	owner.SetBalanceAuthority(bal)
	owner.SetCallMetaStore(NewCallMetaStore(c))
	owner.SetAdmissionLedger(NewAdmissionR(c))

	peer := NewManager(store, nil, time.Second)
	peer.SetBalanceAuthority(bal)
	peer.SetCallMetaStore(NewCallMetaStore(c))
	peer.SetAdmissionLedger(NewAdmissionR(c))

	entry := &rate.Entry{Prefix: "86", Price: money.MustFromYuan(0.06), InitDur: 60, StepDur: 6}
	if err := owner.Start("c-remote", "u1", "8613800138000", entry, 60, false); err != nil {
		t.Fatal(err)
	}
	if owner.Live() != 1 {
		t.Fatalf("owner live = %d", owner.Live())
	}

	// BYE lands on the peer: local map miss → Redis meta settle.
	cdr, err := peer.End("c-remote", 60)
	if err != nil {
		t.Fatal(err)
	}
	if cdr.Release != "remote-settle" {
		t.Errorf("release = %q, want remote-settle", cdr.Release)
	}
	if _, err := peer.End("c-remote", 60); err == nil {
		t.Fatal("second remote End must miss (meta consumed)")
	}
	// Owner's late End must not double-charge (settle lock).
	if _, err := owner.End("c-remote", 60); err == nil {
		t.Fatal("owner End after peer settle must fail")
	}
	balAfter, _ := bal.Get(context.Background(), "u1")
	// 60s at 0.06/min → 0.06 charged from 100.
	want := money.MustFromYuan(100).Sub(money.MustFromYuan(0.06))
	if balAfter != want {
		t.Errorf("balance after remote settle = %s, want %s", balAfter, want)
	}
}
