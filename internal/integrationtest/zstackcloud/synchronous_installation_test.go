package zstackcloud_test

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/provider/zstack/cloud/resourceclaim"
	"gpuworks.ai/gpuworks/internal/server/provision"
	"gpuworks.ai/gpuworks/internal/server/store"
	cloudstore "gpuworks.ai/gpuworks/internal/server/store/zstack/cloud"
)

func TestCloudInstallWaitsForSSHAndCleansUpFailure(t *testing.T) {
	for _, outcome := range []string{"success", "failure", "cancellation"} {
		t.Run(outcome, func(t *testing.T) {
			inventory := cloudInventoryFixture(t)
			st, err := store.Open(filepath.Join(t.TempDir(), "cloud.db"), bus.New())
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			box, err := auth.NewSecretbox("synchronous-installation-test")
			if err != nil {
				t.Fatal(err)
			}
			service := provider.NewService(st, box)
			entered, finish := make(chan struct{}), make(chan struct{})
			service.ConfigureDrivers(installerFunc(func(ctx context.Context, req provision.SSHInstallRequest) (provision.SSHInstallReport, error) {
				if req.Password != "host-password" || req.Token == "" || req.Target != "192.0.2.10" {
					t.Error("incorrect SSH installation settings")
				}
				close(entered)
				select {
				case <-ctx.Done():
					return provision.SSHInstallReport{}, ctx.Err()
				case <-finish:
				}
				if outcome == "failure" {
					return provision.SSHInstallReport{}, errors.New("remote failure with host-password")
				}
				return provision.SSHInstallReport{Checks: []provision.CheckResult{{ID: "agent_process", Status: "pass"}}}, nil
			}))
			p, err := service.Add(t.Context(), "zstack", "Cloud", provider.Credentials{Endpoint: inventory.URL, PublicKey: "test-access-key", PrivateKey: "test-access-secret"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := service.Sync(t.Context(), p.ID); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan struct{})
			var result provider.InstallResult
			var installErr error
			go func() {
				defer close(done)
				result, installErr = service.Install(ctx, p.ID, "host-a", "https://control.example", provider.InstallOptions{Password: "host-password"})
			}()
			select {
			case <-entered:
			case <-done:
				t.Fatalf("installation did not reach SSH: %v", installErr)
			case <-t.Context().Done():
				t.Fatal("SSH installation did not start")
			}
			select {
			case <-done:
				t.Fatal("installation returned before SSH finished")
			default:
			}
			if outcome == "cancellation" {
				cancel()
			} else {
				close(finish)
			}
			<-done
			nodes, err := st.ListNodes()
			if err != nil {
				t.Fatal(err)
			}
			instances, err := st.ProviderInstances(p.ID)
			if err != nil {
				t.Fatal(err)
			}
			if outcome != "success" {
				if installErr == nil || len(nodes) != 0 {
					t.Fatalf("failed install must return an error and remove its pending node: %v nodes=%d", installErr, len(nodes))
				}
				for _, instance := range instances {
					if instance.NodeID != 0 {
						t.Fatal("failed installation left a linked node")
					}
				}
				if outcome == "cancellation" && !errors.Is(installErr, context.Canceled) {
					t.Fatalf("cancellation lost: %v", installErr)
				}
				return
			}
			if installErr != nil || result.NodeUUID == "" || !slices.ContainsFunc(result.InstallChecks, func(c provision.CheckResult) bool { return c.ID == "agent_process" && c.Status == "pass" }) {
				t.Fatalf("installation result = %+v err=%v", result, installErr)
			}
			if len(nodes) != 1 || nodes[0].State != store.NodeStateConnecting {
				t.Fatal("SSH success must still wait for agent registration")
			}
			if _, err := resourceclaim.New(st, service).CloudResourceMutations(result.NodeUUID, []int{0}); err == nil {
				t.Fatal("resource reservation allowed before agent registration and reporting")
			}
			host, err := cloudstore.New(st.DB()).CloudHostByNodeUUID(result.NodeUUID)
			if err != nil || host.InitialCPUReserved == nil || *host.InitialCPUReserved != 2 {
				t.Fatalf("resource baseline lost: %+v err=%v", host, err)
			}
			_, credentials, err := st.GetNodeSSHCredential(result.NodeUUID)
			if err != nil || credentials.Password != "host-password" {
				t.Fatal("shared install flow did not preserve SSH recovery credentials")
			}
		})
	}
}
