package digitalocean

import (
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/provider"
)

func createFake(t *testing.T) (*Driver, *fakeDO) {
	d, f := newFake(t)
	f.sizes = []map[string]any{
		gpuSize(), adaSize(), mi300Size(),
		sizeJSON("gpu-h100x8-640gb", 35.28, "nvidia_h100", 8, 640),
		cpuSize(),
	}
	return d, f
}

func lastCreate(t *testing.T, f *fakeDO) map[string]any {
	t.Helper()
	calls := f.calls(http.MethodPost, "/v2/droplets")
	if len(calls) != 1 {
		t.Fatalf("POST /v2/droplets sent %d times, want 1", len(calls))
	}
	return calls[0].Body
}

func TestCreateInstanceSendsTheDropletRequest(t *testing.T) {
	d, f := createFake(t)
	line, fingerprint := newTestKey(t, "platform")
	if _, err := d.AuthorizePublicKey(t.Context(), testCreds, "", line); err != nil {
		t.Fatal(err)
	}
	userKeyLine, userKeyFP := newTestKey(t, "ops")
	userKeyID := f.addKey(t, "ops", userKeyLine)

	inst, err := d.CreateInstance(t.Context(), testCreds, provider.CreateInstanceRequest{
		Name: "train-1", Region: "nyc2",
		Extra: map[string]string{"size": "gpu-h100x1-80gb", "ssh_keys": fmt.Sprintf("%d, %s,%d", userKeyID, userKeyFP, userKeyID)},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"name": "train-1", "region": "nyc2", "size": "gpu-h100x1-80gb", "image": "gpu-h100x1-base",
		"tags": []any{"gpuworks"},
		// The platform key first, then the operator's by id and by
		// fingerprint; the repeated id is sent once. Ids are numbers.
		"ssh_keys": []any{fingerprint, float64(userKeyID), userKeyFP},
	}
	if got := lastCreate(t, f); !reflect.DeepEqual(got, want) {
		t.Errorf("request body =\n%v\nwant\n%v", got, want)
	}
	if inst.ExternalID == "" || inst.Name != "train-1" || inst.Region != "nyc2" || inst.GPUCount != 1 || inst.GPUModel != "NVIDIA H100" ||
		inst.Power != provider.PowerUnknown || inst.PricePerHr != 4.41 || inst.Kind != "vm" {
		t.Errorf("instance = %+v", inst)
	}
}

func TestCreateInstanceDefaultImageFollowsTheSizesVendor(t *testing.T) {
	for _, tc := range []struct{ size, image string }{
		{"gpu-h100x1-80gb", "gpu-h100x1-base"},
		{"gpu-4000adax1-20gb", "gpu-h100x1-base"},
		{"gpu-h100x8-640gb", "gpu-h100x8-base"},
		{"gpu-mi300x8-1536gb", "gpu-amd-base"},
	} {
		d, f := createFake(t)
		if _, err := d.CreateInstance(t.Context(), testCreds, provider.CreateInstanceRequest{Region: "nyc2", Extra: map[string]string{"size": tc.size}}); err != nil {
			t.Fatalf("%s: %v", tc.size, err)
		}
		body := lastCreate(t, f)
		if body["image"] != tc.image || body["name"] != "gpuworks" {
			t.Errorf("%s: image %v name %v, want image %s", tc.size, body["image"], body["name"], tc.image)
		}
		if keys, _ := body["ssh_keys"].([]any); len(keys) != 0 {
			t.Errorf("%s: ssh_keys = %v without any key registered", tc.size, keys)
		}
	}
}

func TestCreateInstanceImagePrecedence(t *testing.T) {
	d, f := createFake(t)
	extra := map[string]string{"size": "gpu-h100x1-80gb", "image": "ubuntu-24-04-x64"}
	if _, err := d.CreateInstance(t.Context(), testCreds, provider.CreateInstanceRequest{Region: "nyc2", Extra: extra}); err != nil {
		t.Fatal(err)
	}
	if got := lastCreate(t, f)["image"]; got != "ubuntu-24-04-x64" {
		t.Errorf("Extra image: %v", got)
	}
	d, f = createFake(t)
	if _, err := d.CreateInstance(t.Context(), testCreds, provider.CreateInstanceRequest{Region: "nyc2", Image: "123456", Extra: extra}); err != nil {
		t.Fatal(err)
	}
	if got := lastCreate(t, f)["image"]; got != "123456" {
		t.Errorf("req.Image: %v", got)
	}
}

func TestCreateInstanceRefusesBeforeAnyRequest(t *testing.T) {
	for name, req := range map[string]provider.CreateInstanceRequest{
		"no region":                    {Extra: map[string]string{"size": "gpu-h100x1-80gb"}},
		"no size":                      {Region: "nyc2"},
		"blank size":                   {Region: "nyc2", Extra: map[string]string{"size": "  "}},
		"no image for a plain size":    {Region: "nyc2", Extra: map[string]string{"size": "s-2vcpu-4gb"}},
		"no image for a malformed gpu": {Region: "nyc2", Extra: map[string]string{"size": "gpu-"}},
	} {
		t.Run(name, func(t *testing.T) {
			d, f := createFake(t)
			if _, err := d.CreateInstance(t.Context(), testCreds, req); err == nil {
				t.Fatal("create succeeded")
			}
			if len(f.requests) != 0 {
				t.Errorf("requests sent: %v", f.requests)
			}
		})
	}
}

func TestCreateInstanceSurfacesTheAPIsRefusal(t *testing.T) {
	d, f := createFake(t)
	_, err := d.CreateInstance(t.Context(), testCreds, provider.CreateInstanceRequest{Region: "nyc2", Extra: map[string]string{"size": "gpu-nope-1gb", "image": "x"}})
	var apiErr *APIError
	if err == nil || !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnprocessableEntity || apiErr.ID != "unprocessable_entity" ||
		!strings.Contains(err.Error(), "invalid size") {
		t.Fatalf("error = %v", err)
	}
	if len(f.droplets) != 0 {
		t.Errorf("a droplet was created: %v", f.droplets)
	}
	// An ssh key the account does not have is refused by the API too.
	_, err = d.CreateInstance(t.Context(), testCreds, provider.CreateInstanceRequest{Region: "nyc2", Extra: map[string]string{"size": "gpu-h100x1-80gb", "ssh_keys": "aa:bb"}})
	if err == nil || !strings.Contains(err.Error(), "invalid SSH Key") {
		t.Fatalf("error = %v", err)
	}
}

// A create that is repeated after a server error might start a second
// billed droplet, so only a rate limit (not processed) is repeated.
func TestCreateIsNotRepeatedAfterAServerError(t *testing.T) {
	d, f := createFake(t)
	f.forced = []forcedResponse{{only: http.MethodPost, status: 500, body: `{"id":"server_error","message":"Server was unable to give you a response."}`}}
	if _, err := d.CreateInstance(t.Context(), testCreds, provider.CreateInstanceRequest{Region: "nyc2", Extra: map[string]string{"size": "gpu-h100x1-80gb"}}); err == nil {
		t.Fatal("create succeeded")
	}
	if n := len(f.calls(http.MethodPost, "/v2/droplets")); n != 1 || len(f.droplets) != 0 {
		t.Errorf("create sent %d times, %d droplets exist", n, len(f.droplets))
	}
}

func TestCreateIsRepeatedAfterARateLimit(t *testing.T) {
	d, f := createFake(t)
	f.forced = []forcedResponse{{only: http.MethodPost, status: 429, header: map[string]string{"Retry-After": "3"}, body: `{"id":"too_many_requests","message":"API Rate limit exceeded."}`}}
	if _, err := d.CreateInstance(t.Context(), testCreds, provider.CreateInstanceRequest{Region: "nyc2", Extra: map[string]string{"size": "gpu-h100x1-80gb"}}); err != nil {
		t.Fatal(err)
	}
	if len(f.droplets) != 1 {
		t.Errorf("%d droplets exist, want 1", len(f.droplets))
	}
}
