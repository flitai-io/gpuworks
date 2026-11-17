package selfinstall

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/distribution"
	"gpuworks.ai/gpuworks/internal/version"
)

// channelSource reads the release a distribution channel offers (ADR 0040):
// every source is asked for the signed index at once, and the archive is
// downloaded from the source that answered, falling back to the others and
// resuming where a transfer stopped. Without --channel the installation's
// update_channel applies, then the channel the binary was built for.
func (i *Installer) channelSource(ctx context.Context, sources []string, channel, configured, pin string) (upgradeSource, error) {
	channel = strings.TrimSpace(channel)
	if channel == "" {
		channel = strings.TrimSpace(configured)
	}
	if channel == "" {
		channel = version.Channel
	}
	if !distribution.KnownChannel(channel) {
		return upgradeSource{}, fmt.Errorf("--channel %q is not one of %s", channel, strings.Join(distribution.Channels, ", "))
	}
	client, err := i.deps.channelClient(sources)
	if err != nil {
		return upgradeSource{}, err
	}
	fetched, err := client.FetchIndex(ctx, channel)
	if err != nil {
		return upgradeSource{}, err
	}
	release := fetched.Index.Server
	if pin != "" {
		selected, ok := fetched.Index.FindServer(pin)
		if !ok {
			return upgradeSource{}, fmt.Errorf("--version %s is not what the %s channel offers: it is at %s", pin, channel, release.ReleaseID)
		}
		release = selected
	}
	goos, goarch := currentPlatform()
	asset, err := release.Asset(goos, goarch)
	if err != nil {
		return upgradeSource{}, err
	}
	return upgradeSource{
		ReleaseID: release.ReleaseID, Commit: release.Commit, BuiltAt: release.BuiltAt, Source: fetched.Source,
		newerThan: release.NewerThan,
		pinMismatch: func(pin string) error {
			return fmt.Errorf("--version %s is not what the %s channel offers: it is at %s", pin, channel, release.ReleaseID)
		},
		stage: func(ctx context.Context) (string, error) {
			if err := mkdirAll(i.layout.TmpDir(), dirMode); err != nil {
				return "", err
			}
			name := path.Base(asset.Path)
			archive := filepath.Join(i.layout.TmpDir(), name)
			i.io.Say("downloading %s (%s) from %s", name, humanBytes(asset.Size), fetched.Source)
			report := newProgressReport(i.io, fetched.Source)
			if _, err := client.Download(ctx, fetched, asset.File, archive, report.update); err != nil {
				return "", err
			}
			defer func() { _ = os.Remove(archive) }()
			staged := i.stagedBinary()
			if err := extractBinary(archive, staged); err != nil {
				return "", err
			}
			if pin != "" {
				build, err := i.binaryBuild(ctx, staged)
				if err != nil {
					return "", err
				}
				if build.ReleaseID != pin {
					return "", fmt.Errorf("selected %s but the archive contains %s", pin, build.ReleaseID)
				}
			}
			return staged, nil
		},
	}, nil
}

// progressReport narrates a download in quarters, and says so when the
// transfer moves to another source (it resumes there from the bytes already
// on disk).
type progressReport struct {
	io      *IO
	source  string
	quarter int64
}

func newProgressReport(io *IO, source string) *progressReport {
	return &progressReport{io: io, source: source}
}

func (r *progressReport) update(p distribution.Progress) {
	if p.Source != r.source {
		r.source = p.Source
		r.io.Say("  switched to %s", p.Source)
	}
	if p.Total <= 0 {
		return
	}
	if quarter := p.Done * 4 / p.Total; quarter > r.quarter {
		r.quarter = quarter
		r.io.Say("  %d%% of %s", quarter*25, humanBytes(p.Total))
	}
}

// humanBytes renders a size the way the progress lines read it.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	value, suffix := float64(n), "KiB"
	for _, next := range []string{"MiB", "GiB"} {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
		suffix = next
	}
	return fmt.Sprintf("%.1f %s", value/unit, suffix)
}
