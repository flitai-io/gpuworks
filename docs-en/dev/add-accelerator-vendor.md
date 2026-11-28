# Adding a New Accelerator Vendor

This page is the procedure to follow, and it ends with the standard to check such a merge request against. Why the work is divided this way is in [ADR 0082](../adr/0082-vendor-and-runtime-seams.md).

To add a vendor you write two things: a **vendor definition** (who it is and what it can do) and a **driver** (how to read the cards from the vendor's tool, and which devices to hand a workload). The probe sequence, the PCI roster and how devices reach each container runtime are handled by existing code.

What you do not need to change: any runtime driver (`internal/agent/runtime/*`), the scheduler, or another vendor's files. If you find you have to, stop: that is a gap in the seam, and it should be raised on its own.

## Files to Touch

| What | Where |
|---|---|
| Vendor definition | New `internal/accelerator/<vendor>.go`; the vendor constant in `vendor.go`; one line in `vendorSpecs` in `vendor_spec.go` |
| Driver | New `internal/agent/gpu/<vendor>*.go`; one line in `defaultProbers` in `gpu.go` |
| Fixtures | `internal/agent/gpu/testdata/`, raw tool output captured on real hardware |
| Card specs (optional) | `internal/accelerator/<vendor>_card_specs.go`, see "Card Specs" in Step 1 |
| Metrics (with a new metric) | Register the metric name in `internal/platformmetrics` |
| Kubernetes (when `runtimes` lists `kube`) | Add the extended resource name to `ClassifyKubeResource` in `internal/accelerator/kube.go` |

Registration is two lines and cannot be one: the definition is in a package the server and the agent share, and the driver is in the agent only.

The console takes the vendors' order, names and frameworks from the vendor dictionary the server sends (`GET /api/v1/accelerator-vendors`), and the "Support Declared in the Definitions" table of the support matrix is generated from the definitions; neither is edited by hand. After changing a definition, regenerate the table and the console's test fixture (see Step 3). What is still added by hand is the icon (`ui/src/components/vendorAssets.ts` and `ui/public/vendor-logos/`; without one the console draws a round mark with the first letter, whose colour and letter can be set in `VendorLogo.tsx`) and the translated name under `vendor.<id>` in `common.json`. The vendor names other pages keep for themselves need not be added: without one the template pages (`inferenceTemplates.json`) use the name the server gives, and the deploy pages (`deploy.json`) show the id in capitals.

## Step 1: The Vendor Definition

Write one `vendorSpec` value in `internal/accelerator/<vendor>.go`. What each field means is written on `vendorSpec` in `vendor_spec.go`. The fields come in four groups; write them one per line, as an existing vendor's file does (`hygon.go`, for example).

```go
// exampleVendor is Example. PCI identity measured 2026-12-01 on host-a:
// EX100 1234:0001 class 0x0302 bound to exdrv.
var exampleVendor = vendorSpec{
	id:         VendorExample,
	brand:      "Example",
	frameworks: []string{"exf"},
	named:      contains("example", "exf"),
	precedence: 45,

	tool:           "ex-smi",
	detectCommand:  "ex-smi",
	controlDevices: []string{"/dev/exctl"},
	pciID:          0x1234,
	pci:            &pciVendor{classes: []uint32{pciClass3D}, drivers: []string{"exdrv"}},

	runtimes:        []string{RuntimeDocker, RuntimeNerdctl},
	supports:        []Feature{FeatureMultiNode},
	ownCatalogImage: true,

	cardChecks: true,
}
```

There is one rule: **not declared means not supported.**

- `runtimes` lists the runtime drivers its cards can be given to a workload on. A node whose runtime is not in the list is refused before scheduling. A vendor that lists none has its cards found and monitored only and carries no workload; a vendor that gets monitoring first and deployment later starts this way.
- `supports` lists the platform features offered on its cards. What is not listed is refused by the server before scheduling and by the agent at admission.
- `partition` (the dGPU backend), `gpuPlugins`, `healthProbe` and the like are left out when the vendor has none.
- Both lists hold only what was verified on real hardware.
- `precedence` decides which vendor a string naming two of them belongs to; the lower is tried first. Do not use a short word in `named` that another vendor's name contains.
- Fill in `detectCommand` only when the tool's name is this vendor's alone (Kunlunxin's `xpu-smi` shares its name with Intel's, so it has none).
- `pci` follows `lspci -nn` on real hardware, with the machine and the date in the comment above the definition. Do not add the whole `0x03` display class for convenience: a server's BMC display chip is in that class too.
- When upstream `pci.ids` has no name for the card, give the model name and where it was read in `pciModels`.

### Card Specs (Optional)

The peak compute, memory bandwidth and interconnect bandwidth shown on the overview and the card detail come from `cardSpecs` in the definition (type `CardSpec`, see `card_spec.go`). A vendor can be added without them; its cards then read as not catalogued. Put the entries in `internal/accelerator/<vendor>_card_specs.go`, written like `nvidia_card_specs.go`.

Each entry must meet these rules:

- `Name` is the model name the platform stores: the name the vendor tool prints without the brand word (`NormalizeGPUName`), matched exactly. Each variant (SXM and PCIe, different memory sizes) gets its own entry; no model stands in for a similar one.
- `NameSeen` says which tool printed that name, on what hardware and on what day. A name copied from a datasheet does not count; without real-hardware output the entry is not added.
- Every figure has the vendor's own document as its source (`Sources`, with link, document version and retrieval date), and `Covers` says which kinds of figure it vouches for: compute, memory bandwidth, interconnect bandwidth, and whether interconnect bandwidth counts one direction or both.
- Rates are copied as the document prints them (`Stated`). When the document gives a figure with sparsity, mark it `Sparse` and the code converts it to the dense rate. Every precision in `Precisions` goes either into `Peaks` or into `Unsupported`, never neither: with one missing, nobody can tell whether the card lacks it or nobody looked.

`go test -run TestCardSpecsAreTraceable ./internal/accelerator/` checks all of the above.

## Step 2: The Driver

A driver implements `gpu.Prober`. `Probe` does not write its own sequence; it hands over to `probeCards`. You answer two things: whether the tool is installed, and how its output becomes cards.

```go
func (p *ExampleProber) Probe(ctx context.Context) ([]proto.GPUInfo, error) {
	_, err := lookCLI("ex-smi")
	return probeCards(ctx, accelerator.VendorExample, p.hostRoot, err == nil, p.list, &p.seen)
}
```

Rules for `list`:

- When the tool cannot be asked, return an error and no cards. The node then reports the PCI roster, which `probeCards` handles.
- Fill in each card completely: index, UUID, model, PCI address, total memory (bytes), vendor name; use `SetCardFinding` when there is a health finding.
- Refuse the whole listing when the number of rows does not match or an index or address repeats; a truncated output is not a complete listing.
- Run the tool with `queryLogged` or `RunVendorCLI`, which take care of timeouts, isolating a hung process, and running in the host's namespaces. Do not call `exec.Command` directly; `cliCommand` only enters the host's namespaces, with no timeout and no isolation of a hung process, so do not call it directly either. A few places in the existing drivers still execute that way (NVIDIA's and Ascend's lookups of the runtime plugin's version, Ascend's `docker info`, PPU's search for a shared library with `ldconfig`); do not copy them.

`p.seen` is a `cardBaseline`: which card each index was the last time the tool answered. Granting and metrics run after probing, and an index may by then point at another card; compare identities against it and refuse when they differ.

Metric names are the ones registered in `internal/platformmetrics`.

Optional interfaces a driver implements as needed. The framework uses one that is implemented, and a driver without it does not have that capability; the last two are complete only with the matching declaration in the definition. These are the ones most often needed when adding a vendor; `internal/agent/gpu` has others that are used by type assertion (topology, NVLink, accelerator ports).

| Interface | What it does | Also needed in the definition |
|---|---|---|
| `DriverAwareInjector` | The way to grant differs by runtime | Nothing |
| `ComputeProcessLister` | Which processes are on a card, to tell occupancy from outside the platform | Nothing |
| `HostRuntimeChecker` | Has the vendor's runtime actually use one card on the host (the per-card runtime check) | `CheckRuntime` among `checks`. Without it the check still runs and a failure still marks the card unusable, but the result is not reported and the console offers no recheck |
| `ContainerRuntimeGrader` | How the output is graded when the runtime check runs inside an image | `smokeFramework` and `smokeScript`, without which the check does not run in an image; and `CheckRuntime` among `checks`, as in the row above |

### Granting Devices

`RuntimeInjection` says which devices this workload gets, and writes no container command-line option:

```go
return RuntimeInjection{
	Env: map[string]string{"EXF_VISIBLE_DEVICES": visible},
	Grant: runtime.DeviceGrant{
		Devices: []runtime.Device{{Path: "/dev/exctl", Access: runtime.DeviceAccessAll}, {Path: node, Access: runtime.DeviceAccessAll}},
		Groups:  groupIDs,
	},
}
```

- docker, nerdctl, kube and udocker each turn `Grant` into their own form.
- To replace settings the workload carries (another vendor's `--gpus` or `--runtime`, certain device mappings), say so in `Grant.Replaces`; name environment variables to clear in `ClearEnv`. Do not edit the workload's option strings.
- When the way to grant differs by runtime, implement `DriverAwareInjector` and read `target.View` (the container sees only granted devices, sees the host's devices, or there is no container); do not compare driver names.
- Find device nodes from sysfs by PCI address and verify the device numbers; do not assume that index N is `/dev/xxxN`. When verification fails, return `UnsupportedReason` rather than a grant that may point at the wrong card.

## Step 3: Fixtures and Tests

- A fixture is raw tool output captured on real hardware, placed in `testdata/` as it is, with the tool's name in the file name.
- Parsers are asserted exactly against fixtures: the number of cards, and each card's address and memory.
- Cover the failure paths: a tool timeout, a failed command with partial output, empty output, garbled or truncated rows, and a grant after indexes have drifted. Each case must fail once the handling it covers is removed.
- At least one case runs the whole `Probe` through `probeCards`, with `hostRoot` pointed at `t.TempDir()`; one of them puts another card at the same index after `Probe` and asserts that the grant is refused (`probe_baseline_test.go` shows how for PPU and Hygon).
- Do not write tests that copy the definition out again. The definition is the statement; tests prove behaviour.

After a vendor is added, these existing tests ask you to update them, on purpose:

| Test | What to do |
|---|---|
| `TestVendorDefinitionsAreConsistent` in `accelerator` | Fix the definition as the error says. The test names no vendor, and there is no list of exceptions to add to |
| `TestKnownVendorsIsTheClosedSetNormalizeVendorProduces` in `accelerator` | Add to the expected list |
| `TestVendorRuntimeAdmission` in `accelerator` | Add a row: the runtimes this vendor is accepted on. It decides what is refused before scheduling, so it is written out |
| `TestDefaultProbersPreserveExistingVendorOrder` and `TestMixedVendorAutoKeepsExistingProberPriority` in `agent/gpu` | Add to the expected probe order. NVIDIA comes after every vendor that ships a compatible `nvidia-smi` |
| `TestEngineProfileGoldens` in `server/deploy` | `GPUWORKS_UPDATE_PROFILE_GOLDEN=1 go test -run TestEngineProfileGoldens ./internal/server/deploy/` |
| `TestSupportMatrixTableIsWrittenFromTheDefinitions` in `accelerator` | `GPUWORKS_UPDATE_SUPPORT_MATRIX=1 go test -run TestSupportMatrixTableIsWrittenFromTheDefinitions ./internal/accelerator/`, then `python3 docs-site/check_translations.py --write docs/reference/support-matrix.md` to record that page's new source hash |
| `TestAcceleratorVendorsWireJSONIsTheConsoleFixture` in `server/api` | `GPUWORKS_UPDATE_VENDOR_FIXTURE=1 go test -run TestAcceleratorVendorsWireJSONIsTheConsoleFixture ./internal/server/api/` |

## Step 4: Before Submitting

```bash
make test
make test-l2
make test-l3
go test -race ./internal/accelerator/... ./internal/agent/gpu/...
```

## Verification on Real Hardware

Offline tests prove parsing and sequence only. The following can only be answered on real hardware; until they are, do not write them into `runtimes`, `supports` or the support matrix:

- Which numbering selects cards inside a container: select a card other than 0, and several that are not adjacent, and check that the container sees those cards in that order.
- Cards that were not granted really cannot be used in the container.
- One real engine start and one chat on each runtime listed.
- How probing and granting behave when a card is passed through or drops out and the indexes are renumbered.

## Review Standard

Check a merge request that adds a vendor or a runtime against each row. Every row has a check that can be carried out; a row whose result cannot be stated counts as not met.

| # | Requirement | How to check |
|---|---|---|
| 1 | Only its own files are touched | The production files in `git diff --stat <base>` are only: the new vendor's definition and driver files, one vendor constant in `vendor.go`, one line in `vendor_spec.go`, one line in `gpu.go`, the icon and translated name in the front end, and the generated support-matrix table and console fixture; with a new metric, its registration in `internal/platformmetrics` as well, and when it lists `kube`, its extended resource name in `kube.go`. Tests, goldens and the other documents do not count. If a runtime driver, the scheduler or another vendor's file appears, ask for a separate merge request |
| 2 | The definition is consistent | `go test -run TestVendorDefinitionsAreConsistent ./internal/accelerator/` passes, and that test file is unchanged |
| 3 | No budget grew | `make check-architecture` passes, and `git diff <base> -- scripts/arch-constraints.txt` raises no number |
| 4 | The driver writes no container command-line option | Command 4 below the table prints nothing |
| 5 | The driver does not branch on a runtime's name | Command 5 below the table prints nothing. It matches `==`, `!=`, `case`, `EqualFold` and `Contains` against a driver's name, as a literal or one of the three sets of constants |
| 6 | `Probe` goes through `probeCards` | Command 6 below the table has one hit; the driver does not call `Roster` and has no type of its own for the cards seen last time. A driver that checks a card's identity when it grants (any that finds device nodes by index has to; among the existing drivers Ascend builds `/dev/davinciN` from the chip number without checking, a known gap not to copy) passes its own baseline, not `nil`, and has a case that runs `Probe`, then puts another card at the same index, and the grant is refused |
| 7 | Metric names are registered | `go test -run TestEveryMetricLiteralIsRegistered ./internal/platformmetrics/` passes |
| 8 | Every item in `runtimes` and `supports` has evidence from real hardware | The verification section of the merge request gives, for each item, the machine, the driver version, the image digest and the actual command. An item without evidence is removed from the definition |
| 9 | Fixtures are raw output | The commit message or a note beside the fixture gives the machine and the date; the comments on the PCI identity and `pciModels` say where they were read |
| 10 | The failure-path tests really test | The reviewer picks any one failure-path case, removes the handling it covers from the driver, and confirms the case turns red |
| 11 | No test copies a table | No new test asserts the definition's field values listed out again. The row in `TestVendorRuntimeAdmission` is the exception: it states which runtimes are accepted and refused before scheduling |
| 12 | The documents state only what was verified | Each cell added to the support matrix has matching evidence in the verification section |
| 13 | Card specs are traceable | `go test -run TestCardSpecsAreTraceable ./internal/accelerator/` passes; the reviewer picks one entry, opens its source links to check the figures, and finds in the merge request the real-hardware output `NameSeen` refers to |

The commands for rows 4, 5 and 6 (a table cannot hold a vertical bar, so they are written out here; replace `<vendor>` with the file name prefix):

```sh
# 4: prints nothing
grep -nE '"--(device|gpus|runtime|group-add|tmpfs)"' internal/agent/gpu/<vendor>*.go | grep -v '_test\.go'
# 5: prints nothing. The pattern is the one of agent_runtime_name_comparisons in scripts/arch-constraints.md
N='"(docker|nerdctl|udocker|kube|host)"|(agent)?runtime\.Driver[A-Z]|proto\.RuntimeDriver|kube\.DriverName|hostproc\.DriverName|accelerator\.Runtime(Docker|Nerdctl|Udocker|Kube|Host)'
grep -nE "(==|!=) *($N)|($N) *(==|!=)|case .*($N)|(EqualFold|Contains)\(.*($N)" internal/agent/gpu/<vendor>*.go | grep -v '_test\.go'
# 6: one hit; for a driver that checks identity when it grants, the last argument is its own baseline (&p.seen), not nil
grep -n 'probeCards(' internal/agent/gpu/<vendor>*.go | grep -v '_test\.go'
```

## What Not to Do

- Do not add a vendor branch to a runtime driver, and do not add a function to udocker that infers the vendor from environment variable names.
- Do not change logic every vendor shares (the scheduling of checks, how occupancy is judged, how the roster is merged) in the merge request that adds a vendor. If it needs changing, raise it separately and compare existing vendors against a baseline.
- Do not take "the device node exists" or "the environment variable is set" for "inference runs".

<!-- translation-source-sha256: 70761906f3f9d64cc518f65d6f2a068e4d3bc5f680e70d2d14dff422aa3f3bd2 # Adding a New Accelerator Vendor -->
<!-- translation-source-sha256: 0be45eaa9e5040ee6ba442767d3febf2e6818f5309910f0ffeece6f340946e2a ## Files to Touch -->
<!-- translation-source-sha256: 72f35532382ae1f8b24c0827fa855a5c43bf367873e2e75b1910e2509f73face ## Step 1: The Vendor Definition -->
<!-- translation-source-sha256: 1e77368a2805d8b364b25ba612c2a89bb4b092d0912b0b3e347e4609425da95d ### Card Specs (Optional) -->
<!-- translation-source-sha256: f87115249ebd97728701ece1fbf0b11cebb5e1ca1db5ed142eea0cab744d0030 ## Step 2: The Driver -->
<!-- translation-source-sha256: 8fa3cafc245b6882ec70b3edaff7eee36df16d04a1e496ebe3a2502d1294761d ### Granting Devices -->
<!-- translation-source-sha256: 78d3739634eae66535d388435344994d7005d2d16ad271250e6eb6e4d251ae22 ## Step 3: Fixtures and Tests -->
<!-- translation-source-sha256: e5f1e773baca5a8cd4cf9893e9316affc36ea6e6872236476e3c1b7d7e9324ce ## Step 4: Before Submitting -->
<!-- translation-source-sha256: ee9f7e0f0fc8ea4baad3104d491662e298b0ac9a7ed15220e70e8295c9de2f77 ## Verification on Real Hardware -->
<!-- translation-source-sha256: b2b0c303b43d7f10cdd708ff1a4ac203f97966e4c2e742e3af6335e258f06f96 ## Review Standard -->
<!-- translation-source-sha256: 269ac2f3dc089de79070bc268adb2c3ab12a39310a1abc4f1971ce5855e8834b ## What Not to Do -->
<!-- translation-localized-code-block: 9ea29b074ac27a071bd49a8841fc9ac5eadd74e70c0d486438071610e8081ac5 ae3c0ad7266f2638793520aca2ce887f5d5ef0911f479687ccc57079bc8f7471 -->
