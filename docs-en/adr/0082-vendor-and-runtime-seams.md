# ADR 0082: One Seam Each for Vendors and Runtimes: Vendor Definition, Probe Sequence, Typed Grant, Driver Registry

Date: 2026-11-24 · Status: Proposed

## Background

The platform supports a matrix of vendors by runtimes. Both axes used to be identified by bare strings, and what passed between them was a piece of Docker command-line text:

- **Vendor identity.** `internal/accelerator` kept one table per question, each branching by vendor (names, frameworks, PCI identity, checks, feature constraints, host requirements, and so on). A new vendor that missed one table still compiled.
- **Probing.** Every vendor's `Probe` wrote the same sequence again: whether the tool is there, what to report when it cannot be asked, and when to update "the cards seen last time".
- **Injection.** A vendor returned Docker option text such as `--device`, `--runtime` and `--gpus`. docker appended it as it was, nerdctl expanded device directories, kube translated it into Pod fields, and udocker parsed it again and guessed the vendor from environment variable names. When a vendor had to replace settings the workload carried, the engine edited the workload's option strings.
- **Runtime identity.** The list of drivers was written three times in the selection logic; the vendors' injection code branched on driver names, and a name that matched no branch fell through to whichever came last.

As a result, adding a vendor meant editing the insides of runtime drivers and logic shared by every vendor, and adding a runtime meant reading every vendor's injection code.

## Decision

1. **Everything about a vendor is in its own definition.** One `vendorSpec` value in `internal/accelerator/<vendor>.go`: its identity (name, aliases, frameworks), how its cards are recognized on a host (tool, control devices, PCI identity and model names, architecture derivation), what it can run workloads on (runtimes, host requirements, features, dGPU backend, runtime plugins), and how its cards are checked. The functions of the `accelerator` package that answer for a vendor read this definition; their signatures do not change.
2. **Not declared means not supported.** A feature not listed in `supports`, or a runtime not listed in `runtimes`, is refused by the server before scheduling and by the agent at admission. `runtimes` is the only statement of whether a vendor's cards can carry workloads: a vendor that lists none is found and monitored only.
3. **The probe sequence belongs to the framework.** `gpu.probeCards` holds what the vendors read through a tool have in common; a vendor answers only whether the tool is installed and how its output becomes cards. The identity baseline is one type, `cardBaseline`, and only an answer replaces it. AMD (read from sysfs, and still returning the cards it read when there is an error) and Apple (not on a PCI bus) do not go through it.
4. **A grant is typed and carries no container command line.** A vendor returns a `runtime.DeviceGrant`: device nodes, a runtime class, a device request to the NVIDIA Container Toolkit, supplementary groups, and which settings among the workload's own it takes the place of (`Replaces`). Each runtime driver renders it. The workload's own options are not rewritten; `WorkloadSpec.OwnDockerArgs` leaves out what was replaced when they are read.
5. **Runtime drivers have a registry.** `runtime.Drivers()` lists, in one place, each driver's name, where it cannot run, whether it holds images of its own, and which devices its containers see (`DeviceView`: only the granted ones, the host's, or no container). A vendor receives a `runtime.Target` (the name and the view) and decides how to grant from the view. A runtime whose view is unknown is refused once, in `gpu.ResolveInjection`; so is a non-empty grant for a runtime without a container.
6. **Registration is an explicit list at compile time.** A vendor's definition is one line in `vendorSpecs`, and its driver is one line in `defaultProbers`. There is no `init()` and there are no vendor plugins installed at run time.
7. **Tests and budgets hold it in place.** `TestVendorDefinitionsAreConsistent` checks the rules any definition has to satisfy and names no vendor; `TestVendorRuntimesAreTheAgentsDrivers` makes sure the runtimes a vendor can list and the agent's drivers are the same set of names. `scripts/arch-constraints.txt` has four budgets that only go down: comparisons with a driver's name on the injection path, hand-written container option literals, the number of files outside the definitions and drivers that decide by a vendor's name, and the number of functions in udocker that infer a vendor from environment variable names.

## Trade-offs

- **Two registrations, not one.** The definition is in `accelerator`, which the server and the agent share; the driver is in `agent/gpu`, which depends on `accelerator`. A single line would need `accelerator` to depend on `agent/gpu` in return, and the agent's code would enter the server's binary.
- **Vendor plugins installed at run time (not adopted).** A vendor's driver runs in the agent on every node, and probing and granting devices are on the start-up path; plugins would need a host, signed distribution and version negotiation built for the agent. Once the seams are in place, the same interface can be moved out of process if vendors are ever to ship their own binaries.
- **Self-registration through `init()` (not adopted).** It saves one line, at the price that "which vendors the platform supports" can no longer be read in one file and the order of registration depends on file names.
- **`runtimes` is strict for every vendor.** A test double's driver name is no exception, so the test environment's fake driver appears under the name of the runtime it stands in for.
- **A node that has not reported its runtime.** Only a vendor that requires a particular host (operating system or architecture) refuses it for that; the others are judged when the node reports. This is the existing behaviour, kept.
- **A workload without a vendor name is still judged the old way.** udocker records workloads on disk, and a record written by an earlier agent has no typed grant, so its inference of the vendor from environment variable names and its parsing of `--device` remain as the fallback. A new vendor adds nothing there.
- **Downgrade.** A udocker record written by the new agent keeps granted devices in typed fields. The earlier agent can read the record but does not know the grant in it: after a downgrade, a terminal or a command on a udocker container that is already running opens, but without the granted devices bound; with no device options in the record to infer from, it prepares the workload by its environment variable names (a PPU workload is therefore prepared as an NVIDIA one). The container itself is unaffected, and a redeploy restores it.

## Still Outside the Definition

- `InferVendor`'s rules for finding product-line markers in model and architecture names (`accelerator/infer.go`), and the mapping from Kubernetes extended resource names to vendors (`accelerator/kube.go`). Both are rules tried in a fixed order inside one function, and the order is itself behaviour.
- The checks on a driver's name in the engine: the dGPU injectors (`agent/engine/partition_hygon.go`, `partition_ascend.go`), kube's host root in `engine.go` and docker's mount root in `runtime_mounts.go`; and the hand-written `--tmpfs` of Hygon's dGPU backend. The budgets record how many there are.
- Icons and translated names in the console are still written in the front end; the vendors' order, names and frameworks come from the server's vendor dictionary.
- The declared support in the support matrix (`docs/reference/support-matrix.md`) is generated from the definitions; how far each vendor and runtime has been verified is still written by hand.

## Consequences

- Adding a vendor: one definition, one driver, one line of registration for each, and fixtures from real hardware. No runtime driver changes. The steps and the review standard are in [Adding a New Accelerator Vendor](../dev/add-accelerator-vendor.md).
- Adding a runtime: one driver, one entry in the registry that states which devices its containers see, and its name constant in `accelerator`. No vendor's driver changes. `runtimes` is strict, though: until a vendor lists the runtime in its own definition its cards are refused there, and listing it takes evidence from real hardware (row 8 of the review standard). A new runtime is therefore opened vendor by vendor.
- Whether a combination is supported is answered before scheduling.

<!-- translation-source-sha256: de0cc01cf014da31775c1ece6a1c435aeb38e1da4252ae49e8bdaec189e442ee # ADR 0082: One Seam Each for Vendors and Runtimes: Vendor Definition, Probe Sequence, Typed Grant, Driver Registry -->
<!-- translation-source-sha256: b93bc066d7aacb5b8291991a0143d8f1f177b4a1530498b1f6a431641c0dfb7b ## Background -->
<!-- translation-source-sha256: 4a3a9c91c32e5f7a2de4f69f6d4a515b9b8415be26ac52c3fac2597325ace58e ## Decision -->
<!-- translation-source-sha256: 53384c0d5cc0207565c20b024e70ac12cd0d835bd60075685653bdc0fe4e4d71 ## Trade-offs -->
<!-- translation-source-sha256: 46dbaeb180e094f5846d8e0e3add225178c2e4449ddd1f94cfdd8445e044e1e3 ## Still Outside the Definition -->
<!-- translation-source-sha256: 06828ffd0477d4437f4247b4fa57ca6ddcf08a981289279d2434a06e52c8a755 ## Consequences -->
