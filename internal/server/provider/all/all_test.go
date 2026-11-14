package all_test

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/provider"
	_ "gpuworks.ai/gpuworks/internal/server/provider/all"
)

// A kind says it connects Kubernetes exactly when its driver lists managed
// clusters, and instances exactly when it lists those: the dictionary and
// the drivers are written apart and must agree.
func TestKindResourcesMatchWhatTheDriversCanList(t *testing.T) {
	infos := provider.KindInfos()
	if len(infos) == 0 {
		t.Fatal("no provider driver is registered")
	}
	for _, info := range infos {
		kubernetes, instances := false, false
		for _, r := range info.Resources {
			if r.Type == provider.ResourceKubernetes {
				kubernetes = true
			} else {
				instances = true
			}
		}
		if kubernetes != info.CanListClusters {
			t.Errorf("%s: declares Kubernetes = %v, driver lists clusters = %v", info.Kind, kubernetes, info.CanListClusters)
		}
		if instances != info.Capabilities.CanListInstances {
			t.Errorf("%s: declares an instance resource = %v, driver lists instances = %v", info.Kind, instances, info.Capabilities.CanListInstances)
		}
	}
}
