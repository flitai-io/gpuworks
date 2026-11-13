# Local Development Environment

## Dependencies

- Use the Go version specified in `go.mod` (currently Go 1.26.11, consistent with the CI image `golang:1.26.11`).
- Node.js and npm; use the frontend dependency versions specified in `ui/package.json` and the lockfile.
- GNU Make. Builds or L3/L4 tests may also require Docker, SSH, `curl`, and the target vendor's CLI.

```bash
go version
node --version
npm --version
make demo
```

A local server started with `make run-server` listens on `:8800` by default and writes data to `./data`. Parallel development instances use 8800, 8802,
8804, and 8806 in order. Use `password` for manual testing; do not use this test password in production.
`make demo` uses a separate `127.0.0.1:8099` address and a temporary directory, and generates an administrator password for the current run.
When the script exits, it cleans up the processes and temporary directory it created.

## Browser E2E

E2E needs more than a build, in four layers: Playwright with system Chrome only,
then adding `bin/gpuworks`, then `bin/gpuworks-agent`, then a Docker daemon. Most
development work lands in the first two and needs no GPU. The full list, how many
scripts each layer covers, and how to diagnose a missing dependency are in
[test/e2e/RUNNING.md](../../test/e2e/RUNNING.md).

```bash
# Python dependencies (add --user outside a virtualenv)
python3 -m pip install -r requirements-e2e.txt
google-chrome --version          # the tests use channel="chrome" and never download chromium

make build                       # produces bin/gpuworks and bin/gpuworks-agent
make test-e2e-smoke              # shortest path: real Chrome, isolated server, SQLite
make test-e2e-docker-check       # whether the Docker-class cases can run here
```

`check-libvgpu` needs `objdump` (`apt install binutils`). **Do not export `GO`, `CGO_ENABLED`,
`GOTOOLCHAIN` or `GOMAXPROCS`**: they break the `$(GO)` expansion inside the `cross-agents`
loop, whose symptom is `/bin/sh: 4: build: not found` and has nothing to do with a real
compile error. To pick a toolchain, pass `make ... GO=<absolute path>`.

## Start the Backend and Frontend Separately

Open a terminal at the repository root, start the backend, and leave it running:

```bash
make run-server
```

Open another terminal, enter `ui/` from the repository root, and start the frontend development server with hot reloading:

```bash
cd ui
npm install
npm run dev
```

The regular `make ui` build first runs i18n checks, then writes the artifacts to `internal/server/webui/dist`. The frontend development
server is only for hot reloading. To validate the embedded artifacts, run `make ui` before starting the Go server.

## Local Secrets

Store addresses, SSH parameters, and cloud keys only in the gitignored `secrets/` directory. You can create
`secrets/test-environments.md` to record your local environments, but do not commit real values. For non-sensitive descriptions of environment variables, see the
[Configuration Reference](../reference/configuration.md).

## Deploy to a Long-Running Acceptance Environment

Record long-running acceptance servers and physical machines (addresses, SSH methods, and accounts) in the gitignored `secrets/test-environments.md`,
which stays out of the repository. To update these servers, use `gpuworks install` to reinstall into the same directory. It copies itself, rewrites the service, and checks health,
so you do not need to write a systemd unit manually.

`install` does not create backups (only `upgrade` does). Before replacing the binary, make a copy of the old binary
`/opt/gpuworks/server/gpuworks` on the acceptance machine. To roll back, rebuild from the target commit and install again.

Run the build command from the repository root:

```bash
make server-linux-amd64          # Includes the UI and embedded agents; do not substitute go build
```

Run the installation and health check commands on the Linux amd64 acceptance machine. If you build on another machine, copy the artifact to the acceptance machine first,
then replace `./bin/gpuworks-linux-amd64` below with the path to the copied artifact:

```bash
./bin/gpuworks-linux-amd64 install --dir /opt/gpuworks/server --addr :8800
curl --fail http://127.0.0.1:8800/healthz
```

If the service fails to start after deployment, check `systemctl status gpuworks` and `journalctl -u gpuworks` first.

## Common Checks

```bash
make test
make test-integration
make vet
git diff --check
```

For complete requirements on test layers, race detection, build tags, and physical hardware, see the [Testing Guide](testing.md).
