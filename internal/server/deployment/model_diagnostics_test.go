package deployment

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/modelmeta"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestModelAdmissionReportsIndependentStorageFailures(t *testing.T) {
	r := newSaveToRig(t)
	r.model.Source = "local"
	r.model.SourceID = "registered-model"
	second, err := r.st.RegisterNode("gpu-b", store.HashToken("b"), "gpu-b")
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range []*store.Node{r.node, second} {
		if err := r.st.ApplyStorageStatus(node.ID, proto.StorageStatusReport{Storages: []proto.StorageStatus{{
			ID: r.nfs.ID, State: proto.StorageStateFailed, Reason: "not_mounted", Path: r.nfs.Path,
		}}}); err != nil {
			t.Fatal(err)
		}
	}
	r.h.d.FetchModelMeta = func(context.Context, string, string, string, string, []string) (*modelmeta.Meta, error) {
		t.Fatal("a registered local model has no public source to probe")
		return nil, nil
	}
	err = r.h.admitDeploymentModel(context.Background(), r.model, store.SourceRules{SaveTo: r.nfs.ID}, []*store.Node{r.node, second, r.node})
	var pe *messages.ProductError
	if !errors.As(err, &pe) || pe.Code() != messages.ErrorModelSourceUnavailable {
		t.Fatalf("admission error=%v", err)
	}
	out := refusalResponse(err)
	var response struct {
		Diagnostics *dto.DiagnosticReport `json:"diagnostics"`
	}
	if decodeErr := json.Unmarshal(out.Body.Bytes(), &response); decodeErr != nil {
		t.Fatal(decodeErr)
	}
	report := response.Diagnostics
	if report == nil {
		t.Fatalf("missing storage diagnostic report: %v", err)
	}
	failures := map[uint]bool{}
	ids := map[string]bool{}
	for _, check := range report.Checks {
		if ids[check.ID] {
			t.Fatalf("duplicate check ID %q", check.ID)
		}
		ids[check.ID] = true
		if check.Message.Code == messages.ErrorStorageUnavailableOnNode && check.Message.Params["reason"] == "not_mounted" && check.Status == "failed" {
			nodeID, ok := check.Message.Params["node_id"].(float64)
			if !ok {
				t.Fatalf("missing node identity in %+v", check)
			}
			failures[uint(nodeID)] = true
		}
	}
	if !failures[r.node.ID] || !failures[second.ID] {
		t.Fatalf("not all independent node failures were reported: %+v", report)
	}
	blocked := 0
	for _, check := range report.Checks {
		if check.Status != "blocked" {
			continue
		}
		blocked++
		if len(check.BlockedBy) == 0 {
			t.Fatalf("blocked check has no cause: %+v", check)
		}
		for _, dependency := range check.BlockedBy {
			if !ids[dependency] {
				t.Fatalf("unknown blocked dependency %q", dependency)
			}
		}
	}
	if blocked < 2 {
		t.Fatalf("model path checks must be blocked by access failures: %+v", report)
	}
}

func TestForecastPreservesStorageRefusalAndPinnedScope(t *testing.T) {
	r := newSaveToRig(t)
	r.nfsHoldsModel(t)
	r.nfsState(t, proto.StorageStateFailed, false)
	if _, err := r.st.UpsertModelFile(&store.ModelFile{ModelID: r.model.ID, NodeID: r.node.ID,
		State: proto.MFPresent, Path: "/models/elsewhere"}); err != nil {
		t.Fatal(err)
	}
	var fact dto.AcquisitionNode
	r.h.forecastWeights(&fact, r.node, r.model, store.SourceRules{Pinned: []uint{r.nfs.ID}}, r.h.networks().Resolve)
	if fact.WeightsPresent || fact.WeightsPlan == forecastDownload || fact.WeightsFetchError == nil {
		t.Fatalf("forecast ignored pinned storage refusal: %+v", fact)
	}
	if fact.WeightsFetchError.Params["reason"] != store.LocationStorageUnavailable {
		t.Fatalf("forecast lost source reason: %+v", fact.WeightsFetchError)
	}
}

func TestLocalPeerAdmissionMatchesForecast(t *testing.T) {
	r := newForecastRig(t)
	target := r.node("target", true, nil)
	peer := r.node("peer", true, nil)
	r.model.Source = "local"
	r.model.SourceID = "registered-model"
	r.holdsModel(peer)
	r.h.d.FetchModelMeta = func(context.Context, string, string, string, string, []string) (*modelmeta.Meta, error) {
		t.Fatal("local peer acquisition must not fetch public metadata")
		return nil, nil
	}
	if got := r.forecast(target.UUID); got.WeightsPlan != forecastCopy {
		t.Fatalf("expected qualified peer candidate, got %+v", got)
	}
	if err := r.h.admitDeploymentModel(context.Background(), r.model, store.SourceRules{}, []*store.Node{target}); err != nil {
		t.Fatalf("a qualified local peer path was rejected: %v", err)
	}
	r.online[peer.UUID] = false
	if err := r.h.admitDeploymentModel(context.Background(), r.model, store.SourceRules{}, []*store.Node{target}); err == nil {
		t.Fatal("an offline peer was admitted as an available acquisition path")
	}
	r.online[peer.UUID] = true
	r.deleteDefaultGroup()
	if err := r.h.admitDeploymentModel(context.Background(), r.model, store.SourceRules{}, []*store.Node{target}); err == nil {
		t.Fatal("a peer outside every shared sync group was admitted")
	}
}

func TestCachedModelDiagnosticsDoNotRequireSharedMount(t *testing.T) {
	r := newForecastRig(t)
	node := r.node("cached", true, nil)
	cache, err := r.st.CreateStorage(&store.Storage{Name: "cached-models", Type: proto.StorageNFSCache,
		Enabled: true, AllNodes: true, Path: "/mnt/shared", CachePath: "/models/cache", Export: "nfs.example:/models"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.st.ApplyStorageStatus(node.ID, proto.StorageStatusReport{Storages: []proto.StorageStatus{{
		ID: cache.ID, State: proto.StorageStateFailed, Reason: "not_mounted", Path: cache.Path,
	}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.st.UpsertModelFile(&store.ModelFile{ModelID: r.model.ID, StorageID: cache.ID,
		NodeID: node.ID, State: proto.MFPresent, Path: "/models/cache/org/model"}); err != nil {
		t.Fatal(err)
	}
	report, err := r.h.inspectDeploymentModel(context.Background(), r.model, store.SourceRules{}, []*store.Node{node}, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, check := range report.Checks {
		if check.Status != "passed" {
			t.Fatalf("a usable local cache incorrectly depends on its shared mount: %+v", check)
		}
	}
}

func TestModelMetadataFailureIsSharedAndScoped(t *testing.T) {
	r := newForecastRig(t)
	a, b := r.node("a", true, nil), r.node("b", true, nil)
	reads := 0
	r.h.d.FetchModelMeta = func(context.Context, string, string, string, string, []string) (*modelmeta.Meta, error) {
		reads++
		return nil, fmt.Errorf("private source address with credentials: %w", context.DeadlineExceeded)
	}
	err := r.h.admitDeploymentModel(context.Background(), r.model, store.SourceRules{}, []*store.Node{a, b})
	if !errors.Is(err, context.DeadlineExceeded) || reads != 1 {
		t.Fatalf("reads=%d error=%v", reads, err)
	}
	report := diagnosticsFromError(err)
	metadata := 0
	for _, check := range report.Checks {
		if check.Message.Params["observer"] == "management_node" {
			metadata++
			if check.Status != "failed" || check.Message.Params["reason"] != "source_timeout" {
				t.Fatalf("unexpected metadata check: %+v", check)
			}
		}
	}
	if metadata != 1 {
		t.Fatalf("metadata checks=%d report=%+v", metadata, report)
	}
	blocked := 0
	for _, check := range report.Checks {
		if check.Status == "blocked" {
			blocked++
		}
	}
	if blocked != 2 {
		t.Fatalf("node acquisition checks were not linked to their shared metadata failure: %+v", report)
	}
	out := refusalResponse(err)
	if strings.Contains(out.Body.String(), "credentials") {
		t.Fatalf("raw metadata cause escaped to response: %s", out.Body.String())
	}
}

func TestModelFactsFailureIsNotAnEmptyCandidateList(t *testing.T) {
	r := newSaveToRig(t)
	if err := r.st.Close(); err != nil {
		t.Fatal(err)
	}
	_, _, err := r.h.storageFacts(r.model, r.node)
	if err == nil {
		t.Fatal("database read failure became an empty successful candidate list")
	}
	out := refusalResponse(err)
	var response struct {
		Code        string                `json:"code"`
		Diagnostics *dto.DiagnosticReport `json:"diagnostics"`
	}
	if err := json.Unmarshal(out.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Code != messages.ErrorModelSourceUnavailable || response.Diagnostics == nil ||
		response.Diagnostics.Checks[0].Status != "unknown" || response.Diagnostics.Checks[0].Message.Params["reason"] != "facts_unavailable" {
		t.Fatalf("unexpected facts failure response: %s", out.Body.String())
	}
	if strings.Contains(out.Body.String(), "sql:") || strings.Contains(out.Body.String(), "database is closed") {
		t.Fatalf("internal query detail escaped to response: %s", out.Body.String())
	}
}

func TestMetadataFailureClassificationUsesTypedCauses(t *testing.T) {
	for _, test := range []struct {
		name   string
		cause  error
		reason string
	}{
		{"timeout", context.DeadlineExceeded, "source_timeout"},
		{"cancelled", context.Canceled, "source_cancelled"},
		{"dns", &net.DNSError{Name: "hidden.example", IsNotFound: true}, "source_dns_failed"},
		{"tls", x509.UnknownAuthorityError{}, "source_tls_failed"},
		{"connect", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("refused")}, "source_connect_failed"},
		{"auth", modelmeta.ErrTokenRejected, "source_authentication"},
		{"unknown", errors.New("dns/auth/timeout words in an untyped error"), "source_unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := metadataFailureReason(fmt.Errorf("wrapped: %w", test.cause)); got != test.reason {
				t.Fatalf("reason=%q want %q", got, test.reason)
			}
		})
	}
}

func TestLocalPeerAdmissionRequiresPermittedStoragePath(t *testing.T) {
	r := newForecastRig(t)
	target := r.node("target", true, nil)
	peer := r.node("peer", true, nil)
	r.model.Source, r.model.SourceID = "local", "registered-model"
	r.holdsModel(peer)
	if err := r.st.SetNodeManagementRolesOff(target.UUID, []string{netplan.RoleStorageSync}); err != nil {
		t.Fatal(err)
	}
	if err := r.h.admitDeploymentModel(context.Background(), r.model, store.SourceRules{}, []*store.Node{target}); err == nil {
		t.Fatal("same-group peer was admitted although storage sync is forbidden")
	}
	if got := r.forecast(target.UUID); got.WeightsPlan == forecastCopy || got.WeightsFetchError == nil {
		t.Fatalf("forbidden copy was forecast as executable: %+v", got)
	}
}

func TestModelPeerSkipsBlockedPathAndUsesNextCandidate(t *testing.T) {
	r := newForecastRig(t)
	target := r.node("target", true, nil)
	first := r.node("first", true, nil)
	second := r.node("second", true, nil)
	r.holdsModel(first)
	r.holdsModel(second)
	if err := r.st.SetNodeManagementRolesOff(first.UUID, []string{netplan.RoleStorageSync}); err != nil {
		t.Fatal(err)
	}
	peer, err := r.h.modelCopyPeer(r.model.ID, target)
	if err != nil || peer != second.UUID {
		t.Fatalf("peer=%q err=%v; want the candidate with a permitted storage path", peer, err)
	}
}

func TestModelStorageDiagnosticsKeepPendingAndUnknownStates(t *testing.T) {
	for _, test := range []struct {
		state  string
		status string
	}{
		{proto.StorageStateReady, "passed"},
		{proto.StorageStateMounting, "pending"},
		{proto.StorageStateFailed, "failed"},
		{proto.StorageStateUnresponsive, "failed"},
		{proto.StorageStateUnsupported, "failed"},
		{"future-state", "unknown"},
		{"", "unknown"},
	} {
		name := test.state
		if name == "" {
			name = "empty"
		}
		t.Run(name, func(t *testing.T) {
			r := newSaveToRig(t)
			r.nfsState(t, test.state, false)
			if _, err := r.st.UpsertModelFile(&store.ModelFile{ModelID: r.model.ID, StorageID: r.nfs.ID,
				State: proto.MFDownloading, Path: "org/model"}); err != nil {
				t.Fatal(err)
			}
			report, err := r.h.inspectDeploymentModel(context.Background(), r.model, store.SourceRules{SaveTo: r.nfs.ID}, []*store.Node{r.node}, false)
			if err != nil {
				t.Fatalf("a shared write may be awaited: %v", err)
			}
			found := false
			for _, check := range report.Checks {
				if check.Message.Code == messages.ErrorStorageUnavailableOnNode {
					found = true
					if check.Status != test.status || check.Message.Params["state"] != test.state {
						t.Fatalf("storage check=%+v; want %s for state %q", check, test.status, test.state)
					}
				}
			}
			if !found {
				t.Fatal("missing shared-storage observation")
			}
		})
	}
}

func TestDisabledStorageStillReportsReadableExistingModel(t *testing.T) {
	r := newSaveToRig(t)
	r.nfsState(t, proto.StorageStateReady, false)
	r.nfsHoldsModel(t)
	enabled := false
	if _, err := r.st.UpdateStorage(r.nfs.ID, store.StoragePatch{Enabled: &enabled}); err != nil {
		t.Fatal(err)
	}
	report, err := r.h.inspectDeploymentModel(context.Background(), r.model, store.SourceRules{SaveTo: r.nfs.ID}, []*store.Node{r.node}, false)
	if err != nil {
		t.Fatalf("read-only storage must still serve its existing model: %v", err)
	}
	for _, check := range report.Checks {
		if check.Status != "passed" {
			t.Fatalf("read-only was confused with inaccessible storage: %+v", check)
		}
	}
}

func TestPeerNetworkPolicyReasonSurvivesAdmissionResponse(t *testing.T) {
	r := newForecastRig(t)
	target := r.node("target", true, nil)
	peer := r.node("peer", true, nil)
	r.model.Source, r.model.SourceID = "local", "registered-model"
	r.holdsModel(peer)
	if err := r.st.SetNodeManagementRolesOff(target.UUID, []string{netplan.RoleStorageSync}); err != nil {
		t.Fatal(err)
	}
	err := r.h.admitDeploymentModel(context.Background(), r.model, store.SourceRules{}, []*store.Node{target})
	out := refusalResponse(err)
	var response struct {
		Code        string                `json:"code"`
		Params      map[string]any        `json:"params"`
		Diagnostics *dto.DiagnosticReport `json:"diagnostics"`
	}
	if err := json.Unmarshal(out.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Code != messages.ErrorModelSourceUnavailable || response.Params["reason"] != "no_storage_path" || response.Diagnostics == nil {
		t.Fatalf("known peer policy was collapsed into missing model: %s", out.Body.String())
	}
	policyID := ""
	for _, check := range response.Diagnostics.Checks {
		if check.Message.Code != messages.ErrorNoStoragePath {
			continue
		}
		policyID = check.ID
		if check.Status != "failed" || check.Message.Params["source"] != peer.DisplayName()+" ("+peer.UUID+")" || check.Message.Params["source_node"] != peer.UUID || check.Message.Params["node_uuid"] != target.UUID {
			t.Fatalf("peer policy is not actionable: %+v", check)
		}
	}
	if policyID == "" {
		t.Fatalf("missing typed network-policy check: %s", out.Body.String())
	}
	blocked := false
	for _, check := range response.Diagnostics.Checks {
		if check.Status == "blocked" && len(check.BlockedBy) == 1 && check.BlockedBy[0] == policyID {
			blocked = true
		}
	}
	if !blocked {
		t.Fatalf("acquisition did not retain its network prerequisite: %s", out.Body.String())
	}
	if got := r.forecast(target.UUID); got.WeightsPlan != forecastUnavailable || got.WeightsFetchError == nil || got.WeightsFetchError.Code != messages.ErrorNoStoragePath || got.WeightsFetchError.Params["reason"] != "no_storage_path" {
		t.Fatalf("forecast lost known network policy: %+v", got)
	}
}

func TestPeerNetworkPolicyDoesNotBlockPublicAlternative(t *testing.T) {
	r := newForecastRig(t)
	target := r.node("target", true, nil)
	peer := r.node("peer", true, nil)
	r.holdsModel(peer)
	if err := r.st.SetNodeManagementRolesOff(target.UUID, []string{netplan.RoleStorageSync}); err != nil {
		t.Fatal(err)
	}
	report, err := r.h.inspectDeploymentModel(context.Background(), r.model, store.SourceRules{}, []*store.Node{target}, false)
	if err != nil {
		t.Fatalf("an unused peer path blocked public download: %v", err)
	}
	found := false
	for _, check := range report.Checks {
		if check.Message.Code == messages.ErrorNoStoragePath {
			found = true
			if check.Status != "not_applicable" {
				t.Fatalf("unselected peer path became an operation failure: %+v", check)
			}
		}
	}
	if !found {
		t.Fatalf("unused peer-path policy became invisible: %+v", report)
	}
	if got := r.forecast(target.UUID); got.WeightsPlan != forecastDownload || got.WeightsFetchError != nil {
		t.Fatalf("usable public alternative was marked unavailable: %+v", got)
	}
}

// refusalResponse is a refused use case in the shape the API answers it
// with (api.writeErr): the error text, its product code and params, and the
// diagnostics it carries.
func refusalResponse(err error) *httptest.ResponseRecorder {
	body := map[string]any{"error": err.Error()}
	if report := diagnosticsFromError(err); report != nil {
		body["diagnostics"] = report
	}
	var product *messages.ProductError
	if errors.As(err, &product) {
		body["code"] = product.Code()
		if params := product.Params(); len(params) > 0 {
			body["params"] = params
		}
	}
	rec := httptest.NewRecorder()
	_ = json.NewEncoder(rec).Encode(body)
	return rec
}
