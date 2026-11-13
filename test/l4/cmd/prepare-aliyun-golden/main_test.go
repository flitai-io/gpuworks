package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	serverprovider "gpuworks.ai/gpuworks/internal/server/provider"
	aliyundriver "gpuworks.ai/gpuworks/internal/server/provider/aliyun"
)

// imageFixture answers the image calls the way ECS does: DescribeImages lists
// only Available images unless Status names the others, CopyImage needs an
// Available source, and a copy is a new image ID in the destination region.
type imageFixture struct {
	mu      sync.Mutex
	images  map[string]*fixtureImage
	next    int
	creates []map[string]string
	copies  []map[string]string
}

type fixtureImage struct {
	ID, Name, Region, Status string
	describes                int
	finishAfter              int // describes before Creating turns into the final status
	final                    string
}

func (f *imageFixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	q := r.URL.Query()
	action := r.Header.Get("x-acs-action")
	reply := func(status int, body any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}
	fail := func(status int, code, msg string) {
		reply(status, map[string]string{"Code": code, "Message": msg, "RequestId": "REQ"})
	}
	switch action {
	case "CreateImage":
		if q.Get("InstanceId") == "" || q.Get("ImageName") == "" {
			fail(400, "MissingParameter", "InstanceId and ImageName are required")
			return
		}
		for _, img := range f.images {
			if img.Name == q.Get("ImageName") && img.Region == q.Get("RegionId") {
				fail(400, "InvalidImageName.Duplicated", "The specified image name already exists.")
				return
			}
		}
		f.next++
		id := "m-created" + string(rune('0'+f.next))
		f.images[id] = &fixtureImage{ID: id, Name: q.Get("ImageName"), Region: q.Get("RegionId"), Status: "Creating", finishAfter: 2, final: "Available"}
		f.creates = append(f.creates, map[string]string{"instance": q.Get("InstanceId"), "region": q.Get("RegionId"), "tag": q.Get("Tag.1.Key")})
		reply(200, map[string]string{"RequestId": "REQ", "ImageId": id})
	case "CopyImage":
		src := f.images[q.Get("ImageId")]
		if src == nil || src.Region != q.Get("RegionId") {
			fail(404, "InvalidImageId.NotFound", "The specified ImageId does not exist.")
			return
		}
		if src.Status != "Available" {
			fail(400, "IncorrectImageStatus", "The specified image is not Available.")
			return
		}
		if q.Get("DestinationRegionId") == "" || q.Get("DestinationRegionId") == q.Get("RegionId") {
			fail(400, "InvalidDestinationRegionId.Malformed", "DestinationRegionId must be another region.")
			return
		}
		f.next++
		id := "m-copy" + string(rune('0'+f.next))
		f.images[id] = &fixtureImage{ID: id, Name: q.Get("DestinationImageName"), Region: q.Get("DestinationRegionId"), Status: "Creating", finishAfter: 1, final: "Available"}
		f.copies = append(f.copies, map[string]string{"to": q.Get("DestinationRegionId")})
		reply(200, map[string]string{"RequestId": "REQ", "ImageId": id})
	case "DescribeImages":
		wantStatus := map[string]bool{"Available": true}
		if s := q.Get("Status"); s != "" {
			wantStatus = map[string]bool{}
			for _, part := range strings.Split(s, ",") {
				wantStatus[part] = true
			}
		}
		var out []map[string]string
		for _, img := range f.images {
			if img.Region != q.Get("RegionId") || (q.Get("ImageId") != "" && img.ID != q.Get("ImageId")) || (q.Get("ImageName") != "" && img.Name != q.Get("ImageName")) {
				continue
			}
			if img.Status == "Creating" {
				img.describes++
				if img.describes > img.finishAfter {
					img.Status = img.final
				}
			}
			if wantStatus[img.Status] {
				out = append(out, map[string]string{"ImageId": img.ID, "ImageName": img.Name, "Status": img.Status, "Progress": "50%"})
			}
		}
		reply(200, map[string]any{"RequestId": "REQ", "Images": map[string]any{"Image": out}})
	default:
		fail(400, "InvalidAction.NotFound", action)
	}
}

func newImageAPI(t *testing.T) (imageAPI, *imageFixture) {
	t.Helper()
	f := &imageFixture{images: map[string]*fixtureImage{}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return imageAPI{rpc: aliyundriver.NewRPC(), creds: serverprovider.Credentials{PublicKey: "id", PrivateKey: "secret", Endpoint: srv.URL}}, f
}

func TestImageLifecycleCreateWaitAndCopy(t *testing.T) {
	api, f := newImageAPI(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id, err := api.create(ctx, "cn-beijing", "i-source", "golden-1", "desc")
	if err != nil {
		t.Fatal(err)
	}
	// A copy of an image that is still being made is refused by the service.
	if _, err := api.copy(ctx, "cn-beijing", id, "cn-hangzhou", "golden-1", "d"); err == nil || aliyundriver.ErrorCode(err) != "IncorrectImageStatus" {
		t.Fatalf("copy of a Creating image = %v, want IncorrectImageStatus", err)
	}
	var progress []string
	if err := api.wait(ctx, "cn-beijing", id, time.Millisecond, func(line string) { progress = append(progress, line) }); err != nil {
		t.Fatal(err)
	}
	if len(progress) == 0 || !strings.Contains(progress[0], "status=Creating") {
		t.Fatalf("wait reported no progress while the image was Creating: %v", progress)
	}
	copyID, err := api.copy(ctx, "cn-beijing", id, "cn-hangzhou", "golden-1", "d")
	if err != nil {
		t.Fatal(err)
	}
	if err := api.wait(ctx, "cn-hangzhou", copyID, time.Millisecond, nil); err != nil {
		t.Fatal(err)
	}
	if len(f.creates) != 1 || f.creates[0]["instance"] != "i-source" || f.creates[0]["tag"] != "gpuworks-golden" {
		t.Fatalf("CreateImage requests = %v", f.creates)
	}
	if _, err := api.create(ctx, "cn-beijing", "i-source", "golden-1", "d"); aliyundriver.ErrorCode(err) != "InvalidImageName.Duplicated" {
		t.Fatalf("a duplicate image name must surface the API error, got %v", err)
	}
	found, err := api.byName(ctx, "cn-hangzhou", "golden-1")
	if err != nil || found.ImageID != copyID {
		t.Fatalf("byName = %+v, %v", found, err)
	}
	if _, err := api.byName(ctx, "cn-hangzhou", "nope"); err == nil {
		t.Fatal("an unknown image name must be an error")
	}
}

func TestWaitStopsAtOnceWhenTheImageFails(t *testing.T) {
	api, f := newImageAPI(t)
	f.images["m-bad"] = &fixtureImage{ID: "m-bad", Name: "bad", Region: "cn-beijing", Status: "Creating", finishAfter: 1, final: "CreateFailed"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	err := api.wait(ctx, "cn-beijing", "m-bad", time.Millisecond, nil)
	if err == nil || !strings.Contains(err.Error(), "CreateFailed") {
		t.Fatalf("wait on a failed image = %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("wait ran out the clock instead of failing at once")
	}
}

func TestCopyEverywhereCopiesToEveryOtherRegionAndReportsEachID(t *testing.T) {
	api, f := newImageAPI(t)
	f.images["m-src"] = &fixtureImage{ID: "m-src", Name: "golden", Region: "cn-beijing", Status: "Available"}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	o := options{regions: []string{"cn-beijing", "cn-hangzhou", "cn-shenzhen"}, copyPoll: time.Millisecond}
	ids, err := copyEverywhere(ctx, api, o, "cn-beijing", "m-src", "golden")
	if err != nil {
		t.Fatal(err)
	}
	if ids["cn-beijing"] != "m-src" || ids["cn-hangzhou"] == "" || ids["cn-shenzhen"] == "" || len(ids) != 3 {
		t.Fatalf("ids = %v", ids)
	}
	if len(f.copies) != 2 {
		t.Fatalf("copies = %v, want one per other region and none to the source region", f.copies)
	}
	// A source the service refuses to copy is reported with the API's reason.
	f.images["m-src"].Status = "UnAvailable"
	if _, err := copyEverywhere(ctx, api, options{regions: []string{"cn-beijing", "cn-guangzhou"}, copyPoll: time.Millisecond}, "cn-beijing", "m-src", "golden"); err == nil || !strings.Contains(err.Error(), "IncorrectImageStatus") {
		t.Fatalf("copy from an unusable source = %v", err)
	}
}

func TestEnvVarNameMatchesWhatTheHarnessReads(t *testing.T) {
	if got := envVarName("cn-hangzhou"); got != "ALIYUN_IMAGE_CN_HANGZHOU" {
		t.Fatalf("envVarName = %q", got)
	}
}

func TestInstallDockerCommandPinsDockerAndUsesMainlandMirrors(t *testing.T) {
	command := installDockerCommand()
	for _, want := range []string{
		"mirrors.aliyun.com/docker-ce/linux/ubuntu", `5:29\.`, "nvidia-ctk runtime configure --runtime=docker",
		"GPUWORKS_DOCKER_INSTALL_EVIDENCE", "docker info --format '{{json .Runtimes}}' | grep -q nvidia",
	} {
		if !strings.Contains(command, want) {
			t.Errorf("install command lacks %q", want)
		}
	}
	if strings.Contains(command, "download.docker.com") {
		t.Error("install command reaches Docker's own site, which mainland instances cannot rely on")
	}
	if strings.Contains(command, "sudo") {
		t.Error("the builder runs as root; sudo would only add a failure mode")
	}
	if strings.Index(command, "rm -f /etc/apt/sources.list.d/docker.list") > strings.Index(command, "apt-get update") {
		t.Error("a resumed build must drop the Docker repository of the earlier attempt before the base apt update")
	}
	if aliyun, tuna := strings.Index(command, "mirrors.aliyun.com/docker-ce"), strings.Index(command, "mirrors.tuna.tsinghua.edu.cn/docker-ce"); aliyun < 0 || tuna < aliyun {
		t.Error("a Docker CE mirror caught mid-sync must fall back to the next mirror, the cloud's own first")
	}
	if strings.Index(command, "mirrors.aliyun.com/nvidia-container-toolkit") > strings.Index(command, "nvidia.github.io") {
		t.Error("the toolkit mirrors must be tried before nvidia.github.io")
	}
}

func TestPullImageCommandTriesMirrorsInOrderAndTagsTheCanonicalName(t *testing.T) {
	command := pullImageCommand("gpuworks/gpuworks-runner:cuda13.0-vllm0.30.0-r4", []string{"swr.example/gpuworks/gpuworks-runner:x", "gpuworks/gpuworks-runner:cuda13.0-vllm0.30.0-r4"})
	if strings.Index(command, "swr.example") > strings.Index(command, "docker pull 'gpuworks/gpuworks-runner") {
		t.Fatalf("mirror must come first:\n%s", command)
	}
	if !strings.Contains(command, `docker tag "$ok" 'gpuworks/gpuworks-runner:cuda13.0-vllm0.30.0-r4'`) {
		t.Fatalf("the pulled image is not tagged with the name the cases deploy:\n%s", command)
	}
}

func TestBootedChecksRequireModelImageRuntimeAndAGPUInAContainer(t *testing.T) {
	command := bootedChecksCommand("/opt/gpuworks/test-fixtures/models/qwen3-0.6b", "gpuworks/gpuworks-runner:cuda13.0-vllm0.30.0-r4", nil, nil)
	if strings.Contains(command, "GPUWORKS_EXTRA_IMAGE") {
		t.Error("no extra image was asked for, yet the checks look for one")
	}
	for _, want := range []string{"sha256sum -c SHA256SUMS", "docker image inspect", "grep -q nvidia", "docker run --rm --gpus all --entrypoint nvidia-smi", "GPUWORKS_GOLDEN_OK"} {
		if !strings.Contains(command, want) {
			t.Errorf("booted-image checks lack %q", want)
		}
	}
	if !strings.HasPrefix(command, "set -eu") {
		t.Error("checks must abort on the first failure")
	}
}

func TestBootedChecksCoverTheExtraImageInAGPUContainer(t *testing.T) {
	const extra = "swr.example/gpuworks/gpuworks-runner:cuda12.8-v100-vllm1cat1.5.1-r1"
	command := bootedChecksCommand("/opt/m", "gpuworks/gpuworks-runner:cuda13.0-vllm0.30.0-r2", []string{extra}, nil)
	for _, want := range []string{
		"docker image inspect '" + extra + "' --format 'GPUWORKS_EXTRA_IMAGE",
		"gpu_smoke '" + extra + "'",
		"gpu_smoke 'gpuworks/gpuworks-runner:cuda13.0-vllm0.30.0-r2'",
		`for try in 1 2 3; do
    if docker run --rm --gpus all --entrypoint nvidia-smi "$1" -L; then return 0; fi`,
		"GPUWORKS_GOLDEN_OK",
	} {
		if !strings.Contains(command, want) {
			t.Errorf("booted-image checks lack %q", want)
		}
	}
}

func TestBuildStepsOnAnEarlierGoldenImageOnlyAddTheExtraImage(t *testing.T) {
	phases := func(o options) string {
		var names []string
		for _, step := range buildSteps(o, nil) {
			names = append(names, step.phase)
		}
		return strings.Join(names, ",")
	}
	base := options{modelPath: "/opt/m", dockerImage: "runner:a", pullSources: []string{"runner:a"}}
	if got := phases(base); got != "install-docker,docker-cgroupfs,pull-runner-image,prepare-model,validate-source" {
		t.Fatalf("fresh build phases = %s", got)
	}
	withExtra := base
	withExtra.extraImages, withExtra.extraPullSources = []string{"runner:v100"}, []string{"mirror/runner:v100", "runner:v100"}
	if got := phases(withExtra); got != "install-docker,docker-cgroupfs,pull-runner-image,prepare-model,pull-extra-image,validate-source" {
		t.Fatalf("fresh build with an extra image phases = %s", got)
	}
	layered := withExtra
	layered.baseGolden = true
	steps := buildSteps(layered, nil)
	if got := phases(layered); got != "validate-base,docker-cgroupfs,pull-extra-image,validate-source" {
		t.Fatalf("layered build phases = %s (it must not reinstall Docker or touch the model)", got)
	}
	if strings.Contains(steps[0].command, "runner:v100") {
		t.Error("the base check must not expect the image it is about to add")
	}
	pull := steps[2].command
	if strings.Index(pull, "docker pull 'mirror/runner:v100'") > strings.Index(pull, "docker pull 'runner:v100'") || !strings.Contains(pull, "docker tag \"$ok\" 'runner:v100'") {
		t.Errorf("extra image pull must try the mirror first and tag the canonical name:\n%s", pull)
	}
	if !strings.Contains(steps[3].command, "GPUWORKS_EXTRA_IMAGE") || !strings.Contains(imageDescription(layered), "runner:v100") {
		t.Error("the final check and the image description must name the extra image")
	}
}

func TestLayeredBuildRemovesRetiredImagesBeforeAddingTheNewOne(t *testing.T) {
	o := options{modelPath: "/opt/m", dockerImage: "runner:v100", baseGolden: true,
		extraImages: []string{"runner:r4"}, extraPullSources: []string{"runner:r4"}, removeImages: []string{"runner:r2"}}
	steps := buildSteps(o, nil)
	var names []string
	for _, step := range steps {
		names = append(names, step.phase)
	}
	if got := strings.Join(names, ","); got != "validate-base,remove-images,docker-cgroupfs,pull-extra-image,validate-source" {
		t.Fatalf("phases = %s (the old runner must go before the new one is pulled)", got)
	}
	remove := steps[1].command
	for _, want := range []string{"docker image inspect --format '{{.Id}}' 'runner:r2'", "docker image rm -f \"$id\"", "docker image prune -f"} {
		if !strings.Contains(remove, want) {
			t.Errorf("remove step lacks %q:\n%s", want, remove)
		}
	}
	if strings.Contains(steps[0].command, "runner:r2") {
		t.Error("the base check must not depend on the image about to be removed")
	}
	if final := steps[4].command; !strings.Contains(final, "if docker image inspect 'runner:r2'") || !strings.Contains(final, "still present") {
		t.Errorf("the final check must fail when the removed image is still there:\n%s", final)
	}
}

func TestRemoveImagesNeedsALayeredBuildAndCannotDropAKeptImage(t *testing.T) {
	t.Setenv("ALIYUN_ACCESS_KEY_ID", "id")
	t.Setenv("ALIYUN_ACCESS_KEY_SECRET", "secret")
	base := options{regions: []string{"cn-beijing"}, modelPath: "/opt/m", diskGB: 150, dockerImage: "runner:v100", extraImages: []string{"runner:r4"}, removeImages: []string{"runner:r2"}}
	if err := run(base); err == nil || !strings.Contains(err.Error(), "-remove-images needs -base-golden") {
		t.Fatalf("fresh build with -remove-images: err = %v", err)
	}
	keep := base
	keep.baseGolden, keep.removeImages = true, []string{"runner:r4"}
	if err := run(keep); err == nil || !strings.Contains(err.Error(), "must keep") {
		t.Fatalf("removing the image being added: err = %v", err)
	}
}

func TestSeveralExtraImagesArePulledFromTheMirrorsInOrder(t *testing.T) {
	o := options{modelPath: "/opt/m", dockerImage: "runner:v100", baseGolden: true,
		extraImages: []string{"runner:r4", "runner:cu129"}, extraMirrors: []string{"swr.example/", "quay.example"}}
	steps := buildSteps(o, nil)
	var names []string
	for _, step := range steps {
		names = append(names, step.phase)
	}
	if got := strings.Join(names, ","); got != "validate-base,docker-cgroupfs,pull-extra-image,pull-extra-image,validate-source" {
		t.Fatalf("phases = %s", got)
	}
	for i, image := range o.extraImages {
		pull := steps[2+i].command
		swr, quay, own := strings.Index(pull, "docker pull 'swr.example/"+image+"'"), strings.Index(pull, "docker pull 'quay.example/"+image+"'"), strings.Index(pull, "docker pull '"+image+"'")
		if swr < 0 || quay < swr || own < quay {
			t.Errorf("%s must be tried from swr, then quay, then its own name:\n%s", image, pull)
		}
	}
	final := steps[4].command
	for _, image := range o.extraImages {
		if !strings.Contains(final, "gpu_smoke '"+image+"'") {
			t.Errorf("final check does not run %s on the GPU", image)
		}
	}
	t.Setenv("ALIYUN_ACCESS_KEY_ID", "id")
	t.Setenv("ALIYUN_ACCESS_KEY_SECRET", "secret")
	bad := o
	bad.regions, bad.diskGB, bad.extraPullSources = []string{"cn-beijing"}, 150, []string{"x/runner:r4"}
	if err := run(bad); err == nil || !strings.Contains(err.Error(), "single -extra-image") {
		t.Fatalf("-extra-pull-sources with two images: err = %v", err)
	}
}

func TestEveryBuildSwitchesDockerToCgroupfsAndTheFinalCheckRequiresIt(t *testing.T) {
	for _, o := range []options{
		{modelPath: "/opt/m", dockerImage: "runner:a", pullSources: []string{"runner:a"}},
		{modelPath: "/opt/m", dockerImage: "runner:a", baseGolden: true},
	} {
		steps := buildSteps(o, nil)
		var cgroupfs, final string
		for _, step := range steps {
			if step.phase == "docker-cgroupfs" {
				cgroupfs = step.command
			}
		}
		final = steps[len(steps)-1].command
		for _, want := range []string{"native.cgroupdriver=cgroupfs", "systemctl restart docker", "want cgroupfs"} {
			if !strings.Contains(cgroupfs, want) {
				t.Errorf("baseGolden=%v: cgroupfs step lacks %q:\n%s", o.baseGolden, want, cgroupfs)
			}
		}
		if !strings.Contains(final, "GPUWORKS_DOCKER_CGROUP_DRIVER") {
			t.Errorf("baseGolden=%v: final check does not require cgroupfs", o.baseGolden)
		}
		if o.baseGolden && strings.Contains(steps[0].command, "want cgroupfs") {
			t.Error("the base check must not require cgroupfs: earlier golden images use the systemd driver")
		}
	}
}

func TestSplitList(t *testing.T) {
	got := splitList(" cn-hangzhou, cn-beijing\ncn-shenzhen ,,")
	if strings.Join(got, "|") != "cn-hangzhou|cn-beijing|cn-shenzhen" {
		t.Fatalf("splitList = %v", got)
	}
}

func TestImageNameAndDescriptionFollowTheModel(t *testing.T) {
	for repo, want := range map[string]string{"Qwen/Qwen3-0.6B": "qwen3-0-6b", "Qwen/Qwen2-0.5B-Instruct": "qwen2-0-5b", "plain_name": "plain-name"} {
		if got := modelSlug(repo); got != want {
			t.Fatalf("modelSlug(%q) = %q, want %q", repo, got, want)
		}
	}
	o := options{modelRepo: "Qwen/Qwen3-0.6B", modelPath: "/opt/m", dockerImage: "gpuworks/gpuworks-runner:cuda13.0-vllm0.30.0-r2"}
	if got := imageDescription(o); !strings.Contains(got, o.modelRepo) || !strings.Contains(got, o.dockerImage) || len(got) > 256 {
		t.Fatalf("description = %q", got)
	}
	o.dockerImage = strings.Repeat("x", 300)
	if got := imageDescription(o); len(got) != 256 {
		t.Fatalf("description is %d chars, want capped at 256", len(got))
	}
}
