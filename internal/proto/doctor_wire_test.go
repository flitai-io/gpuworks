package proto

import (
	"encoding/json"
	"reflect"
	"testing"
)

// The runtime-setup and filesystem parts of the doctor report keep these
// exact keys on the wire: the UI reads them by name.
func TestDoctorReportRuntimeAndFilesystemWireForm(t *testing.T) {
	const wire = `{"report":"text\n",` +
		`"host":{"hostname":"pod","os":"linux","arch":"amd64","kernel":"5.15","cpu_model":"EPYC","cpu_cores":8,"mem_total_bytes":1,"mem_avail_bytes":1,"disks":[` +
		`{"kind":"root","path":"/","total_bytes":100,"avail_bytes":50,"mount":{"point":"/","fs_type":"overlay","source":"overlay","options":"rw,relatime","ephemeral":true}},` +
		`{"kind":"udocker","path":"/workspace/udocker","total_bytes":100,"avail_bytes":90,"mount":{"point":"/workspace","fs_type":"nfs4","source":"10.1.2.3:/volumes/abcd","options":"rw,relatime,vers=4.1","network":true,"noexec":true,"read_only":true}}]},` +
		`"findings":[` +
		`{"id":"runtime_provision","status":"fail","code":"RUNTIME_PROVISION_FAILED","probe":"set up the udocker runtime","evidence":[{"name":"error","value":"prepare udocker ownership: exit status 1"},{"name":"failed_at","value":"2026-10-10T08:30:05Z"}],"recheck":"findmnt -T /workspace/udocker"},` +
		`{"id":"path_exec","status":"fail","code":"PATH_NOEXEC","disk":{"kind":"udocker","path":"/workspace/udocker","total_bytes":100,"avail_bytes":90,"mount":{"point":"/workspace","fs_type":"nfs4","noexec":true}},"probe":"/proc/self/mountinfo","evidence":[{"name":"TARGET","value":"/workspace"}],"recheck":"findmnt -T /workspace/udocker"},` +
		`{"id":"path_write","status":"fail","code":"PATH_READ_ONLY"},{"id":"path_persist","status":"warn","code":"PATH_EPHEMERAL"},{"id":"mounts","status":"skip","code":"MOUNT_TABLE_UNREADABLE","evidence":[{"name":"error","value":"permission denied"}]}],` +
		`"facts":[{"code":"runtime_identity","value":"dedicated user udocker (uid 1001)","identity":{"runtime":"runveil","user":"udocker","uid":1001}},` +
		`{"code":"network_filesystem","value":"/workspace is nfs4","disks":[{"kind":"models","path":"/workspace/models","total_bytes":100,"avail_bytes":90,"mount":{"point":"/workspace","fs_type":"nfs4","network":true}}]}]}`

	var report DoctorReport
	if err := json.Unmarshal([]byte(wire), &report); err != nil {
		t.Fatal(err)
	}
	root, udocker := report.Host.Disks[0].Mount, report.Host.Disks[1].Mount
	if root == nil || *root != (DoctorMount{Point: "/", FSType: "overlay", Source: "overlay", Options: "rw,relatime", Ephemeral: true}) {
		t.Fatalf("root mount = %+v", root)
	}
	if udocker == nil || *udocker != (DoctorMount{Point: "/workspace", FSType: "nfs4", Source: "10.1.2.3:/volumes/abcd", Options: "rw,relatime,vers=4.1", Network: true, NoExec: true, ReadOnly: true}) {
		t.Fatalf("udocker mount = %+v", udocker)
	}
	codes := []string{}
	for _, f := range report.Findings {
		codes = append(codes, f.Code)
	}
	if !reflect.DeepEqual(codes, []string{DoctorRuntimeProvisionFailed, DoctorPathNoExec, DoctorPathReadOnly, DoctorPathEphemeral, DoctorSkipMountTableUnreadable}) ||
		report.Facts[0].Code != DoctorFactRuntimeIdentity || report.Facts[1].Code != DoctorFactNetworkFilesystem {
		t.Fatalf("codes = %v facts = %+v", codes, report.Facts)
	}
	if id := report.Facts[0].Identity; id == nil || *id != (DoctorRunIdentity{Runtime: "runveil", User: "udocker", UID: 1001}) {
		t.Fatalf("identity = %+v", id)
	}
	if disks := report.Facts[1].Disks; len(disks) != 1 || disks[0].Path != "/workspace/models" || !disks[0].Mount.Network {
		t.Fatalf("network disks = %+v", disks)
	}
	if report.Findings[1].Disk == nil || !report.Findings[1].Disk.Mount.NoExec {
		t.Fatalf("path_noexec disk = %+v", report.Findings[1].Disk)
	}

	// Re-encoding reproduces the document key for key.
	out, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var want, got any
	if err := json.Unmarshal([]byte(wire), &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip changed the document:\n got %s\nwant %s", out, wire)
	}
}

// A report from an agent that predates mounts decodes with none, and a disk
// without one encodes without the key.
func TestDoctorDiskWithoutMountKeepsItsOldWireForm(t *testing.T) {
	const old = `{"kind":"root","path":"/","total_bytes":100,"avail_bytes":5}`
	var disk DoctorDisk
	if err := json.Unmarshal([]byte(old), &disk); err != nil || disk.Mount != nil {
		t.Fatalf("disk = %+v err=%v", disk, err)
	}
	out, err := json.Marshal(disk)
	if err != nil || string(out) != old {
		t.Fatalf("encoded = %s err=%v, want %s", out, err, old)
	}
}

// The untraversable-ancestor finding keeps its code string and evidence keys
// on the wire: the node page interpolates its sentence from them by name.
func TestDoctorRuntimeStateUntraversableWireForm(t *testing.T) {
	if DoctorRuntimeStateUntraversable != "RUNTIME_STATE_UNTRAVERSABLE" {
		t.Fatalf("code = %q", DoctorRuntimeStateUntraversable)
	}
	const wire = `{"id":"runtime_state_access","status":"fail","code":"RUNTIME_STATE_UNTRAVERSABLE","probe":"search permission on every directory above /root/gpuworks for udocker (uid 1001, gid 1001)",` +
		`"evidence":[{"name":"directory","value":"/root"},{"name":"mode","value":"0700"},{"name":"owner","value":"0:0"},{"name":"user","value":"udocker"},` +
		`{"name":"state_dir","value":"/root/gpuworks"},{"name":"fix","value":"chmod o+x /root"}],"recheck":"namei -m /root/gpuworks"}`
	var f DoctorFinding
	if err := json.Unmarshal([]byte(wire), &f); err != nil {
		t.Fatal(err)
	}
	if f.Code != DoctorRuntimeStateUntraversable || f.ID != DoctorCheckRuntimeStateAccess || f.Status != CheckFail || len(f.Evidence) != 6 || f.Evidence[5] != (DoctorField{Name: "fix", Value: "chmod o+x /root"}) {
		t.Fatalf("finding = %+v", f)
	}
	out, err := json.Marshal(f)
	if err != nil || string(out) != wire {
		t.Fatalf("encoded = %s err=%v, want %s", out, err, wire)
	}
}

// A pending runtime and an agent-own identity keep their wire form: pending
// only when set, uid always (0 is root), no user for the agent's own.
func TestDoctorRunIdentityWireForm(t *testing.T) {
	for _, tc := range []struct {
		identity DoctorRunIdentity
		wire     string
	}{
		{DoctorRunIdentity{Runtime: "runveil", Pending: true}, `{"runtime":"runveil","pending":true,"uid":0}`},
		{DoctorRunIdentity{Runtime: "runveil", Fallback: "no python"}, `{"runtime":"runveil","uid":0,"fallback":"no python"}`},
	} {
		out, err := json.Marshal(tc.identity)
		if err != nil || string(out) != tc.wire {
			t.Fatalf("encoded = %s err=%v, want %s", out, err, tc.wire)
		}
	}
}
