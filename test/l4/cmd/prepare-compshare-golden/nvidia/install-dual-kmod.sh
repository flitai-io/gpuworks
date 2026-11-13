#!/bin/sh
# Install NVIDIA driver @VERSION@, build its open and proprietary kernel
# modules, store both, and install the boot-time selector. Runs as root on
# the golden source VM. When the source image carries another driver, its
# userspace is replaced first: the kernel modules and the libraries have to
# match exactly.
set -eu
version="@VERSION@"
sha256="@SHA256@"
url="https://download.nvidia.com/XFree86/Linux-x86_64/$version/NVIDIA-Linux-x86_64-$version.run"
store="@STORE@"
kernel="$(uname -r)"
work="$(mktemp -d /tmp/gpuworks-nvidia-kmod.XXXXXX)"
trap 'rm -rf "$work"' EXIT

test -f "/lib/modules/$kernel/build/Makefile" || { echo "kernel headers for $kernel are missing" >&2; exit 1; }

curl -fsSL --retry 3 --connect-timeout 30 -o "$work/driver.run" "$url"
echo "$sha256  $work/driver.run" | sha256sum -c -
sh "$work/driver.run" -x --target "$work/driver" >/dev/null

installed="$(nvidia-smi --query-gpu=driver_version --format=csv,noheader 2>/dev/null | head -1 || true)"
if [ "$installed" != "$version" ]; then
	echo "replacing NVIDIA driver '$installed' with $version"
	systemctl stop docker.service containerd.service nvidia-persistenced.service 2>/dev/null || true
	for module in nvidia_peermem nvidia_drm nvidia_modeset nvidia_uvm nvidia; do
		rmmod "$module" 2>/dev/null || true
	done
	# A packaged driver would shadow the runfile's libraries.
	packages="$(dpkg-query -W -f '${Package} ${Status}\n' 2>/dev/null | awk '/ installed$/ && $1 !~ /^libnvidia-container/ && $1 ~ /^(nvidia-driver-|libnvidia-|nvidia-dkms-|nvidia-kernel-|nvidia-utils-|nvidia-compute-utils-)/ {print $1}')"
	if [ -n "$packages" ]; then
		# shellcheck disable=SC2086
		DEBIAN_FRONTEND=noninteractive apt-get purge -y $packages >"$work/apt-purge.log" 2>&1 || { tail -20 "$work/apt-purge.log" >&2; exit 1; }
	fi
	# Userspace only: the selector installs the kernel modules built below.
	# The runfile uninstalls an earlier runfile installation itself.
	"$work/driver/nvidia-installer" --silent --no-kernel-modules --no-x-check --no-nouveau-check >"$work/installer.log" 2>&1 || {
		tail -40 "$work/installer.log" /var/log/nvidia-installer.log >&2 2>/dev/null
		exit 1
	}
fi

for flavour in open proprietary; do
	source="$work/driver/kernel"
	[ "$flavour" = open ] && source="$work/driver/kernel-open"
	make -C "$source" SYSSRC="/lib/modules/$kernel/build" -j"$(nproc)" modules >"$work/build-$flavour.log" 2>&1 || {
		tail -40 "$work/build-$flavour.log" >&2
		exit 1
	}
	rm -rf "$store/$flavour"
	mkdir -p "$store/$flavour"
	for module in nvidia nvidia-uvm nvidia-modeset nvidia-drm nvidia-peermem; do
		if [ -f "$source/$module.ko" ]; then
			install -m 0644 "$source/$module.ko" "$store/$flavour/"
		fi
	done
	test -f "$store/$flavour/nvidia.ko"
	modinfo -F license "$store/$flavour/nvidia.ko"
done
echo "$version" >"$store/VERSION"

# Containers reach the GPU through nvidia-container-toolkit. The cn-wlcb
# golden lineage has it; the cn-sh2 one shipped Docker without it, so
# `docker run --gpus all` failed there. Install the same version when it is
# missing (NVIDIA's apt repository, or the USTC mirror when that is
# unreachable) and point Docker at the nvidia runtime.
if command -v docker >/dev/null 2>&1; then
	if ! command -v nvidia-ctk >/dev/null 2>&1; then
		# The cn-sh2 image has no sources.list.d at all.
		mkdir -p /etc/apt/sources.list.d /usr/share/keyrings
		curl -fsSL --retry 3 --connect-timeout 20 https://nvidia.github.io/libnvidia-container/gpgkey |
			gpg --batch --yes --dearmor -o /usr/share/keyrings/nvidia-container-toolkit-keyring.gpg
		toolkit_ok=
		for repo in https://nvidia.github.io/libnvidia-container/stable/deb https://mirrors.ustc.edu.cn/libnvidia-container/stable/deb; do
			echo "deb [signed-by=/usr/share/keyrings/nvidia-container-toolkit-keyring.gpg] $repo/\$(ARCH) /" >/etc/apt/sources.list.d/nvidia-container-toolkit.list
			if apt-get update -o Dir::Etc::sourcelist=sources.list.d/nvidia-container-toolkit.list -o Dir::Etc::sourceparts=- -o APT::Get::List-Cleanup=0 >"$work/apt-update.log" 2>&1 &&
				DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
					nvidia-container-toolkit="@TOOLKIT@" nvidia-container-toolkit-base="@TOOLKIT@" \
					libnvidia-container-tools="@TOOLKIT@" libnvidia-container1="@TOOLKIT@" >"$work/apt-install.log" 2>&1; then
				toolkit_ok=1
				break
			fi
		done
		if [ -z "$toolkit_ok" ]; then
			tail -20 "$work/apt-update.log" "$work/apt-install.log" >&2
			exit 1
		fi
	fi
	nvidia-ctk runtime configure --runtime=docker
fi

install -m 0755 /tmp/gpuworks-nvidia-kmod.select /usr/local/sbin/gpuworks-nvidia-kmod
install -m 0644 /tmp/gpuworks-nvidia-kmod.service /etc/systemd/system/gpuworks-nvidia-kmod.service
rm -f /tmp/gpuworks-nvidia-kmod.select /tmp/gpuworks-nvidia-kmod.service
systemctl daemon-reload
systemctl enable gpuworks-nvidia-kmod.service
# Modules an earlier driver installed stay in place; the selector's copies
# under updates/ take precedence in depmod's search order.
systemctl stop docker.service containerd.service nvidia-persistenced.service 2>/dev/null || true
/usr/local/sbin/gpuworks-nvidia-kmod
nvidia-smi -L
systemctl start containerd.service docker.service 2>/dev/null || true
