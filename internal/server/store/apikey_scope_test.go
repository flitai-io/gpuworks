package store

import (
	"errors"
	"reflect"
	"testing"
)

func scopeTestDeployment(t *testing.T, st *Store, name string) uint {
	t.Helper()
	dep, err := st.CreateDeployment(&Deployment{Name: name, ServedName: name})
	if err != nil {
		t.Fatal(err)
	}
	return dep.ID
}

func keyPrefix(t *testing.T, st *Store, name string) string {
	t.Helper()
	var k APIKey
	if err := st.db.Where("name = ?", name).First(&k).Error; err != nil {
		t.Fatal(err)
	}
	return k.Prefix
}

func TestScopedAPIKeyCreateRefusesBadScopesWithoutAKey(t *testing.T) {
	st := openAPIKeyStore(t)
	a := scopeTestDeployment(t, st, "a")

	_, err := st.CreateScopedAPIKey("unknown", 0, APIKeyScope{Restricted: true, DeploymentIDs: []uint{a, 999, 998}})
	var unknown *UnknownDeploymentsError
	if !errors.As(err, &unknown) || !reflect.DeepEqual(unknown.IDs, []uint{998, 999}) {
		t.Fatalf("err = %v", err)
	}
	for _, ids := range [][]uint{nil, {}, {0}} {
		if _, err := st.CreateScopedAPIKey("empty", 0, APIKeyScope{Restricted: true, DeploymentIDs: ids}); !errors.Is(err, ErrAPIKeyScopeEmpty) {
			t.Fatalf("ids %v: err = %v", ids, err)
		}
	}
	// The failed creates left neither a key nor a grant behind.
	var keys, grants int64
	st.db.Model(&APIKey{}).Count(&keys)
	st.db.Model(&APIKeyDeployment{}).Count(&grants)
	if keys != 0 || grants != 0 {
		t.Fatalf("keys=%d grants=%d", keys, grants)
	}
}

func TestScopedAPIKeyResolvesItsDeploymentsOnBothPaths(t *testing.T) {
	calls := countSlowChecks(t)
	st := openAPIKeyStore(t)
	a, b := scopeTestDeployment(t, st, "a"), scopeTestDeployment(t, st, "b")
	key, err := st.CreateScopedAPIKey("scoped", 3, APIKeyScope{Restricted: true, DeploymentIDs: []uint{a, a}})
	if err != nil {
		t.Fatal(err)
	}
	open, err := st.CreateAPIKey("open", 0)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		id, ok := st.ResolveAPIKey(key)
		if !ok || !id.Restricted || id.UserID != 3 || !id.Allows(a) || id.Allows(b) {
			t.Fatalf("scoped identity = %+v %v", id, ok)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("slow checks = %d, the second resolve must be cached", calls.Load())
	}
	id, ok := st.ResolveAPIKey(open)
	if !ok || id.Restricted || !id.Allows(a) || !id.Allows(b) || !id.Allows(12345) {
		t.Fatalf("unrestricted identity = %+v %v", id, ok)
	}
	grants, err := st.APIKeyGrants()
	scopedID, _ := st.ResolveAPIKey(key)
	if err != nil || !reflect.DeepEqual(grants, map[uint][]APIKeyGrant{scopedID.KeyID: {{APIKeyID: scopedID.KeyID, DeploymentID: a, Name: "a", ServedName: "a"}}}) {
		t.Fatalf("grants = %v err=%v", grants, err)
	}
}

func TestSetAPIKeyScopeTakesEffectOnTheNextRequest(t *testing.T) {
	calls := countSlowChecks(t)
	st := openAPIKeyStore(t)
	a, b := scopeTestDeployment(t, st, "a"), scopeTestDeployment(t, st, "b")
	key, err := st.CreateScopedAPIKey("k", 0, APIKeyScope{Restricted: true, DeploymentIDs: []uint{a}})
	if err != nil {
		t.Fatal(err)
	}
	prefix := keyPrefix(t, st, "k")
	if id, _ := st.ResolveAPIKey(key); id.Allows(b) {
		t.Fatal("b allowed before the edit")
	}

	if err := st.SetAPIKeyScope(prefix, APIKeyScope{Restricted: true, DeploymentIDs: []uint{b}}); err != nil {
		t.Fatal(err)
	}
	if id, _ := st.ResolveAPIKey(key); id.Allows(a) || !id.Allows(b) {
		t.Fatalf("after narrowing to b: %+v", id)
	}
	if calls.Load() != 2 {
		t.Fatalf("slow checks = %d, the edit must drop the cached scope", calls.Load())
	}

	// A failed edit keeps the old scope whole.
	err = st.SetAPIKeyScope(prefix, APIKeyScope{Restricted: true, DeploymentIDs: []uint{a, 777}})
	var unknown *UnknownDeploymentsError
	if !errors.As(err, &unknown) {
		t.Fatalf("err = %v", err)
	}
	if err := st.SetAPIKeyScope(prefix, APIKeyScope{Restricted: true}); !errors.Is(err, ErrAPIKeyScopeEmpty) {
		t.Fatalf("empty: %v", err)
	}
	if id, _ := st.ResolveAPIKey(key); id.Allows(a) || !id.Allows(b) {
		t.Fatalf("after failed edits: %+v", id)
	}

	// Widening to every deployment drops the grants.
	if err := st.SetAPIKeyScope(prefix, APIKeyScope{DeploymentIDs: []uint{a}}); err != nil {
		t.Fatal(err)
	}
	if id, _ := st.ResolveAPIKey(key); id.Restricted || !id.Allows(a) || !id.Allows(b) {
		t.Fatalf("after widening: %+v", id)
	}
	if grants, _ := st.APIKeyGrants(); len(grants) != 0 {
		t.Fatalf("grants = %v", grants)
	}

	if err := st.SetAPIKeyScope("gw-missing000", APIKeyScope{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown key: %v", err)
	}
	if err := st.RevokeAPIKey(prefix); err != nil {
		t.Fatal(err)
	}
	if err := st.SetAPIKeyScope(prefix, APIKeyScope{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked key: %v", err)
	}
}

func TestDeletingADeploymentDropsItsGrants(t *testing.T) {
	st := openAPIKeyStore(t)
	a, b := scopeTestDeployment(t, st, "a"), scopeTestDeployment(t, st, "b")
	both, err := st.CreateScopedAPIKey("both", 0, APIKeyScope{Restricted: true, DeploymentIDs: []uint{a, b}})
	if err != nil {
		t.Fatal(err)
	}
	onlyA, err := st.CreateScopedAPIKey("only-a", 0, APIKeyScope{Restricted: true, DeploymentIDs: []uint{a}})
	if err != nil {
		t.Fatal(err)
	}
	// Warm the cache with the grant that is about to go.
	if id, _ := st.ResolveAPIKey(onlyA); !id.Allows(a) {
		t.Fatal("a not allowed before the delete")
	}
	if err := st.DeleteDeployment(a); err != nil {
		t.Fatal(err)
	}
	if id, ok := st.ResolveAPIKey(both); !ok || id.Allows(a) || !id.Allows(b) {
		t.Fatalf("both after delete = %+v %v", id, ok)
	}
	// Losing its last grant leaves the key restricted to nothing, never
	// widened to everything.
	id, ok := st.ResolveAPIKey(onlyA)
	if !ok || !id.Restricted || id.Allows(a) || id.Allows(b) {
		t.Fatalf("only-a after delete = %+v %v", id, ok)
	}
	var grants []APIKeyDeployment
	st.db.Find(&grants)
	if len(grants) != 1 || grants[0].DeploymentID != b {
		t.Fatalf("grants = %+v", grants)
	}
}
