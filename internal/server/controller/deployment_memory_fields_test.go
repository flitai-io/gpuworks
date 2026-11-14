package controller

import (
	"reflect"
	"testing"
)

// Every Controller field is either memory forgetDeployment drops or said here
// not to be kept by deployment. A new field fails this test until it is
// classified, so state kept by deployment cannot be added without its
// cleanup.
func TestEveryControllerFieldIsClassified(t *testing.T) {
	// Dropped by deploymentMemories (TestForgetDeploymentLeavesNoMemory).
	forgotten := map[string]bool{
		"pool": true, "waits": true, "placements": true, "sleepPushes": true,
		"drainClocks": true, "dgpuPushes": true, "imageWaits": true, "failover": true,
		"orders": true, // its placement hints; the order lock is not by deployment
	}
	notByDeployment := map[string]string{
		"cacheCopies":          "by deployment, but released by withdrawCacheCopies, which talks to nodes and is retried",
		"downloadUsers":        "by model, rebuilt from the store",
		"evictions":            "by model file",
		"modelSyncs":           "by model and node",
		"routes":               "events waiting for the next tick",
		"observation":          "by template",
		"peerCool":             "by node pair",
		"imageRefsNotify":      "a signal",
		"nodeStatusDirty":      "a flag",
		"enginePorts":          "configuration",
		"retryBackoff":         "configuration",
		"offlineGrace":         "configuration",
		"maxAttempts":          "configuration",
		"startTimeout":         "configuration",
		"watchingSince":        "when Run began",
		"store":                "a dependency",
		"networks":             "a dependency",
		"hub":                  "a dependency",
		"log":                  "a dependency",
		"servingDial":          "a dependency",
		"drainProbe":           "a dependency",
		"inferenceNodeHandler": "a dependency",
		"now":                  "the clock",
		"q":                    "the work queue",
		"cacheQ":               "the work queue",
		"syncMu":               "guards modelSyncs",
	}
	seen := map[string]bool{}
	ty := reflect.TypeOf(Controller{})
	for i := 0; i < ty.NumField(); i++ {
		name := ty.Field(i).Name
		seen[name] = true
		if !forgotten[name] && notByDeployment[name] == "" {
			t.Errorf("Controller.%s is not classified: if it is kept by deployment, add it to deploymentMemories and TestForgetDeploymentLeavesNoMemory; otherwise say why here", name)
		}
	}
	for name := range forgotten {
		if !seen[name] {
			t.Errorf("forgotten lists %s, which Controller no longer has", name)
		}
	}
	for name := range notByDeployment {
		if !seen[name] {
			t.Errorf("notByDeployment lists %s, which Controller no longer has", name)
		}
	}
}
