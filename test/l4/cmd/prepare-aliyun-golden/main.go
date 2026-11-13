// Command prepare-aliyun-golden builds the Alibaba Cloud custom image the
// Aliyun L4 cases boot: Alibaba's public Ubuntu GPU-driver image plus Docker,
// the NVIDIA container toolkit, the vLLM runner image and the test model, so a
// case neither installs nor downloads anything. It copies the image to every
// region the cases may rent in and prints the ALIYUN_IMAGE_<REGION> lines for
// secrets/cloud-providers.env. It never deletes an older image and never
// changes CI variables.
//
// Credentials: ALIYUN_ACCESS_KEY_ID and ALIYUN_ACCESS_KEY_SECRET.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	serverprovider "gpuworks.ai/gpuworks/internal/server/provider"
	aliyundriver "gpuworks.ai/gpuworks/internal/server/provider/aliyun"
	"gpuworks.ai/gpuworks/internal/testenv/cloudprovider"
	"gpuworks.ai/gpuworks/internal/testenv/goldenimage"
	"gpuworks.ai/gpuworks/internal/testenv/sshutil"
)

const (
	// defaultBaseImage is Alibaba's public Ubuntu 22.04 image with the NVIDIA
	// driver (580.x) and CUDA preinstalled; its ID is the same in every region.
	defaultBaseImage = "ubuntu_22_04_x64_100G_with_gpu_driver_and_cuda_alibase_20260520.vhd"
	// defaultRegions are where mainland spot GPU stock was found (survey of
	// 2026-10-25): 1-GPU L20/A10/T4 and 2-GPU T4/L20 across these four.
	defaultRegions = "cn-hangzhou,cn-beijing,cn-shenzhen,cn-guangzhou"
	// defaultPullSources are tried in order; the final tag is -docker-image.
	defaultPullSources = "swr.cn-southwest-2.myhuaweicloud.com/gpuworks/gpuworks-runner:cuda13.0-vllm0.30.0-r4,quay.io/gpuworks/gpuworks-runner:cuda13.0-vllm0.30.0-r4,gpuworks/gpuworks-runner:cuda13.0-vllm0.30.0-r4"
)

type options struct {
	regions       []string
	sourceRegions []string
	modelRepo     string
	modelRev      string
	baseImage     string
	gpuModel      string
	diskGB        int
	dockerImage   string
	pullSources   []string
	imageName     string
	onDemand      bool
	validate      bool
	listOnly      bool
	resume        string
	copyImage     string
	copyFrom      string
	modelPath     string
	// baseGolden says -base-image is an earlier golden image that already
	// carries Docker, -docker-image and the model: the build only adds
	// -extra-image on top, and the earlier image is left as it is.
	baseGolden bool
	// extraImages are images baked in next to -docker-image (the 1Cat-vLLM
	// runner for V100, a CUDA 12 runner for Turing). Each is pulled from
	// extraPullSources when it is the only one, otherwise from every prefix in
	// extraMirrors and then its own name; empty adds none.
	extraImages      []string
	extraPullSources []string
	extraMirrors     []string
	// removeImages are images of the earlier golden image to delete before
	// -extra-image is added (a runner the templates no longer name); only
	// with baseGolden.
	removeImages  []string
	buildDeadline time.Duration
	// copyPoll is how often a copy's state is read; zero means 30s.
	copyPoll time.Duration
}

func main() {
	var (
		regions = flag.String("regions", defaultRegions, "regions the golden image is made available in")
		sources = flag.String("source-regions", "", "regions the source and validation VMs may be rented in, whichever has stock (default -regions)")
		pull    = flag.String("pull-sources", defaultPullSources, "registry references tried in order to obtain the runner image")
		remove  = flag.String("remove-images", "", "with -base-golden: images of the earlier golden image to delete first (comma separated)")
		extra   = flag.String("extra-pull-sources", "", "registry references tried in order to obtain -extra-image (default -extra-image itself)")
		o       options
	)
	flag.StringVar(&o.baseImage, "base-image", defaultBaseImage, "public base image (GPU driver + CUDA preinstalled)")
	flag.StringVar(&o.gpuModel, "gpu-model", "T4", "card of the source VM (any 1-GPU card works; the driver is generic)")
	flag.IntVar(&o.diskGB, "disk-gb", 150, "system disk of the source VM, which becomes the image's minimum disk")
	flag.StringVar(&o.dockerImage, "docker-image", goldenimage.DefaultDockerImage, "tag the runner image carries in the golden image (what the L4 cases deploy)")
	flag.StringVar(&o.modelRepo, "model-repo", goldenimage.DefaultModelRepo, "ModelScope model stored in the image")
	flag.StringVar(&o.modelRev, "model-rev", goldenimage.DefaultModelRev, "revision of -model-repo")
	flag.StringVar(&o.imageName, "name", "", "image name (default gpuworks-golden-<model>-<UTC timestamp>)")
	flag.BoolVar(&o.onDemand, "on-demand", false, "rent the source VM on-demand instead of spot-first (a reclaimed spot VM loses the build)")
	flag.BoolVar(&o.validate, "validate", true, "boot the finished image once in the source region and check it")
	flag.BoolVar(&o.listOnly, "list-placements", false, "only print where a source VM could be rented right now; rents nothing")
	flag.StringVar(&o.resume, "resume-credentials", "", "attach to the source VM kept by GPUWORKS_CLOUD_KEEP_ON_FAILURE=1 (credentials file it printed)")
	flag.StringVar(&o.copyImage, "copy-image", "", "finish an interrupted build: copy this existing Available image ID to -regions (other than -copy-from), validate it, print the result")
	flag.StringVar(&o.copyFrom, "copy-from", "", "region of -copy-image")
	flag.StringVar(&o.modelPath, "model-path", goldenimage.DefaultModelPath, "absolute model path stored in the image")
	flag.BoolVar(&o.baseGolden, "base-golden", false, "-base-image is an earlier golden image (Docker, -docker-image and the model already in it): only add -extra-image")
	extraImages := flag.String("extra-image", "", "images to bake in next to -docker-image, comma separated (e.g. the 1Cat-vLLM runner for V100)")
	mirrors := flag.String("extra-mirrors", "", "registry prefixes tried in order for each -extra-image before its own name (e.g. swr.cn-southwest-2.myhuaweicloud.com)")
	flag.DurationVar(&o.buildDeadline, "timeout", 3*time.Hour, "overall limit")
	flag.Parse()
	o.regions = splitList(*regions)
	o.sourceRegions = splitList(*sources)
	o.pullSources = splitList(*pull)
	o.extraImages = splitList(*extraImages)
	o.extraPullSources = splitList(*extra)
	o.extraMirrors = splitList(*mirrors)
	o.removeImages = splitList(*remove)
	if err := run(o); err != nil {
		fmt.Fprintln(os.Stderr, "prepare Aliyun golden image:", err)
		os.Exit(1)
	}
}

// extraSources are the references an extra image is pulled from, in order.
func (o options) extraSources(image string) []string {
	if len(o.extraImages) == 1 && len(o.extraPullSources) > 0 {
		return o.extraPullSources
	}
	var sources []string
	for _, mirror := range o.extraMirrors {
		sources = append(sources, strings.TrimSuffix(mirror, "/")+"/"+image)
	}
	return append(sources, image)
}

func (o options) copyPollEvery() time.Duration {
	if o.copyPoll > 0 {
		return o.copyPoll
	}
	return 30 * time.Second
}

// modelSlug turns a repository into the image-name part: Qwen/Qwen3-0.6B
// becomes qwen3-0-6b (image names allow no dots or slashes).
func modelSlug(repo string) string {
	name := repo[strings.LastIndex(repo, "/")+1:]
	name = strings.TrimSuffix(strings.ToLower(name), "-instruct")
	return strings.NewReplacer(".", "-", "_", "-").Replace(name)
}

// imageDescription names what the image carries; ECS caps it at 256 chars.
func imageDescription(o options) string {
	description := "gpuworks golden: Docker " + dockerMajor + ", NVIDIA container toolkit, " + o.dockerImage + ", " + o.modelRepo + " at " + o.modelPath
	for _, image := range o.extraImages {
		description += ", " + image
	}
	if len(description) > 256 {
		description = description[:256]
	}
	return description
}

func splitList(value string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' }) {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func run(o options) error {
	keyID, secret := strings.TrimSpace(os.Getenv("ALIYUN_ACCESS_KEY_ID")), strings.TrimSpace(os.Getenv("ALIYUN_ACCESS_KEY_SECRET"))
	if keyID == "" || secret == "" {
		return errors.New("ALIYUN_ACCESS_KEY_ID and ALIYUN_ACCESS_KEY_SECRET are required")
	}
	if len(o.regions) == 0 {
		return errors.New("at least one region is required")
	}
	if !strings.HasPrefix(o.modelPath, "/") {
		return errors.New("model-path must be absolute")
	}
	if len(o.removeImages) > 0 && !o.baseGolden {
		return errors.New("-remove-images needs -base-golden (a fresh build has nothing to remove)")
	}
	for _, image := range o.removeImages {
		if image == o.dockerImage || slices.Contains(o.extraImages, image) {
			return fmt.Errorf("-remove-images names %s, which the image must keep", image)
		}
	}
	if len(o.extraPullSources) > 0 && len(o.extraImages) != 1 {
		return errors.New("-extra-pull-sources is for a single -extra-image; use -extra-mirrors for several")
	}
	if o.diskGB < 100 {
		return fmt.Errorf("disk-gb=%d is below the 100 GiB base image", o.diskGB)
	}
	if len(o.sourceRegions) == 0 {
		o.sourceRegions = o.regions
	}
	if strings.TrimSpace(o.imageName) == "" {
		o.imageName = "gpuworks-golden-" + modelSlug(o.modelRepo) + "-" + time.Now().UTC().Format("20060102-150405")
	}
	ctx, cancel := context.WithTimeout(context.Background(), o.buildDeadline)
	defer cancel()
	runID, err := cloudprovider.NewRunID()
	if err != nil {
		return err
	}
	runID = "golden-" + runID
	images := map[string]string{}
	for _, region := range o.sourceRegions {
		images[region] = o.baseImage
	}
	provider, err := cloudprovider.NewAliyun(cloudprovider.AliyunConfig{
		AccessKeyID: keyID, AccessKeySecret: secret, Images: images, NoSpot: o.onDemand,
	}, runID)
	if err != nil {
		return err
	}
	api := imageAPI{rpc: aliyundriver.NewRPC(), creds: serverprovider.Credentials{PublicKey: keyID, PrivateKey: secret}}

	switch {
	case o.listOnly:
		return listPlacements(ctx, provider, o)
	case o.copyImage != "":
		if o.copyFrom == "" {
			return errors.New("-copy-image needs -copy-from")
		}
		if o.validate {
			if err := validateImage(ctx, keyID, secret, o.copyFrom, o.copyImage, runID, o); err != nil {
				return fmt.Errorf("validate the image in %s: %w", o.copyFrom, err)
			}
		}
		ids, err := copyEverywhere(ctx, api, o, o.copyFrom, o.copyImage, o.imageName)
		if err != nil {
			return err
		}
		printResult(o, ids)
		return nil
	}
	return build(ctx, provider, api, o, runID, keyID, secret)
}

func listPlacements(ctx context.Context, provider *cloudprovider.Aliyun, o options) error {
	for _, count := range []int{1, 2} {
		for _, model := range cloudprovider.AliyunGPUModels() {
			best, err := provider.SelectPlacement(ctx, model, count)
			if err != nil {
				fmt.Printf("%-4s x%d  none: %v\n", model, count, err)
				continue
			}
			fmt.Printf("%-4s x%d  best %s (%s)\n", model, count, best, best.Stock)
		}
	}
	return nil
}

// sourceVM is the VM the image is made from.
type sourceVM struct{ instance *cloudprovider.Instance }

func (s sourceVM) wait(ctx context.Context) error {
	return sshutil.WaitSSHReadyPasswordAs(ctx, s.instance.PublicIP, s.instance.SSHPort, s.instance.Username, s.instance.Password)
}

func (s sourceVM) run(ctx context.Context, command string) (string, error) {
	return sshutil.RunCommandPassword(ctx, s.instance.PublicIP, s.instance.SSHPort, s.instance.Username, s.instance.Password, command)
}

func build(ctx context.Context, provider *cloudprovider.Aliyun, api imageAPI, o options, runID, keyID, secret string) (err error) {
	var files []goldenimage.File
	if !o.baseGolden {
		if files, err = goldenimage.ModelFiles(ctx, o.modelRepo, o.modelRev); err != nil {
			return err
		}
	}
	var source *cloudprovider.Instance
	if o.resume != "" {
		if source, err = readCredentials(o.resume); err != nil {
			return err
		}
		fmt.Printf("run_id=%s phase=resume instance=%s region=%s\n", runID, source.InstanceID, source.Region)
	} else {
		fmt.Printf("run_id=%s phase=rent-source gpu=%s on_demand=%t\n", runID, o.gpuModel, o.onDemand)
		source, err = provider.Rent(ctx, cloudprovider.GPUSpec{GPUModel: o.gpuModel, GPUCount: 1, DiskGB: o.diskGB})
		for _, line := range provider.Attempts() {
			fmt.Printf("run_id=%s attempt: %s\n", runID, line)
		}
		if err != nil {
			return fmt.Errorf("rent source VM: %w", err)
		}
		fmt.Printf("run_id=%s source=%s region=%s zone=%s type=%s billing=%s\n", runID, source.InstanceID, source.Region, source.Zone, source.InstanceType, source.Billing)
	}
	// The source VM is released on success and on failure, unless the operator
	// asked to keep a failed one for investigation; then its credentials go to
	// a 0600 file and the next run can resume from it.
	released := false
	defer func() {
		if released {
			return
		}
		if err != nil && os.Getenv("GPUWORKS_CLOUD_KEEP_ON_FAILURE") == "1" {
			path, saveErr := saveCredentials(runID, source)
			fmt.Fprintf(os.Stderr, "GPUWORKS_CLOUD_KEEP_ON_FAILURE=1: keeping source VM %s in %s (credentials %s, err %v); resume with -resume-credentials %s, release with cleanup-leaked --run-id %s\n",
				source.InstanceID, source.Region, path, saveErr, path, runID)
			return
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 4*time.Minute)
		defer cleanupCancel()
		if destroyErr := provider.Destroy(cleanupCtx, source.InstanceID); destroyErr != nil {
			fmt.Fprintln(os.Stderr, "release source VM:", destroyErr)
		}
	}()
	vm := sourceVM{instance: source}
	if err := vm.wait(ctx); err != nil {
		return fmt.Errorf("source VM SSH: %w", err)
	}
	for _, step := range buildSteps(o, files) {
		fmt.Printf("run_id=%s phase=%s\n", runID, step.phase)
		stepCtx, stepCancel := context.WithTimeout(ctx, step.timeout)
		out, stepErr := vm.run(stepCtx, step.command)
		stepCancel()
		for _, line := range evidenceLines(out) {
			fmt.Printf("run_id=%s evidence: %s\n", runID, line)
		}
		if stepErr != nil {
			return fmt.Errorf("%s: %w: %s", step.phase, stepErr, tail(out, 1500))
		}
	}
	fmt.Printf("run_id=%s phase=quiesce\n", runID)
	if out, finishErr := vm.run(ctx, finishCommand()); finishErr != nil {
		return fmt.Errorf("quiesce source VM: %w: %s", finishErr, tail(out, 800))
	}

	fmt.Printf("run_id=%s phase=create-image name=%s\n", runID, o.imageName)
	imageID, err := api.create(ctx, source.Region, source.InstanceID, o.imageName, imageDescription(o))
	if err != nil {
		return err
	}
	progress := func(line string) { fmt.Printf("run_id=%s phase=wait-image %s\n", runID, line) }
	if err := api.wait(ctx, source.Region, imageID, 20*time.Second, progress); err != nil {
		return fmt.Errorf("wait for custom image: %w (image_id=%s)", err, imageID)
	}
	releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 4*time.Minute)
	if err := provider.Destroy(releaseCtx, source.InstanceID); err != nil {
		releaseCancel()
		return fmt.Errorf("release source VM: %w", err)
	}
	releaseCancel()
	released = true

	// Validate before copying: a broken image is not worth copying to four
	// regions (a copy of a 150 GiB disk takes the better part of an hour).
	if o.validate {
		if err := validateImage(ctx, keyID, secret, source.Region, imageID, runID, o); err != nil {
			fmt.Printf("# image %s in %s FAILED validation; no copies were made\n", imageID, source.Region)
			return fmt.Errorf("validate the image in %s: %w", source.Region, err)
		}
	}
	ids, err := copyEverywhere(ctx, api, o, source.Region, imageID, o.imageName)
	if err != nil {
		return err
	}
	printResult(o, ids)
	return nil
}

type buildStep struct {
	phase   string
	command string
	timeout time.Duration
}

// buildSteps are the commands run on the source VM in order. On an earlier
// golden image the base is checked first, so a broken base fails before the
// extra image is pulled onto it.
func buildSteps(o options, files []goldenimage.File) []buildStep {
	var steps []buildStep
	if o.baseGolden {
		steps = append(steps, buildStep{"validate-base", bootedChecksCommand(o.modelPath, o.dockerImage, nil, nil), 10 * time.Minute})
		if len(o.removeImages) > 0 {
			steps = append(steps, buildStep{"remove-images", removeImagesCommand(o.removeImages), 10 * time.Minute})
		}
		steps = append(steps, buildStep{"docker-cgroupfs", dockerCgroupfsCommand(), 5 * time.Minute})
	} else {
		steps = append(steps,
			buildStep{"install-docker", installDockerCommand(), 40 * time.Minute},
			buildStep{"docker-cgroupfs", dockerCgroupfsCommand(), 5 * time.Minute},
			buildStep{"pull-runner-image", pullImageCommand(o.dockerImage, o.pullSources), 60 * time.Minute},
			buildStep{"prepare-model", goldenimage.PrepareModelCommand(o.modelPath, o.dockerImage, files, ""), 30 * time.Minute},
		)
	}
	for _, image := range o.extraImages {
		steps = append(steps, buildStep{"pull-extra-image", pullImageCommand(image, o.extraSources(image)), 60 * time.Minute})
	}
	return append(steps, buildStep{"validate-source", bootedChecksCommand(o.modelPath, o.dockerImage, o.extraImages, o.removeImages) + cgroupfsCheck, 10 * time.Minute})
}

// copyEverywhere copies the image to every other region in parallel and waits
// until each copy is Available. The returned map holds the source region too.
func copyEverywhere(ctx context.Context, api imageAPI, o options, srcRegion, imageID, name string) (map[string]string, error) {
	ids := map[string]string{srcRegion: imageID}
	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		failures []string
	)
	for _, region := range o.regions {
		if region == srcRegion {
			continue
		}
		wg.Add(1)
		go func(region string) {
			defer wg.Done()
			fail := func(err error) {
				mu.Lock()
				failures = append(failures, err.Error())
				mu.Unlock()
			}
			fmt.Printf("phase=copy-image from=%s to=%s\n", srcRegion, region)
			copyID, err := api.copy(ctx, srcRegion, imageID, region, name, "gpuworks L4 golden image (copy)")
			if err != nil {
				fail(err)
				return
			}
			mu.Lock()
			ids[region] = copyID
			mu.Unlock()
			progress := func(line string) { fmt.Printf("phase=wait-copy %s\n", line) }
			if err := api.wait(ctx, region, copyID, o.copyPollEvery(), progress); err != nil {
				fail(err)
			}
		}(region)
	}
	wg.Wait()
	if len(failures) > 0 {
		sort.Strings(failures)
		printResult(o, ids)
		return ids, fmt.Errorf("copy to %d region(s) failed: %s", len(failures), strings.Join(failures, "; "))
	}
	return ids, nil
}

// validateImage boots the new image once and runs the same checks the source
// VM passed.
func validateImage(ctx context.Context, keyID, secret, region, imageID, runID string, o options) error {
	fmt.Printf("run_id=%s phase=validate-image region=%s image=%s\n", runID, region, imageID)
	provider, err := cloudprovider.NewAliyun(cloudprovider.AliyunConfig{
		AccessKeyID: keyID, AccessKeySecret: secret, Images: map[string]string{region: imageID}, NoSpot: o.onDemand,
	}, runID)
	if err != nil {
		return err
	}
	instance, err := provider.Rent(ctx, cloudprovider.GPUSpec{GPUModel: o.gpuModel, GPUCount: 1, DiskGB: o.diskGB})
	if err != nil {
		return fmt.Errorf("rent validation VM: %w", err)
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
		defer cancel()
		if destroyErr := provider.Destroy(cleanupCtx, instance.InstanceID); destroyErr != nil {
			fmt.Fprintln(os.Stderr, "release validation VM:", destroyErr)
		}
	}()
	vm := sourceVM{instance: instance}
	if err := vm.wait(ctx); err != nil {
		return err
	}
	out, err := vm.run(ctx, bootedChecksCommand(o.modelPath, o.dockerImage, o.extraImages, o.removeImages)+cgroupfsCheck)
	if err != nil {
		return fmt.Errorf("%w: %s", err, tail(out, 1500))
	}
	for _, line := range evidenceLines(out) {
		fmt.Printf("run_id=%s validation: %s\n", runID, line)
	}
	return nil
}

func printResult(o options, ids map[string]string) {
	fmt.Println("# Add to secrets/cloud-providers.env (or the CI environment variables):")
	regions := make([]string, 0, len(ids))
	for region := range ids {
		regions = append(regions, region)
	}
	sort.Strings(regions)
	for _, region := range regions {
		fmt.Printf("%s=%s\n", envVarName(region), ids[region])
	}
	fmt.Printf("RESULT_MODEL_PATH=%s\n", o.modelPath)
}

func evidenceLines(out string) []string {
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "GPUWORKS_") || strings.Contains(line, "installed from") || strings.HasPrefix(line, "GPU 0:") {
			lines = append(lines, line)
		}
	}
	return lines
}

func tail(out string, n int) string {
	out = strings.TrimSpace(out)
	if len(out) > n {
		return "..." + out[len(out)-n:]
	}
	return out
}

type credentialsFile struct {
	InstanceID string `json:"instance_id"`
	Region     string `json:"region"`
	Host       string `json:"host"`
	PrivateIP  string `json:"private_ip"`
	Password   string `json:"password"`
	Zone       string `json:"zone"`
	Type       string `json:"type"`
}

func saveCredentials(runID string, in *cloudprovider.Instance) (string, error) {
	path := filepath.Join(os.TempDir(), "gpuworks-aliyun-golden-"+runID+".json")
	data, err := json.Marshal(credentialsFile{InstanceID: in.InstanceID, Region: in.Region, Host: in.PublicIP, PrivateIP: in.PrivateIP, Password: in.Password, Zone: in.Zone, Type: in.InstanceType})
	if err != nil {
		return path, err
	}
	return path, os.WriteFile(path, data, 0o600)
}

func readCredentials(path string) (*cloudprovider.Instance, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c credentialsFile
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	if c.InstanceID == "" || c.Host == "" || c.Password == "" {
		return nil, fmt.Errorf("%s is not a source VM credentials file", path)
	}
	return &cloudprovider.Instance{
		InstanceID: c.InstanceID, Region: c.Region, PublicIP: c.Host, PrivateIP: c.PrivateIP,
		SSHPort: 22, Username: "root", Password: c.Password, Zone: c.Zone, InstanceType: c.Type, Kind: "vm",
	}, nil
}
