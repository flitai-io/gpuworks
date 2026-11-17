# Build context: this directory with the pinned upstream/ checkout.
# Preparation and bounded build instructions are in README.md.
ARG CUDA_BUILD_IMAGE=nvidia/cuda:13.0.0-devel-ubuntu24.04
ARG CUDA_RUNTIME_IMAGE=nvidia/cuda:13.0.0-runtime-ubuntu24.04
FROM ${CUDA_BUILD_IMAGE} AS build
ENV DEBIAN_FRONTEND=noninteractive PATH=/venv/bin:${PATH}
RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential ca-certificates git python3 python3-venv libgomp1 libatomic1 \
    && rm -rf /var/lib/apt/lists/*
COPY upstream/ /src/
WORKDIR /src
ARG BUILD_JOBS=4
ARG CUDA_ARCHITECTURES="75-real;86-real;89-real;89-virtual"
RUN test "$(git rev-parse HEAD)" = d2f096475817d4666c364c820b0ca830f751cfc5 \
    && test -z "$(git status --porcelain --untracked-files=no)" \
    && test "$(cut -d ' ' -f1 third_party/ggml/VERSION.txt)" = 3cf03257f219afbe7334045ff7c6a06ac68c627d \
    && python3 tools/check_vendored.py \
    && python3 -m venv /venv \
    && /venv/bin/pip install --no-cache-dir -r requirements-build.txt
RUN /venv/bin/cmake -S . -B /build -G Ninja -DCMAKE_BUILD_TYPE=Release \
    -DSTRATA_ENABLE_CUDA=ON -DSTRATA_BUILD_DSV41=ON -DSTRATA_BUILD_TESTS=OFF \
    -DSTRATA_NATIVE_EXPERTS=ON -DSTRATA_PORTABLE=ON \
    "-DCMAKE_CUDA_ARCHITECTURES=${CUDA_ARCHITECTURES}" \
    && /venv/bin/cmake --build /build --target strata-dsv41 --parallel "${BUILD_JOBS}" \
    && /build/strata-dsv41 --version
# Keep the runtime independent of the build tree, including ggml shared libraries if present.
RUN mkdir -p /engine \
    && cp /build/strata-dsv41 /engine/ \
    && test -s /build/libstrata_kq.so \
    && python3 packaging/collect_libs.py --binary /build/strata-dsv41 --also 'libstrata_kq.so*' \
       --search /usr/local/cuda/lib64 --search /build --out /engine \
    && rm -rf /src/.git

FROM ${CUDA_RUNTIME_IMAGE}
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
    python3 python3-venv ca-certificates libgomp1 libatomic1 libnuma1 \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /src /opt/strata
COPY --from=build /engine /opt/strata/engine-dsv41
RUN python3 -m venv /opt/strata/.venv \
    && /opt/strata/.venv/bin/pip install --no-cache-dir -r /opt/strata/requirements.txt \
    && /opt/strata/.venv/bin/pip check
COPY entrypoint.py model-manifest.json source-manifest.json /opt/gpuworks/strata-deepseek/
COPY image.json /opt/gpuworks/image.json
COPY metadata/DeepSeek-V4.1-Flash-1017-UD-Q8_K_XL-00001-of-00009.gguf /opt/gpuworks/strata-deepseek/metadata/DeepSeek-V4.1-Flash-1017-UD-Q8_K_XL-00001-of-00009.gguf
RUN test "$(stat --format=%s /opt/gpuworks/strata-deepseek/metadata/DeepSeek-V4.1-Flash-1017-UD-Q8_K_XL-00001-of-00009.gguf)" = 5780589 \
    && printf '%s  %s\n' cc15d470afcb030558bbc4d8e2ccd840182e2e95de1a95198138efe63a5fd99f /opt/gpuworks/strata-deepseek/metadata/DeepSeek-V4.1-Flash-1017-UD-Q8_K_XL-00001-of-00009.gguf | sha256sum --check --strict
RUN chmod 0755 /opt/gpuworks/strata-deepseek/entrypoint.py \
    && LD_LIBRARY_PATH=/opt/strata/engine-dsv41 /opt/strata/engine-dsv41/strata-dsv41 --version
ENV PYTHONUNBUFFERED=1 PYTHONNOUSERSITE=1 \
    LD_LIBRARY_PATH=/opt/strata/engine-dsv41:${LD_LIBRARY_PATH} \
    HF_HUB_OFFLINE=1 TRANSFORMERS_OFFLINE=1 STRATA_OFFLINE=1 \
    NVIDIA_DRIVER_CAPABILITIES=compute,utility
WORKDIR /opt/strata
EXPOSE 8080
LABEL org.opencontainers.image.title="GPUWorks Strata DeepSeek runner" \
    org.opencontainers.image.version="0.1.59-deepseek41-amd64-r4" \
    org.opencontainers.image.source="https://github.com/Niko1221/Strata" \
    org.opencontainers.image.revision="d2f096475817d4666c364c820b0ca830f751cfc5" \
    ai.gpuworks.strata.release="0.2.13" \
    ai.gpuworks.strata.revision="d2f096475817d4666c364c820b0ca830f751cfc5" \
    ai.gpuworks.engine="callable" ai.gpuworks.callable.family="strata-deepseek41"
STOPSIGNAL SIGTERM
ENTRYPOINT ["/opt/gpuworks/strata-deepseek/entrypoint.py"]
