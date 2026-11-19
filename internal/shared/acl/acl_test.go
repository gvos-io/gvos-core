package acl

import (
	"strings"
	"testing"
)

func TestGenerateKeyShape(t *testing.T) {
	pt, prefix, hash := GenerateKey(RoleAdmin)
	if !strings.HasPrefix(pt, "gvos_admin_") || len(pt) != len("gvos_admin_")+32 {
		t.Errorf("plaintext shape wrong: %q", pt)
	}
	if !strings.HasPrefix(prefix, "gvos_admin_") || len(prefix) != len("gvos_admin_")+4 {
		t.Errorf("prefix shape wrong: %q", prefix)
	}
	if HashKey(pt) != hash {
		t.Error("hash mismatch with HashKey")
	}
	_, prefix2, _ := GenerateKey(RoleTenant)
	if !strings.HasPrefix(prefix2, "gvos_tenant_") {
		t.Errorf("tenant scope wrong: %q", prefix2)
	}
}

func TestLookupRevocation(t *testing.T) {
	s := NewStore()
	pt, _, hash := GenerateKey(RoleAdmin)
	s.Add(Key{ID: "k1", TenantID: "t1", Role: RoleAdmin, Prefix: "p", Hash: hash, CreatedBy: "boot"})

	if k, ok := s.Lookup(pt); !ok || k.Role != RoleAdmin || k.TenantID != "t1" {
		t.Fatalf("lookup miss: %v %v", k, ok)
	}
	if _, ok := s.Lookup("gvos_admin_wrong"); ok {
		t.Error("wrong key must miss")
	}
	if n := s.Revoke("p"); n != 1 {
		t.Errorf("revoke n = %d", n)
	}
	if _, ok := s.Lookup(pt); ok {
		t.Error("revoked key must miss")
	}
}

func TestLastUsedObservability(t *testing.T) {
	s := NewStore()
	pt, _, hash := GenerateKey(RoleOperator)
	s.Add(Key{ID: "k2", TenantID: "t1", Role: RoleOperator, Prefix: "q", Hash: hash, CreatedBy: "boot"})
	if _, ok := s.Lookup(pt); !ok {
		t.Fatal("lookup fail")
	}
	s.mu.RLock()
	var used bool
	for _, k := range s.keys {
		if k.ID == "k2" && k.LastUsed != nil {
			used = true
		}
	}
	s.mu.RUnlock()
	if !used {
		t.Error("LastUsed not stamped")
	}
}
