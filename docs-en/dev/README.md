# Developer Documentation

GPUWorks consists of two static Go binaries built with `CGO_ENABLED=0` and a React frontend. The control plane entry point is
`cmd/gpuworks`, and the node entry point is `cmd/gpuworks-agent`. The frontend lives in `ui/`, and its build output is embedded in the server.

## From Clone to Demo

```bash
git clone <repository-url> gpuworks
cd gpuworks
make demo
```

`make demo` builds the UI, server, and agents for each platform, then starts a local server and a real agent to verify node connection,
disconnection, and reconnection. It also runs on machines without a GPU. When the script finishes, it cleans up the processes and temporary directories created for the run.
For dependencies, ports, and joint frontend/backend development, see [Local Setup](setup.md).

## Repository Map

| Directory | Responsibilities |
|---|---|
| `cmd/` | Server, agent, and catalog maintenance CLI |
| `internal/server/` | REST, controllers, scheduling, storage, gateway, and Web UI embedding |
| `internal/agent/` | Connections, detection, runtime, collection, and transport |
| `internal/proto/` | Server/agent wire contract, including message formats and fields |
| `pkg/` | Reusable libraries for estimation, model parsing, benchmarking, and log parsing |
| `ui/` | React + Vite frontend |
| `test/` | Fake engines/models and assets for test tiers |
| `docs/adr/` | Accepted architectural decisions |
| `artifacts/` | Plans, research, and acceptance evidence (`work/`/`archive/`/`research/`; see `artifacts/README.md`) |

The root [`CLAUDE.md`](../../CLAUDE.md) is authoritative for development conventions, and
[`CONTEXT.md`](../../CONTEXT.md) is authoritative for terminology. For detailed package responsibilities, see the
[Code Map](code-map.md).

## Next Steps

- [Code Map: Package Responsibilities](code-map.md)
- [Local Development Environment](setup.md)
- [Test Tiers and Cloud Strategy](testing.md)
- [AMD support design: responsibilities, admission and compatibility](amd-support-design.md)
- [AMD Integration and Hardware Validation](amd-hardware-validation.md)
- [Benchmark Information and Reporting Standards](benchmark-reporting-standard.md)
- [Observability Limits of Per-Request Engine Queue Time](engine-queue-observation.md)
- [Community Registry Browsing](community-registry-browse.md)
- [Browser E2E Testing Standards](../../test/e2e/browser-e2e-testing-standard.md)
- [Browser E2E Component Checklists](../../test/e2e/browser-e2e-component-checklists.md)
- [Release Builds](release.md)
- [Contributing and Documentation Rules](contributing.md)
