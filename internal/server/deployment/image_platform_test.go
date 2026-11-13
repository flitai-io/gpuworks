package deployment

import (
	"errors"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestDeploymentImageTargetExecutionOS(t *testing.T) {
	for _, tc := range []struct{ runtime, hostOS, want string }{
		{"docker", "darwin", "linux"}, {"nerdctl", "linux", "linux"}, {proto.RuntimeDriverHost, "darwin", "darwin"},
	} {
		got := deploymentImageTarget("vllm", "", &store.Node{RuntimeDriver: tc.runtime, OS: tc.hostOS})
		if got.PlatformOS != tc.want {
			t.Fatalf("runtime %s on %s: %+v", tc.runtime, tc.hostOS, got)
		}
	}
}

func TestImageAdmissionChecksEveryTargetNode(t *testing.T) {
	const image = "catalog/arm:v1"
	rig := newCompatTemplateRig(t, "docker", func(spec *store.InferenceSpecRow) { spec.Image = image })
	if _, err := rig.st.UpsertImage(&store.Image{Engine: "vllm", Framework: "cuda", Arch: "arm64", ImageRef: image, Source: "custom", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	nodes := []*store.Node{{UUID: "leader", Arch: "arm64"}, {UUID: "follower", Arch: "amd64"}}
	err := rig.h.admitImagePlatforms("vllm", templateSelection{}, image, nodes)
	var product *messages.ProductError
	if !errors.As(err, &product) || product.Code() != messages.WarningImageOverrideIncompatible || product.Params()["node"] != "follower" {
		t.Fatalf("rank admission: %v", err)
	}
}

// Even an image the template itself names is not offered as compatible when
// the catalog knows it is built for another platform than the node's.
func TestTemplateOwnedImageOfAnotherPlatformIsNotACompatibleOption(t *testing.T) {
	for _, tc := range []struct{ name, arch, framework string }{
		{"architecture", "arm64", "cuda"},
		{"execution OS", "amd64", "metal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const image = "catalog/platform:v1"
			rig := newCompatTemplateRig(t, "docker", func(spec *store.InferenceSpecRow) { spec.Image = image })
			node, err := rig.st.NodeByUUID(rig.nodeID)
			if err != nil {
				t.Fatal(err)
			}
			if err := rig.st.DB().Model(node).Updates(map[string]any{"arch": "amd64", "os": "linux"}).Error; err != nil {
				t.Fatal(err)
			}
			if _, err := rig.st.UpsertImage(&store.Image{Engine: "vllm", Framework: tc.framework, Arch: tc.arch, ImageRef: image, Source: "custom", Enabled: true}); err != nil {
				t.Fatal(err)
			}
			node, err = rig.st.NodeWithGPUs(rig.nodeID)
			if err != nil {
				t.Fatal(err)
			}
			selection := templateSelection{Spec: &store.InferenceSpecRow{Image: image}}
			options := rig.h.deploymentImageOptions(deploymentImageTarget("vllm", "", node), selection, image, nil)
			for _, option := range options {
				if option.Image == image && option.Compatible {
					t.Fatalf("trusted incompatible option: %+v", option)
				}
			}
		})
	}
}
