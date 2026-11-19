package axb

import (
	"testing"
	"time"
)

func TestBindAndResolve(t *testing.T) {
	s := NewStore()
	bd, err := s.Bind("13800001111", "13900002222", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(bd.XNumber) != 10 || bd.XNumber[0] != '9' {
		t.Errorf("X shape = %q", bd.XNumber)
	}
	got, ok := s.Resolve(bd.XNumber)
	if !ok || got.BNumber != "13900002222" || got.ANumber != "13800001111" {
		t.Fatalf("resolve = %+v", got)
	}
	// Pair reuse: the same (a,b) gets the same X back.
	bd2, _ := s.Bind("13800001111", "13900002222", time.Hour)
	if bd2.XNumber != bd.XNumber {
		t.Errorf("pair reuse broken: %s vs %s", bd2.XNumber, bd.XNumber)
	}
}

func TestExpiryLazyDeactivate(t *testing.T) {
	s := NewStore()
	bd, _ := s.Bind("138", "139", time.Hour)
	bd.ExpireAt = time.Now().Add(-time.Minute) // force-expired
	if _, ok := s.Resolve(bd.XNumber); ok {
		t.Fatal("expired binding must not resolve")
	}
	if bd.Active {
		t.Error("lazy expiry should flip Active off")
	}
}

func TestRevoke(t *testing.T) {
	s := NewStore()
	bd, _ := s.Bind("13811112222", "13933334444", time.Hour)
	if s.RevokeByID(bd.ID) != 1 {
		t.Fatal("revoke by id")
	}
	if _, ok := s.Resolve(bd.XNumber); ok {
		t.Fatal("revoked binding must not resolve")
	}
	if s.RevokeByID(bd.ID) != 0 {
		t.Error("double revoke = 0")
	}
}

func TestValidate(t *testing.T) {
	s := NewStore()
	if _, err := s.Bind("12", "139000", time.Hour); err == nil {
		t.Error("short a rejected")
	}
	if _, err := s.Bind("   ", "139000", time.Hour); err == nil {
		t.Error("blank a rejected")
	}
}
