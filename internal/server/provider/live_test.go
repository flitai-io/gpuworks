package provider_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/provider"
	_ "gpuworks.ai/gpuworks/internal/server/provider/all"
)

// TestRunpodLive hits the real RunPod API when RUNPOD_KEY is set (skipped in CI).
// It validates the whole provider chain against a live account.
func TestRunpodLive(t *testing.T) {
	key := os.Getenv("RUNPOD_KEY")
	if key == "" {
		t.Skip("RUNPOD_KEY not set; skipping live test")
	}
	drv, ok := provider.Get("runpod")
	if !ok {
		t.Fatal("runpod driver not registered")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	creds := provider.Credentials{APIKey: key}

	if err := drv.ValidateCredentials(ctx, creds); err != nil {
		t.Fatalf("ValidateCredentials failed: %v", err)
	}
	insts, err := drv.ListInstances(ctx, creds)
	if err != nil {
		t.Fatalf("ListInstances failed: %v", err)
	}
	t.Logf("runpod returned %d instance(s)", len(insts))
	for _, in := range insts {
		t.Logf("  %s name=%q gpu=%dx%s power=%s price=%.2f/hr kind=%s ip=%s:%d",
			in.ExternalID, in.Name, in.GPUCount, in.GPUModel, in.Power, in.PricePerHr, in.Kind, in.PublicIP, in.SSHPort)
	}
}

// TestAliyunLive lists the account's GPU instances through every region when
// ALIYUN_ACCESS_KEY_ID and ALIYUN_ACCESS_KEY_SECRET are set (skipped in CI);
// ALIYUN_ROLE_ARN adds the RAM-role path and ALIYUN_REGIONS limits the scan.
// ALIYUN_COMMAND_INSTANCE=<region>/<instance-id> also runs the install
// precheck on that instance through Cloud Assistant (read-only), which
// exercises the RunCommand permissions.
//
//	ALIYUN_ACCESS_KEY_ID=… ALIYUN_ACCESS_KEY_SECRET=… go test ./internal/server/provider/ -run TestAliyunLive -v
func TestAliyunLive(t *testing.T) {
	id, secret := os.Getenv("ALIYUN_ACCESS_KEY_ID"), os.Getenv("ALIYUN_ACCESS_KEY_SECRET")
	if id == "" || secret == "" {
		t.Skip("ALIYUN_ACCESS_KEY_ID / ALIYUN_ACCESS_KEY_SECRET not set; skipping live test")
	}
	drv, ok := provider.Get("aliyun")
	if !ok {
		t.Fatal("aliyun driver not registered")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	creds := provider.Credentials{PublicKey: id, PrivateKey: secret, Extra: map[string]string{
		"role_arn": os.Getenv("ALIYUN_ROLE_ARN"), "regions": os.Getenv("ALIYUN_REGIONS"),
	}}
	if err := drv.ValidateCredentials(ctx, creds); err != nil {
		t.Fatalf("ValidateCredentials failed: %v", err)
	}
	start := time.Now()
	insts, err := drv.ListInstances(ctx, creds)
	if err != nil {
		t.Fatalf("ListInstances failed: %v", err)
	}
	t.Logf("aliyun returned %d instance(s) in %s", len(insts), time.Since(start).Round(time.Millisecond))
	for _, in := range insts {
		t.Logf("  %s %s name=%q gpu=%dx%s power=%s public=%s private=%s connections=%d", in.Region, in.ExternalID, in.Name, in.GPUCount, in.GPUModel, in.Power, in.PublicIP, in.PrivateIP, len(in.SSHConnections))
	}
	target := os.Getenv("ALIYUN_COMMAND_INSTANCE")
	if target == "" {
		return
	}
	region, id, ok := strings.Cut(target, "/")
	if !ok {
		t.Fatalf("ALIYUN_COMMAND_INSTANCE = %q, want <region>/<instance-id>", target)
	}
	prechecker, ok := drv.(provider.CommandPrechecker)
	if !ok {
		t.Fatal("aliyun driver does not precheck through Cloud Assistant")
	}
	checks, err := prechecker.PrecheckCommand(ctx, creds, provider.Instance{ExternalID: id, Region: region},
		provider.SSHConnection{ID: provider.ConnectionCloudAssistant, Type: provider.ConnectionCloudAssistant}, provider.CommandPrecheckRequest{ConnectMode: "agent_dial"})
	if err != nil {
		t.Fatalf("precheck through Cloud Assistant: %v", err)
	}
	for _, check := range checks {
		t.Logf("  precheck %s = %s %s: %s | %s", check.ID, check.Status, check.Code, check.Message, check.Evidence)
		if check.ID == "cloud_assistant" && check.Status != "pass" {
			t.Fatalf("Cloud Assistant check failed: %+v", check)
		}
	}
}
