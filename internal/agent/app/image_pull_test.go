package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

type imagePullTestDriver struct {
	pullErr    error
	tagErr     error
	tagImage   bool
	pullLines  []string
	containers []string // reported by ListContainers
	listErr    error    // injected ListContainers failure
}

func (d *imagePullTestDriver) InspectImage(context.Context, string) (runtime.ImageInspection, error) {
	return runtime.ImageInspection{ID: "sha256:test", Entrypoint: []string{"vllm"}, Cmd: []string{"serve"}, Env: map[string]string{"A": "B"}, Volumes: []string{"/model"}}, nil
}

func (d *imagePullTestDriver) Name() string                                      { return "image-pull-test" }
func (d *imagePullTestDriver) DeviceView() runtime.DeviceView                    { return runtime.ViewGranted }
func (d *imagePullTestDriver) Detect(context.Context) (bool, error)              { return true, nil }
func (d *imagePullTestDriver) ImageExists(context.Context, string) (bool, error) { return false, nil }
func (d *imagePullTestDriver) PullImage(_ context.Context, _ string, onLine func(string)) error {
	if onLine != nil {
		lines := d.pullLines
		if len(lines) == 0 {
			lines = []string{"pulling"}
		}
		for _, line := range lines {
			onLine(line)
		}
	}
	return d.pullErr
}
func (d *imagePullTestDriver) Prepare(context.Context, runtime.WorkloadSpec) error { return nil }
func (d *imagePullTestDriver) Run(context.Context, runtime.WorkloadSpec) (string, error) {
	return "id", nil
}
func (d *imagePullTestDriver) Stop(context.Context, string) error   { return nil }
func (d *imagePullTestDriver) Remove(context.Context, string) error { return nil }
func (d *imagePullTestDriver) ListContainers(context.Context) ([]string, error) {
	return d.containers, d.listErr
}
func (d *imagePullTestDriver) RunningContainers(context.Context) ([]string, error) {
	return nil, d.listErr
}
func (d *imagePullTestDriver) Status(context.Context, string) (runtime.Status, error) {
	return runtime.Status{}, nil
}
func (d *imagePullTestDriver) Logs(context.Context, string, bool, int) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
func (d *imagePullTestDriver) Exec(context.Context, string, []string) ([]byte, error) {
	return nil, nil
}
func (d *imagePullTestDriver) TagImage(context.Context, string, string) error {
	if !d.tagImage {
		return errors.New("tag unsupported")
	}
	return d.tagErr
}

// imagePullNoTagDriver deliberately implements runtime.Driver without the
// optional TagImage method, matching udocker/older runtime behavior.
type imagePullNoTagDriver struct{ base *imagePullTestDriver }

func (d *imagePullNoTagDriver) Name() string                             { return d.base.Name() }
func (d *imagePullNoTagDriver) Detect(ctx context.Context) (bool, error) { return d.base.Detect(ctx) }
func (d *imagePullNoTagDriver) ImageExists(ctx context.Context, image string) (bool, error) {
	return d.base.ImageExists(ctx, image)
}
func (d *imagePullNoTagDriver) PullImage(ctx context.Context, image string, onLine func(string)) error {
	return d.base.PullImage(ctx, image, onLine)
}
func (d *imagePullNoTagDriver) Prepare(ctx context.Context, spec runtime.WorkloadSpec) error {
	return d.base.Prepare(ctx, spec)
}
func (d *imagePullNoTagDriver) Run(ctx context.Context, spec runtime.WorkloadSpec) (string, error) {
	return d.base.Run(ctx, spec)
}
func (d *imagePullNoTagDriver) Stop(ctx context.Context, id string) error {
	return d.base.Stop(ctx, id)
}
func (d *imagePullNoTagDriver) ListContainers(ctx context.Context) ([]string, error) {
	return d.base.ListContainers(ctx)
}
func (d *imagePullNoTagDriver) RunningContainers(ctx context.Context) ([]string, error) {
	return d.base.RunningContainers(ctx)
}
func (d *imagePullNoTagDriver) Remove(ctx context.Context, id string) error {
	return d.base.Remove(ctx, id)
}
func (d *imagePullNoTagDriver) Status(ctx context.Context, id string) (runtime.Status, error) {
	return d.base.Status(ctx, id)
}
func (d *imagePullNoTagDriver) Logs(ctx context.Context, id string, follow bool, tail int) (io.ReadCloser, error) {
	return d.base.Logs(ctx, id, follow, tail)
}
func (d *imagePullNoTagDriver) Exec(ctx context.Context, id string, cmd []string) ([]byte, error) {
	return d.base.Exec(ctx, id, cmd)
}

func TestRPCImagePullRejectsDigestAndMissingReferences(t *testing.T) {
	n := NewNodeRuntime(slog.Default(), &imagePullTestDriver{tagImage: true}, t.TempDir())
	for _, raw := range []string{
		`{"effective_ref":"repo/image@sha256:abc","canonical_ref":"repo/image:tag"}`,
		`{"effective_ref":"repo/image:tag","canonical_ref":"repo/image@sha256:abc"}`,
		`{"effective_ref":"","canonical_ref":"repo/image:tag"}`,
	} {
		if _, err := n.imageWork().rpcImagePull(context.Background(), json.RawMessage(raw)); err == nil {
			t.Errorf("rpcImagePull(%s) unexpectedly succeeded", raw)
		}
	}
}

func TestRPCImagePullReportsFailureAndSuccessOrder(t *testing.T) {
	tests := []struct {
		name       string
		driver     runtime.Driver
		wantStates []string
		wantError  string
	}{
		{name: "pull failure", driver: &imagePullTestDriver{pullErr: errors.New("registry down"), tagImage: true}, wantStates: []string{"pulling", "failed"}, wantError: "registry down"},
		{name: "tag unsupported", driver: &imagePullNoTagDriver{base: &imagePullTestDriver{}}, wantStates: []string{"pulling", "failed"}, wantError: "does not support"},
		{name: "tag failure", driver: &imagePullTestDriver{tagImage: true, tagErr: errors.New("tag denied")}, wantStates: []string{"pulling", "failed"}, wantError: "tag canonical image"},
		{name: "success", driver: &imagePullTestDriver{tagImage: true, pullLines: make([]string, 100)}, wantStates: []string{"pulling", "present"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			statuses := make(chan proto.ImageNodeStatus, 8)
			n := NewNodeRuntime(slog.Default(), tc.driver, t.TempDir())
			n.send = func(kind string, data any) error {
				if kind == proto.TStatusImageNode {
					statuses <- data.(proto.ImageNodeStatus)
				}
				return nil
			}
			result, err := n.imageWork().rpcImagePull(context.Background(), json.RawMessage(`{"effective_ref":"mirror/repo:tag","canonical_ref":"repo/image:tag"}`))
			if err != nil {
				t.Fatal(err)
			}
			if result.(proto.ImagePullResult).State != "pulling" {
				t.Fatalf("rpc result = %+v", result)
			}
			// Progress lines are rate limited and may repeat a state, so the
			// contract is the ordered sequence of state changes, not a fixed
			// number of reports. Collect until the terminal state arrives.
			var got []proto.ImageNodeStatus
			var transitions []string
			terminal := tc.wantStates[len(tc.wantStates)-1]
			deadline := time.After(2 * time.Second)
			for {
				select {
				case status := <-statuses:
					got = append(got, status)
					if len(transitions) == 0 || transitions[len(transitions)-1] != status.State {
						transitions = append(transitions, status.State)
					}
					if status.State != terminal {
						continue
					}
				case <-deadline:
					t.Fatalf("statuses = %+v, want states %v", got, tc.wantStates)
				}
				break
			}
			if len(transitions) != len(tc.wantStates) {
				t.Fatalf("state transitions = %v, want %v (raw %+v)", transitions, tc.wantStates, got)
			}
			for i, want := range tc.wantStates {
				if transitions[i] != want {
					t.Fatalf("state transitions = %v, want %v", transitions, tc.wantStates)
				}
			}
			if tc.wantError != "" && !strings.Contains(got[len(got)-1].Message, tc.wantError) {
				t.Fatalf("terminal message = %q, want %q", got[len(got)-1].Message, tc.wantError)
			}
		})
	}
}

func TestRPCImageInspect(t *testing.T) {
	n := NewNodeRuntime(slog.Default(), &imagePullTestDriver{}, t.TempDir())
	got, err := n.imageWork().rpcImageInspect(context.Background(), json.RawMessage(`{"image_ref":"repo/image:tag"}`))
	if err != nil {
		t.Fatal(err)
	}
	result := got.(proto.ImageInspectResult)
	if result.ImageID != "sha256:test" || result.Entrypoint[0] != "vllm" || result.Env["A"] != "B" {
		t.Fatalf("inspect=%+v", result)
	}
}
