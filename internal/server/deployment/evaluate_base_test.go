package deployment

import (
	"path/filepath"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestEvaluationBaseDistinguishesMissingFromStoreFailure(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "evaluate.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s := New(Deps{Store: st})
	for _, id := range []uint{0, 98765} {
		base, err := s.evaluationLaunchBase(id)
		if err != nil || base != nil {
			t.Fatalf("missing %d = %+v, %v", id, base, err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.evaluationLaunchBase(98765); err == nil {
		t.Fatal("closed store was silently treated as a missing deployment")
	} else if KindOf(internal(err)) != KindInternal {
		t.Fatal("store failure must remain an internal error")
	}
}
