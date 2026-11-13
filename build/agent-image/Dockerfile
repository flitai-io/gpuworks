# The agent image a Kubernetes cluster's DaemonSet runs (internal/server/
# kubemanifest): the ordinary static agent plus nerdctl, which the kube
# runtime installs into the node and runs there, in the host's mount
# namespace (internal/agent/runtime/kube/host.go). NFS storages are mounted
# the same way, with the node's own mount.nfs. The base is glibc-based
# because the NVIDIA runtime mounts the host's nvidia-smi, a glibc program,
# into the agent's container, and Ascend's npu-smi is mounted from the host;
# it also carries the /bin/sh and nsenter the nerdctl wrapper needs.
#
# The image is the agent's runtime: its tag is the revision in REVISION, and
# changes only when what is installed here does. The agent binary it carries
# seeds the node's state directory, which the agent then runs from and is
# upgraded in (internal/agent/kubeboot).
#
# scripts/agent_image.sh prepares the build context: <arch>/gpuworks-agent
# and <arch>/nerdctl for every platform built. The final stage only copies,
# so one build produces amd64 and arm64 without emulation; the CA bundle
# comes from a stage on the build platform.
ARG BASE_IMAGE=debian:bookworm-slim
FROM --platform=$BUILDPLATFORM ${BASE_IMAGE} AS certs
ARG DEBIAN_MIRROR=deb.debian.org
RUN sed -i "s#deb.debian.org#${DEBIAN_MIRROR}#g" /etc/apt/sources.list.d/debian.sources \
    && apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates

FROM ${BASE_IMAGE}
ARG TARGETARCH
ARG AGENT_IMAGE_REVISION
ENV GPUWORKS_AGENT_IMAGE_REVISION=${AGENT_IMAGE_REVISION}
COPY --from=certs /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY ${TARGETARCH}/nerdctl ${TARGETARCH}/gpuworks-agent /usr/bin/
ENTRYPOINT ["/usr/bin/gpuworks-agent"]
