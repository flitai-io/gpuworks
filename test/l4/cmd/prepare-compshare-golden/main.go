// Command prepare-compshare-golden creates and validates a CompShare custom
// image containing the Docker and model fixtures used by explicit L4 tests.
// It never changes CI variables or deletes an older custom image.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	serverprovider "gpuworks.ai/gpuworks/internal/server/provider"
	compsharedriver "gpuworks.ai/gpuworks/internal/server/provider/compshare"
	"gpuworks.ai/gpuworks/internal/testenv/cloudprovider"
	"gpuworks.ai/gpuworks/internal/testenv/goldenimage"
	"gpuworks.ai/gpuworks/internal/testenv/sshutil"
)

// The model fixture and its checks are shared with the other providers'
// golden-image builders.
const (
	defaultModelPath = goldenimage.DefaultModelPath
	// defaultDockerImage is the CUDA 12.9 runner: the image's NVIDIA driver
	// (nvidiaDriverVersion, R570) runs CUDA 12.x through minor version
	// compatibility but not the CUDA 13.0 builds, which need R580.
	defaultDockerImage = goldenimage.TuringDockerImage
	// defaultPullSources are tried in order when the source image does not
	// carry -docker-image yet; the final tag is -docker-image.
	defaultPullSources = "swr.cn-southwest-2.myhuaweicloud.com/" + defaultDockerImage + ",quay.io/" + defaultDockerImage + "," + defaultDockerImage
)

type remoteFile = goldenimage.File

func main() {
	var (
		modelPath   = flag.String("model-path", defaultModelPath, "absolute model path stored in the custom image")
		dockerImage = flag.String("docker-image", defaultDockerImage, "runner image the new image carries; pulled from -pull-sources when the source image lacks it")
		pull        = flag.String("pull-sources", defaultPullSources, "registry references tried in order to obtain -docker-image")
		remove      = flag.String("remove-images", "", "comma-separated images to delete from the source image's Docker store (a runner the new image replaces)")
		gpuModel    = flag.String("gpu-model", "RTX4090", "GPU model used by temporary validation VMs")
		diskGB      = flag.Int("disk-gb", 160, "system disk size for temporary VMs")
		imageName   = flag.String("name", "", "new custom image name")
		resumeName  = flag.String("resume-name", "", "resume and validate an existing image task with this exact name")
		resumeRunID = flag.String("resume-run-id", "", "run-id that owns the source VM of a resumed image task")
		sourceID    = flag.String("source-instance-id", "", "source VM to clean after a resumed image becomes Available")
		region      = flag.String("region", "", "region for the source VM and the new image (empty = provider default cn-wlcb)")
		zone        = flag.String("zone", "", "zone for the source VM and the new image (empty = provider default)")
		gpuCount    = flag.Int("gpu-count", 1, "GPU count for temporary VMs; some regions only offer wider shapes (e.g. cn-sh2-02 2080Ti x4)")
		sourceImg   = flag.String("source-image", "", "override the source image id (defaults to COMPSHARE_GOLDEN_IMAGE_ID / COMPSHARE_IMAGE_ID); needed in a region the golden image cannot reach")
		dualKmod    = flag.Bool("dual-kmod", true, "build and store both the open and the proprietary NVIDIA kernel modules and pick one at boot")
		gpus        = flag.String("validate-gpus", "V100S,P40,5090,A800,H20,A100,4090", "GPU types to boot the new image on after it validates; types without stock or refused by CompShare are reported, not failed")
		checkImage  = flag.String("validate-image", "", "only boot this existing image on -validate-gpus (no new image is made)")
	)
	flag.Parse()
	useDualKmod = *dualKmod
	validateGPUs = strings.Split(*gpus, ",")
	var err error
	if strings.TrimSpace(*checkImage) != "" {
		err = checkExistingImage(strings.TrimSpace(*checkImage), *modelPath, *dockerImage, *diskGB)
	} else if strings.TrimSpace(*resumeName) != "" {
		err = resume(*modelPath, *dockerImage, *gpuModel, *resumeName, *resumeRunID, *sourceID, *diskGB)
	} else {
		pullSources, removeImages = splitList(*pull), splitList(*remove)
		err = run(*modelPath, *dockerImage, *gpuModel, *imageName, *diskGB, *region, *zone, *gpuCount, *sourceImg)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "prepare CompShare golden image:", err)
		os.Exit(1)
	}
}

func run(modelPath, dockerImage, gpuModel, imageName string, diskGB int, region, zone string, gpuCount int, sourceImageOverride string) error {
	accessKey := strings.TrimSpace(os.Getenv("COMPSHARE_ACCESS_KEY"))
	secretKey := strings.TrimSpace(os.Getenv("COMPSHARE_SECRET_KEY"))
	sourceImage := firstNonEmpty(sourceImageOverride, os.Getenv("COMPSHARE_GOLDEN_IMAGE_ID"), os.Getenv("COMPSHARE_IMAGE_ID"))
	if accessKey == "" || secretKey == "" || sourceImage == "" {
		return errors.New("COMPSHARE_ACCESS_KEY, COMPSHARE_SECRET_KEY and COMPSHARE_GOLDEN_IMAGE_ID (or COMPSHARE_IMAGE_ID) are required")
	}
	if !strings.HasPrefix(strings.TrimSpace(modelPath), "/") {
		return errors.New("model-path must be absolute")
	}
	if diskGB < 160 {
		return fmt.Errorf("disk-gb=%d is too small; golden image tests require at least 160 GiB", diskGB)
	}
	if strings.TrimSpace(imageName) == "" {
		imageName = "gpuworks-golden-qwen3-0-6b-" + time.Now().UTC().Format("20060102-150405")
	}
	// CompShare answered a 50-character name with "RetCode 230: Params [Name]
	// not available" after the source VM was fully prepared; the default
	// 42-character form is known to work. Fail before renting anything.
	if len(imageName) > maxImageNameLen {
		return fmt.Errorf("image name %q is %d characters; CompShare rejects long names, keep it to %d", imageName, len(imageName), maxImageNameLen)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Minute)
	defer cancel()
	files, err := modelFiles(ctx)
	if err != nil {
		return err
	}
	runID, err := cloudprovider.NewRunID()
	if err != nil {
		return err
	}
	runID = "golden-" + runID
	provider, err := cloudprovider.NewCompshare(accessKey, secretKey, sourceImage, runID)
	if err != nil {
		return err
	}
	creds := serverprovider.Credentials{PublicKey: accessKey, PrivateKey: secretKey}
	driver := compsharedriver.New()

	fmt.Printf("run_id=%s phase=rent-source\n", runID)
	source, err := provider.Rent(ctx, cloudprovider.GPUSpec{GPUModel: gpuModel, GPUCount: gpuCount, DiskGB: diskGB, Region: region, Zone: zone})
	if err != nil {
		return fmt.Errorf("rent source VM: %w", err)
	}
	sourceAlive := true
	defer func() {
		if sourceAlive {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cleanupCancel()
			if cleanupErr := provider.Destroy(cleanupCtx, source.InstanceID); cleanupErr != nil {
				fmt.Fprintln(os.Stderr, "cleanup source VM:", cleanupErr)
			}
		}
	}()
	if err := waitSSH(ctx, source); err != nil {
		return fmt.Errorf("source VM SSH: %w", err)
	}
	if useDualKmod {
		fmt.Printf("run_id=%s phase=dual-kmod driver=%s\n", runID, nvidiaDriverVersion)
		if out, err := runSSH(ctx, source, remoteDualKmodCommand()); err != nil {
			return fmt.Errorf("install dual NVIDIA kernel modules: %w: %s", err, strings.TrimSpace(out))
		}
	}
	fmt.Printf("run_id=%s phase=pull-runner-image image=%s\n", runID, dockerImage)
	if out, err := runSSH(ctx, source, goldenimage.PullImageCommand(dockerImage, pullSources, "sudo -n ")); err != nil {
		return fmt.Errorf("pull runner image: %w: %s", err, strings.TrimSpace(out))
	}
	if len(removeImages) > 0 {
		fmt.Printf("run_id=%s phase=remove-images images=%s\n", runID, strings.Join(removeImages, ","))
		if out, err := runSSH(ctx, source, goldenimage.RemoveImagesCommand(removeImages, dockerImage, "sudo -n ")); err != nil {
			return fmt.Errorf("remove replaced images: %w: %s", err, strings.TrimSpace(out))
		}
	}
	fmt.Printf("run_id=%s phase=prepare-model files=%d\n", runID, len(files))
	if _, err := runSSH(ctx, source, remotePrepareCommand(modelPath, dockerImage, files)); err != nil {
		return fmt.Errorf("prepare source VM: %w", err)
	}
	if _, err := runSSH(ctx, source, remoteValidationCommand(modelPath, dockerImage)); err != nil {
		return fmt.Errorf("validate source VM fixtures: %w", err)
	}

	fmt.Printf("run_id=%s phase=create-image name=%s\n", runID, imageName)
	imageID, err := driver.CreateCustomImage(ctx, creds, compsharedriver.CustomImageRequest{
		InstanceID:  source.InstanceID,
		Name:        imageName,
		Description: "gpuworks L4: preloaded vLLM image and Qwen3-0.6B",
		Region:      region,
		Zone:        zone,
	})
	if err != nil {
		return err
	}
	fmt.Printf("run_id=%s phase=wait-image\n", runID)
	if err := waitImage(ctx, driver, creds, imageID); err != nil {
		return fmt.Errorf("wait for custom image: %w (image_id=%s)", err, imageID)
	}
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 3*time.Minute)
	if err := provider.Destroy(cleanupCtx, source.InstanceID); err != nil {
		cleanupCancel()
		return fmt.Errorf("destroy source VM: %w", err)
	}
	cleanupCancel()
	sourceAlive = false

	return validateImage(ctx, accessKey, secretKey, imageID, runID, modelPath, dockerImage, gpuModel, diskGB, region, zone, gpuCount)
}

func resume(modelPath, dockerImage, gpuModel, imageName, runID, sourceID string, diskGB int) error {
	accessKey := strings.TrimSpace(os.Getenv("COMPSHARE_ACCESS_KEY"))
	secretKey := strings.TrimSpace(os.Getenv("COMPSHARE_SECRET_KEY"))
	if accessKey == "" || secretKey == "" || strings.TrimSpace(runID) == "" || strings.TrimSpace(sourceID) == "" {
		return errors.New("resume requires CompShare credentials, -resume-run-id and -source-instance-id")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()
	creds := serverprovider.Credentials{PublicKey: accessKey, PrivateKey: secretKey}
	driver := compsharedriver.New()
	images, err := driver.ListCustomImages(ctx, creds, "", "")
	if err != nil {
		return err
	}
	var found compsharedriver.CustomImage
	for _, image := range images {
		if image.Name == imageName && image.CreateTime >= found.CreateTime {
			found = image
		}
	}
	if found.ID == "" {
		return fmt.Errorf("custom image named %q not found", imageName)
	}
	fmt.Printf("run_id=%s phase=resume-image status=%s\n", runID, found.Status)
	if err := waitImage(ctx, driver, creds, found.ID); err != nil {
		return err
	}
	owner, err := cloudprovider.NewCompshare(accessKey, secretKey, "", runID)
	if err != nil {
		return err
	}
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 3*time.Minute)
	err = owner.Destroy(cleanupCtx, sourceID)
	cleanupCancel()
	if err != nil {
		return fmt.Errorf("destroy resumed source VM: %w", err)
	}
	return validateImage(ctx, accessKey, secretKey, found.ID, runID, modelPath, dockerImage, gpuModel, diskGB, "", "", 1)
}

func validateImage(ctx context.Context, accessKey, secretKey, imageID, runID, modelPath, dockerImage, gpuModel string, diskGB int, region, zone string, gpuCount int) error {
	fmt.Printf("run_id=%s phase=rent-validation\n", runID)
	validator, err := cloudprovider.NewCompshare(accessKey, secretKey, imageID, runID)
	if err != nil {
		return err
	}
	validation, err := validator.Rent(ctx, cloudprovider.GPUSpec{GPUModel: gpuModel, GPUCount: gpuCount, DiskGB: diskGB, Region: region, Zone: zone})
	if err != nil {
		return fmt.Errorf("rent validation VM from new image %s: %w", imageID, err)
	}
	validationAlive := true
	defer func() {
		if validationAlive {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cleanupCancel()
			if cleanupErr := validator.Destroy(cleanupCtx, validation.InstanceID); cleanupErr != nil {
				fmt.Fprintln(os.Stderr, "cleanup validation VM:", cleanupErr)
			}
		}
	}()
	if err := waitSSH(ctx, validation); err != nil {
		return fmt.Errorf("validation VM SSH: %w", err)
	}
	if _, err := runSSH(ctx, validation, remoteValidationCommand(modelPath, dockerImage)); err != nil {
		return fmt.Errorf("new image fixture validation: %w", err)
	}
	if useDualKmod {
		if out, err := runSSH(ctx, validation, remoteKmodValidationCommand(dockerImage)); err != nil {
			return fmt.Errorf("new image NVIDIA module validation: %w: %s", err, strings.TrimSpace(out))
		}
	}
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 3*time.Minute)
	if err := validator.Destroy(cleanupCtx, validation.InstanceID); err != nil {
		cleanupCancel()
		return fmt.Errorf("destroy validation VM: %w", err)
	}
	cleanupCancel()
	validationAlive = false
	fmt.Printf("RESULT_IMAGE_ID=%s\n", imageID)
	fmt.Printf("RESULT_MODEL_PATH=%s\n", modelPath)
	// The GPU matrix rents one VM per type after the image exists; it gets
	// its own budget rather than what is left of the build's.
	matrixCtx, matrixCancel := context.WithTimeout(context.Background(), 4*time.Hour)
	defer matrixCancel()
	printGPUChecks(validateOnGPUs(matrixCtx, accessKey, secretKey, imageID, runID, modelPath, dockerImage, diskGB, validateGPUs))
	return nil
}

// maxImageNameLen is the longest custom image name CompShare has accepted.
const maxImageNameLen = 42

// Set from flags; module-level so resume and validation share them.
var (
	useDualKmod  = true
	validateGPUs []string
	pullSources  = splitList(defaultPullSources)
	removeImages []string
)

func splitList(raw string) []string {
	var out []string
	for _, item := range strings.Split(raw, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

func checkExistingImage(imageID, modelPath, dockerImage string, diskGB int) error {
	accessKey := strings.TrimSpace(os.Getenv("COMPSHARE_ACCESS_KEY"))
	secretKey := strings.TrimSpace(os.Getenv("COMPSHARE_SECRET_KEY"))
	if accessKey == "" || secretKey == "" {
		return errors.New("COMPSHARE_ACCESS_KEY and COMPSHARE_SECRET_KEY are required")
	}
	runID, err := cloudprovider.NewRunID()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Hour)
	defer cancel()
	printGPUChecks(validateOnGPUs(ctx, accessKey, secretKey, imageID, "golden-check-"+runID, modelPath, dockerImage, diskGB, validateGPUs))
	return nil
}

func printGPUChecks(results []gpuCheck) {
	for _, r := range results {
		fmt.Printf("RESULT_GPU %-8s %-15s %s\n", r.GPU, r.Outcome, r.Detail)
	}
}

func modelFiles(ctx context.Context) ([]remoteFile, error) {
	return goldenimage.ModelFiles(ctx, goldenimage.DefaultModelRepo, goldenimage.DefaultModelRev)
}

func waitSSH(ctx context.Context, instance *cloudprovider.Instance) error {
	if len(instance.PrivateKey) > 0 {
		return sshutil.WaitSSHReadyAs(ctx, instance.PublicIP, instance.SSHPort, instance.Username, instance.PrivateKey)
	}
	return sshutil.WaitSSHReadyPasswordAs(ctx, instance.PublicIP, instance.SSHPort, instance.Username, instance.Password)
}

func runSSH(ctx context.Context, instance *cloudprovider.Instance, command string) (string, error) {
	if len(instance.PrivateKey) > 0 {
		return sshutil.RunCommand(ctx, instance.PublicIP, instance.SSHPort, instance.Username, instance.PrivateKey, command)
	}
	return sshutil.RunCommandPassword(ctx, instance.PublicIP, instance.SSHPort, instance.Username, instance.Password, command)
}

type customImageClient interface {
	CustomImageProgress(context.Context, serverprovider.Credentials, string, string, string) (float64, error)
	ListCustomImages(context.Context, serverprovider.Credentials, string, string) ([]compsharedriver.CustomImage, error)
}

func waitImage(ctx context.Context, driver customImageClient, creds serverprovider.Credentials, imageID string) error {
	return waitImageWithInterval(ctx, driver, creds, imageID, 20*time.Second)
}

func waitImageWithInterval(ctx context.Context, driver customImageClient, creds serverprovider.Credentials, imageID string, interval time.Duration) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		progress, progressErr := driver.CustomImageProgress(ctx, creds, imageID, "", "")
		images, listErr := driver.ListCustomImages(ctx, creds, imageID, "")
		status := "unknown"
		if listErr == nil && len(images) == 1 {
			status = images[0].Status
		}
		if progressErr == nil {
			fmt.Printf("phase=wait-image progress=%.1f status=%s\n", progress, status)
		}
		if strings.EqualFold(status, "Available") {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func remotePrepareCommand(modelPath, dockerImage string, files []remoteFile) string {
	return goldenimage.PrepareModelCommand(modelPath, dockerImage, files, "sudo -n ")
}

func remoteValidationCommand(modelPath, dockerImage string) string {
	return goldenimage.ValidationCommand(modelPath, dockerImage, "sudo -n ")
}

func shellQuote(value string) string { return goldenimage.ShellQuote(value) }

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
