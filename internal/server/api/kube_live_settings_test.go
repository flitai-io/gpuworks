package api

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/auth"
)

// The idle return, the RuntimeClass and the engine ports are handed to the
// entry's agents by the server, so an edit of them needs no apply: the answer
// says nothing is to be re-applied and gives no commands, and the entry's
// online agents are pushed once per edit.
func TestEditingLiveSettingsNeedsNoReapply(t *testing.T) {
	rig := newTokenRig(t)
	signer, err := auth.NewTokenSigner("platform-token-test-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var pushed []uint
	rig.h = Router(Deps{AuthSigner: signer, Store: rig.st, AI: &fakeAI{available: true}, SchedulingActivity: replayActivity{rig.st},
		NodeSync: nodeSyncCallbacks{kube: func(id uint) { pushed = append(pushed, id) }}})

	created := decodeKube[map[string]any](t, rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters", kubeCreateBody))
	base := "/api/v1/kube/clusters/" + created["uuid"].(string)
	manifestBefore := rig.do(rig.session, http.MethodGet, base+"/manifest", "").Body.String()

	for _, tc := range []struct{ body, changed string }{
		{`{"idle_return_seconds":-1}`, "[idle_return_seconds]"},
		{`{"runtime_class":"nvidia-legacy"}`, "[runtime_class]"},
		{`{"engine_ports":"41000-41999"}`, "[engine_ports]"},
		{`{"idle_return_seconds":300,"runtime_class":"","engine_ports":"40000-42767"}`, "[engine_ports idle_return_seconds runtime_class]"},
	} {
		before := len(pushed)
		rec := rig.do(rig.session, http.MethodPut, base, tc.body)
		got := decodeKube[map[string]any](t, rec)
		if rec.Code != http.StatusOK || got["reapply"] != false || fmt.Sprint(got["changed"]) != tc.changed || len(got["commands"].([]any)) != 0 {
			t.Fatalf("%s = %d %s", tc.body, rec.Code, rec.Body)
		}
		if len(pushed) != before+1 {
			t.Fatalf("%s: agents pushed %d times, want once", tc.body, len(pushed)-before)
		}
		if manifest := rig.do(rig.session, http.MethodGet, base+"/manifest", "").Body.String(); manifest != manifestBefore {
			t.Fatalf("%s changed the manifest:\n%s", tc.body, manifest)
		}
	}
	if pushed[0] == 0 {
		t.Fatal("pushed an entry without an id")
	}

	// An edit of anything else, or none at all, pushes nothing; a setting
	// that does not validate is refused before anything changes.
	before := len(pushed)
	for _, body := range []string{`{"name":"prod"}`, `{"priority":500}`, `{"runtime_class":"No_Class"}`, `{"engine_ports":"x"}`, `{"idle_return_seconds":-2}`} {
		rig.do(rig.session, http.MethodPut, base, body)
	}
	if len(pushed) != before {
		t.Fatalf("pushed %d times for edits that change no live setting", len(pushed)-before)
	}
	// A manifest setting still needs the apply.
	got := decodeKube[map[string]any](t, rig.do(rig.session, http.MethodPut, base, `{"node_selector":"gpuworks.ai/pool=b"}`))
	if got["reapply"] != true {
		t.Fatalf("selector edit = %v", got)
	}
}
