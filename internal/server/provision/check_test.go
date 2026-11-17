package provision

import (
	"context"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// The wire form of one onboarding check (ADR 0067): id, status, code,
// params, the English fallback and the evidence, under exactly these keys.
// The keys of the old shape (name, detail) are gone.
func TestCheckResultWireJSON(t *testing.T) {
	check := WarnCheck(CheckIDInstallDirSpace, messages.CheckInstallDirSpaceLow, "12.0 GiB free of 40.0 GiB; images need 50.0 GiB",
		map[string]any{"available": "12.0 GiB", "total": "40.0 GiB", "minimum": "50.0 GiB"}).WithEvidence("df: /data 12582912 41943040")
	raw, err := json.Marshal(check)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"id":"install_dir_space","status":"warn","code":"CHECK_INSTALL_DIR_SPACE_LOW","params":{"available":"12.0 GiB","minimum":"50.0 GiB","total":"40.0 GiB"},"message":"12.0 GiB free of 40.0 GiB; images need 50.0 GiB","evidence":"df: /data 12582912 41943040"}`
	if string(raw) != want {
		t.Fatalf("wire = %s\nwant  %s", raw, want)
	}
	var back CheckResult
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, check) {
		t.Fatalf("round trip = %+v, want %+v", back, check)
	}

	// Params travel as text: numbers in decimal, booleans as true/false.
	raw, _ = json.Marshal(PassCheck(CheckIDServerDialPort, messages.CheckServerDialPortChosen, "m", map[string]any{"port": 18800, "opened": true, "bytes": int64(7)}))
	if string(raw) != `{"id":"server_dial_port","status":"pass","code":"CHECK_SERVER_DIAL_PORT_CHOSEN","params":{"bytes":"7","opened":"true","port":"18800"},"message":"m"}` {
		t.Fatalf("typed params = %s", raw)
	}

	// A bare pass carries only what it has.
	raw, _ = json.Marshal(PassCheck(CheckIDGPU, "", "", nil).WithEvidence("  "))
	if string(raw) != `{"id":"gpu","status":"pass"}` {
		t.Fatalf("bare pass = %s", raw)
	}
}

// Every constructor yields one of the five statuses and nothing else.
func TestCheckConstructorsUseTheFiveStatuses(t *testing.T) {
	for want, check := range map[string]CheckResult{
		proto.CheckPass: PassCheck("x", "", "", nil),
		proto.CheckWarn: WarnCheck("x", "C", "", nil),
		proto.CheckFail: FailCheck("x", "C", "", nil),
		proto.CheckSkip: SkipCheck("x", "C", "", nil),
		proto.CheckNA:   NACheck("x", "C", "", nil),
	} {
		if check.Status != want || !proto.ValidCheckStatus(check.Status) {
			t.Fatalf("status = %q, want %q", check.Status, want)
		}
	}
}

func TestWorstStatusRanksSkipAbovePass(t *testing.T) {
	for _, tc := range []struct {
		statuses []string
		want     string
	}{
		{nil, proto.CheckNA},
		{[]string{proto.CheckNA, proto.CheckPass}, proto.CheckPass},
		{[]string{proto.CheckPass, proto.CheckSkip}, proto.CheckSkip},
		{[]string{proto.CheckSkip, proto.CheckWarn}, proto.CheckWarn},
		{[]string{proto.CheckWarn, proto.CheckFail, proto.CheckPass}, proto.CheckFail},
		{[]string{proto.CheckPass, "ok"}, proto.CheckSkip},
	} {
		checks := make([]CheckResult, len(tc.statuses))
		for i, status := range tc.statuses {
			checks[i] = CheckResult{ID: "x", Status: status}
		}
		if got := WorstStatus(checks); got != tc.want {
			t.Fatalf("WorstStatus(%v) = %q, want %q", tc.statuses, got, tc.want)
		}
	}
}

// A command check that answered is judged on its exit status; one whose
// session never finished is skipped, not blamed on the node.
func TestCommandCheckSkipsWhatDidNotRun(t *testing.T) {
	host, port := startFakeSSHServer(t, "root", "pw", func(cmd string, _ io.Reader) (string, string, uint32) {
		if strings.Contains(cmd, "hang") {
			time.Sleep(2 * time.Second)
		}
		return "no supported GPU CLI found\n", "", 1
	})
	client := dialFakeSSH(t, host, port, "root", "pw")
	command := precheckCommand{id: CheckIDGPU, command: "probe", required: true, failCode: messages.CheckGPUToolMissing, failMessage: "no supported GPU CLI found"}
	got := checkCommandContext(t.Context(), client, command)
	if got.Status != proto.CheckFail || got.Code != messages.CheckGPUToolMissing || !strings.Contains(got.Evidence, "no supported GPU CLI found") || !strings.Contains(got.Evidence, "exited with status 1") {
		t.Fatalf("required = %+v", got)
	}
	command.required = false
	if got := checkCommandContext(t.Context(), client, command); got.Status != proto.CheckWarn || got.Code != messages.CheckGPUToolMissing {
		t.Fatalf("advisory = %+v", got)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	command.command = "hang"
	if got := checkCommandContext(ctx, client, command); got.Status != proto.CheckSkip || got.Code != messages.CheckCommandNotRun || !strings.Contains(got.Evidence, "deadline") {
		t.Fatalf("timed out = %+v", got)
	}
}
