# Extension Points: The Module Contract and Installable Extensions

Inside the core, the gpuworks server composes modules through one set of interfaces (`app.Module` and `app.Host`). Tests and forks use this contract today: another Go module can use the server as a library and compile it, together with its own modules, into a new server binary (a "separately built server" below); official builds carry no module. Official and third-party extensions are installed as signed plugin packages and connected through the same contract by a built-in plugin host module ([ADR 0071](../adr/0071-installable-extensions.md)); what the plugin host does today and what is not built yet is in [Installable extensions](#可安装扩展).

This page describes the interfaces the open-source code leaves, the thread each one is called on, the rules they must follow, and what the open-source code does not do. The irreversible decisions are in [ADR 0070](../adr/0070-extension-tiers-and-edition-boundary.md) and ADR 0071; the terms (extension module, feature key, feature availability, extension-held cordon, plugin package, pushed table) are defined in `CONTEXT.md`.

Extension docs, examples and tests describe each mechanism in its own terms; sample publishers, editions and plugins use the fictional name `acme`, as sample domains use `example.com`.

## Tiers

| Tier | Runs where | Carries |
|---|---|---|
| T0 Core | server, agent | Every general capability and every enforcement point: all decisions taken per request, insert, status report, claim or placement live here; the community edition itself, free by definition |
| T1 Built-in module | inside the server process, in Go | Core-internal composition through `app.Module` / `app.Host`; `cli.Options.Extensions` stays for tests and forks and is not a distribution path for vendors or third parties |
| T2 Package server part | a child process of the server | Decisions that need I/O, state, workflows or integrations, pushed as tables the core enforces (installation, supervision and pushed state are built, see [Installable extensions](#可安装扩展); the rest is under [Not built yet](#尚未实现)) |
| T3 Package node part | core workloads and jobs on nodes | Products that use GPUs; no plugin code runs in the agent (not built yet) |
| T4 Policy part | inside the server | Reserved only, not built |

Every interface on this page belongs to T1. The design of T2–T4 is in ADR 0071.

## How a build composes

A separately built server (a fork or a test) has its own `main`, which calls `cli.Main` once:

```go
func main() {
	os.Exit(cli.Main(os.Args[1:], cli.Options{
		Edition:      "acme",
		Extensions:   app.Extensions{Modules: []app.Module{audit.New(), sso.New()}, UI: console.FS()},
		Distribution: distribution.Profile{Edition: "acme", Sources: sources, Keys: keys},
		InstallHooks: []selfinstall.InstallHook{devices.Hook{}},
		Commands:     []cli.Command{statusCommand},
	}))
}
```

The zero value of `cli.Options` is the community server that `cmd/gpuworks` runs:

- `Edition` is written to `version.Edition` and is for display only: `gpuworks edition`, `/healthz`, the build info, the usage report and the console manifest read it. Open-source code never branches on it (the `edition_branches` budget is 0).
- `Extensions.Modules` go to `serve`. `migrate`, `reset-password`, `support-bundle` and extra commands also open the database with the modules' migrations (extra commands use `cli.Env.OpenMaintenanceStore`).
- `Extensions.UI` replaces the embedded community console and has the same layout as `internal/server/webui/dist`; when it is nil the embedded build is served.
- `Distribution` is the build's own distribution channel: update checks, notices, catalog updates and upgrades read it, and an index signed for another edition is refused. Its edition must equal `Edition`, or the binary panics at startup.
- `InstallHooks` run in order on `install` and `upgrade` and in reverse on `uninstall`, for host steps that need root, such as device permissions. The core's own hook (the systemd drop-in of the [host identity](#管理-server-的主机身份)) comes before them, so it runs first on install and last on uninstall. The `Layout.UnitDir` a hook gets is non-empty only in systemd mode, and without it the host identity hook does nothing; a console upgrade runs no hook.

Official builds pass the zero value. The edition scoping of `Edition` and `Distribution` serves forks only, and the official release path no longer extends it (ADR 0071).

## Modules and the Host

```go
type Module interface{ Name() string }                     // [a-z][a-z0-9_]{1,31}
type Migrator interface{ Migrations() store.MigrationSet } // optional
type Initializer interface{ Init(ctx context.Context, h *app.Host) error }
type Service interface{ Run(ctx context.Context) error }
```

A module's name is its only identity: its `extensions.<name>` section in `server.yaml`, its migration namespace and the `module` label on its log records all use it. An invalid or repeated name, or a migration set whose namespace is not the module's name, stops the server before the database is opened.

`Init` runs once, in `app.New`, in module order, after the API dependencies are built and before the router. A module reaches the server only through the `Host`:

| Method | Purpose |
|---|---|
| `Log()` / `Bus()` / `DataDir()` / `InstallID()` | A logger with `module=<name>`, the event bus, the data directory, the installation ID |
| `Section(v)` | Strict decode of `extensions.<name>` in `server.yaml`; without the section `v` stays as it is |
| `Tables()` | The module's own tables (`store.ExtensionTables`); nil for a module without a migration set |
| `DeriveKey(purpose)` | A 32-byte key derived from the master key with HKDF-SHA256 and bound to the module name |
| `MountAPI(e)` | Mounts an `api.Extension`'s routes under `/api/v1/x/<name>/` |
| `DeclareFeatures(...)` / `ProvideFeatures(src)` | Declares feature keys; provides the feature source for the keys compiled modules declare (at most one across all modules; the keys plugin packages declare are answered by the plugin host, not by it) |
| `AddBannerSource(b)` | Banners at the top of every console page |
| `GuardNodeInsert(g)` | A guard on every new node's insert |
| `LimitPlatform(src)` | The source of the whole server's availability (see "Availability"); at most one across all modules |
| `AnswerMembers(fn)` / `AskMember(ctx, member, question)` | In a control-plane cluster, what this module answers the other members, and a question to this module on another member. It travels over the members' existing channel (mutual TLS with member certificates), so the answer comes from the member asked and nobody else can ask; question and answer are at most 64 KiB each. The member asked need not lead. On a single server `AskMember` returns `ErrNotClusterMember`; on a cluster member, until its route to the others is up, `ErrMemberRouteNotReady` (a module's `Init` runs before the route, so a question asked there always gets it: the question was not asked, which says nothing about the member); for a member that is down, or whose build registered no answerer, `ErrMemberUnreachable`. `AskMember` is called while serving |
| `HoldScheduling(h)` | A scheduling holder, keyed by its source; a repeated source is refused |
| `HoldCardScheduling(h)` | A card holder (`store.CardSchedulingHolder`), keyed by its source; a repeated source is refused. One module may hold nodes and cards under one source; see [Card holders](#按卡的保持者) |
| `ReapplySchedulingHolds()` | The holder's answers changed: asks for the holds to be applied to every node (and its cards) again, rather than at each node's next status report |
| `ReapplySchedulingHoldsFor(ids)` | The same, but only for the nodes (by node ID) whose answers changed, and their cards |
| `NodeFleet(ctx)` | Every node's hold facts (`store.NodeHoldFacts`), ordered by ID (enrollment order), in one query on the read pool |
| `CardFleet(ctx)` | Every card's hold facts (`store.CardHoldFacts`), ordered by node ID and then card identity, in one query on the read pool |
| `NodeIdentity(ctx, ids, salt)` | The salted hardware identity of the named nodes (`store.NodeIdentity`); raw serials never leave the core. See [Node identity and node inventory](#节点身份与节点清单) |
| `NodeInventory(ctx, cursor, limit)` | A page of the node inventory in enrollment order (`store.NodeInventoryPage`): names, enrollment times, each node's own cards (index, model, whether it has a serial) |
| `NodeNamesByID(ctx, ids)` | The display names of the named nodes, keyed by ID (`map[uint]string`), in one query on the read pool |
| `ClusterObservation(ctx, timeout)` | One observation of the control-plane cluster this server belongs to (`app.ClusterObservation`): the members, their roles, which of them answer, and whether the cluster can lose another member; it has the same source as the `cluster-status` report. A single server, or a build without the cluster runtime, answers `Clustered: false`. Read-only, and may be called after `Init`, while serving; each probe is bounded by `timeout` and they run one after the other |
| `HostIdentity(salt)` | The salted identity of the machine the management server runs on (`hostidentity.Salted`); see [The management server's host identity](#管理-server-的主机身份) |

The `Host` hands out neither the `*store.Store` nor a database handle. A registration after `Init` returns is refused (`ReapplySchedulingHolds`, `ReapplySchedulingHoldsFor`, `NodeFleet`, `CardFleet`, `NodeIdentity`, `NodeInventory` `NodeNamesByID` and `HostIdentity` are not registrations and may be called at any time): methods with an error result return `app.ErrHostClosed`, the others panic. If `Init` fails, `New` fails and closes the database, so the same data directory starts again at once.

Extension routes live under `/api/v1/x/<name>/`: `api.RouteSpec.Pattern` is relative to `/api/v1` and is `/x/<name>` or starts with `/x/<name>/`, and is a clean path (no `.`, `..`, `//` or trailing `/`). A route outside the module's own namespace, an unclean pattern, or two patterns of one method that differ only in parameter names (`{id}` and `{name}`) panic while the router is built. The core registers no route under `/api/v1/x/`, so a module name may equal a core path segment (such as `nodes`) and never collides with a path the core adds later.

A route's access level (`api.RouteSpec.Access`) is one of four: `AccessRead` (any signed-in user and any platform token; a read token only reads), `AccessAdmin` (administrators, by session or platform token), `AccessAdminSession` (administrators signed in with a session) and `AccessPublic` (no authentication). A public route is limited per client address by the same rule as a plugin package's `public` routes (60 a minute by default, bursts of 30; over that, 429 `TOO_MANY_REQUESTS` with `Retry-After`); its chain never reads or renews a session, so the handler never learns who calls, and it cannot be readable by the AI assistant. `RouteSpec.SignIn` marks a public route as part of signing in (an identity connector's start and callback, including the POST of a SAML response); only an `AccessPublic` route may set it, otherwise mounting panics. It is served while the whole server is limited ([ADR 0088](../adr/0088-extension-public-routes-under-a-platform-limit.md)).

After every `Init` has run, the server releases extension-held cordons and card holds that no module of this build owns (so they do not outlive a changed build or a removed module), applies the holders' answers to every node and its cards once if any holder (of nodes or cards) is registered, then builds the feature gate (`featuregate.New`) and hands it to the API.

## Reapplying holds and the node fleet

A holder is otherwise asked on every status report, so a node whose answer changed can wait up to a minute to be cordoned or released. A module whose answers changed (for example, a rebuilt snapshot holds a different set of nodes) calls `ReapplySchedulingHolds()`, and the server runs `ApplySchedulingHolds` over every node again:

- It never blocks and allocates nothing, and may be called from any goroutine, **except from inside a hook** (`SchedulingHolder.Hold`, `CardSchedulingHolder.HoldCard`, an insert guard): a pass asks every holder again, so a holder that asked for one from `Hold` would keep the whole fleet in passes. Hooks request no other recompute either.
- Requests are coalesced: any number made while a pass is pending or running make at most one more pass, and passes are at least 2 seconds apart, so a holder whose answers flap costs at most one fleet-wide writer transaction every 2 seconds, never one per event.
- A module that knows which nodes its answers changed for calls `ReapplySchedulingHoldsFor(ids)`: that pass reads and writes those nodes only (`ApplySchedulingHoldsFor`, `WHERE id IN`), so a holder that often changes a few nodes does not cost a fleet-wide transaction each time. It is coalesced with every other request; when the nodes named between two passes exceed `holdpass.MaxScoped` (512), or anyone asked for every node, the pass covers every node. Pushed state (the node holds table plugins push) takes this path and asks for every node only when a capacity cap changed.
- The pass runs in the same `services()` loop as the modules' `Service`, off the caller's goroutine, and stops with the server; a failed pass is logged as an error and retried with a backoff that starts at 2 seconds and doubles up to a minute. With no holder registered it does nothing.

A holder whose answers depend on the whole fleet (for example, choosing nodes in enrollment order) learns the fleet from these two, rather than by recording the nodes it sees in `Hold`:

- `NodeFleet(ctx)`: every node's hold facts, in one query on the read pool. It is for boot and for a recompute after the fleet changed, coalesced; never per report or per event, and never from inside a hook. Subscribe to the topic below before the first read, so no change between them is missed.
- The bus topic `store.NodeFleetTopic`: one event for each node insert (all three insert paths), each delete, and each status report that changed the node's hold facts (its card count or vendor; its ID, UUID, enrollment time and Kubernetes cluster never change) or a card's (a card added or removed, or its index, vendor or ownership changed), keyed by the node's ID. Read it with `store.NodeFleetChangeOf(ev)`, which gives `{NodeID, Kind}` (`NodeInserted`, `NodeDeleted`, `NodeFactsChanged`); do not parse the reason text of the `node` topic. A routine report that changed no hold fact publishes nothing, so publishers cost what they did. Like every bus topic, a subscriber that falls behind gets only each node's latest event, so it treats any event as "this node, and so the fleet, changed" and recomputes from `NodeFleet`, coalesced, rather than counting events. A node ID is never reused after its node is deleted.

## Card holders

`store.CardSchedulingHolder` (`Source()`, `HoldCard(store.CardHoldFacts) store.CardHold`) holds single cards. Its rules are those of `SchedulingHolder`, applied to a card:

- Facts, `CardHoldFacts`: the ID, UUID and enrollment time of the card's node, and the card's identity `Ident` (the key its row is stored under, `accelerator.CardIdent`; `index:<n>` when the probe reports no identity), its `Index` (-1 when the vendor CLI does not list it; renumbered when the card set changes, so shown, not keyed), its `Vendor` and its `Ownership` (empty for a card this node may use).
- Answer, `CardHold{Code, Fallback, Params}`: the zero value does not hold; a hold is shown to an operator as a product message, `CARD_SCHEDULING_HELD` when it names no code. With several holders, the first registered that holds a card owns it.
- A held card's `GPU.CanTakeWork` is false: the scheduler puts no new work on it (no whole card, no memory share, no share with processes outside the platform, not even when the cards are chosen by hand), with the refusal reason `gpus_held` (params `gpus`, `count`, `node`). The node itself stays schedulable, and instances already running on the card keep serving and are never drained.
- The effect is stored on the card's row: `gpus.scheduling_held_by` records the holder's source (empty when none holds it), and readers that count cards in SQL (`OnlineGPUGroups`) read that column; the hold itself is not stored. Every status report that writes the node's cards (a successful probe, or one that read only PCI) asks again for each card in the same writer transaction, and `ApplySchedulingHolds` / `ApplySchedulingHoldsFor` (the passes `ReapplySchedulingHolds` and `ReapplySchedulingHoldsFor` ask for) go over the cards first and then the nodes in one transaction; a node whose card holds changed is published as a scheduling change but is not counted in the returned node lists.
- Operators: a card hold outranks an administrator's enable and yields to their disable. While a card is held, `SetCardScheduling` to schedulable returns `store.ErrCardHeld` (wrapping the holder's `*messages.ProductError`; the API answers 409 with the holder's code), and a stop works as usual; the card's `scheduling_actions` never offers `schedulable`. Once the hold ends, the card follows the administrator's decision or the platform's verdict again.
- A card's view (`GET /nodes/{uuid}` and `GET /gpus`) carries `scheduling_held_by` and `scheduling_hold` (the message its holder gives now; the generic `CARD_SCHEDULING_HELD` while the row still records a hold its holder has let go of and the next pass has not run).
- A holder whose answers depend on every card reads every card's hold facts from `CardFleet(ctx)` and recomputes, coalesced, after `store.NodeFleetTopic` events (a card added or removed, or a change of its hold facts, publishes `NodeFactsChanged`), rather than recording the cards it sees in `HoldCard`.
- `HoldCard` is called once per card on every report, so the purity rules below apply: no allocation when it does not hold, and a prebuilt answer when it does. `store.Store.CardHoldFor(facts)` is exactly the ask the writer transaction makes and reads nothing; the contract tests use it to prove that nothing is allocated.

`Service.Run` is started by the same `services()` loop in `App.Run` as the health checks, card failure records, usage report and notice refresh, after the controller starts. It returns when `ctx` is done; an error it returns is logged and the service is not restarted. One App can be `Run` more than once (an in-place restart), so `Run` must tolerate being called again.

## Node identity and node inventory

A module that needs to recognize "the same machine, the same cards" (an inventory export, a support bundle) reads salted digests through the `Host`, never raw serials:

- **Host system UUID**: the agent reports `host_system_uuid` in the hello of every handshake: the firmware system UUID (`/sys/class/dmi/id/product_uuid` on Linux, `IOPlatformUUID` on macOS), normalized by `proto.NormalizeHostSystemUUID` (lower case; placeholders, all zeros, all F and malformed values dropped). On Linux only root can read that file, so a non-root agent reports none; older agents report none either and connect as before. The server normalizes it again on receipt and writes `nodes.host_system_uuid` only when it differs from the node's row, so a routine reconnect adds no write; an empty value never clears one already recorded. It describes the machine, not the node: several logical nodes on one machine report the same value.
- **Card serials**: a card's id is the `GPUInfo.UUID` its probe reported. `accelerator.CardSerial` tells a serial the hardware carries, which survives a move to another slot or host, from an id the probe made up from a slot or an index; only serials take part in identity:

  | Vendor | Reported id | Stable |
  |---|---|---|
  | NVIDIA, PPU, Kunlunxin | The card UUID from the vendor CLI | Yes |
  | AMD | `amd-<unique_id>` (the ASIC serial) | Yes; `amd-<PCI address>` when the driver has no `unique_id`, no |
  | Hygon | `GPU-<unique id>` (hy-smi or sysfs) | Yes; `hygon-<PCI address>` or `hygon-<index>` without one, no |
  | Ascend | `ascend-<index>` | No (npu-smi's table gives no serial) |
  | Apple | `APPLE-<IOPlatformUUID>` | Yes, and the same machine as the host system UUID |
  | Fake probers | `GPU-FAKE-<n>`, `FAKE-<vendor>-<n>` | No (test values, the same on every node) |

- `NodeIdentity(ctx, ids, salt)` answers `{NodeID, Host, Cards, CardsWithoutSerial}` for each named node that still exists, in ascending ID order:
  - Each item of `Host` and `Cards` is a `store.IdentityPart{H, Tail}`. `H` is the first 16 hex characters (8 bytes) of HMAC-SHA256 keyed with the caller's salt over the kind, a zero byte and the raw value, the kind being `host` or `card`; `Tail` is the raw value's last 4 characters, for a person to check against the machine, and is empty when the raw value is shorter than 8 characters, so a tail is never the whole value. One salt gives the same digests every time; different salts give unrelated `H` values. `Tail` is not salted: every caller sees the same raw tail.
  - `Cards` holds only the cards the node may use (`accelerator.CardUsable`) that carry a serial, ordered by `H`; usable cards whose id was made up are only counted in `CardsWithoutSerial`. So two logical nodes on one machine share `Host` and each has only its own cards.
  - Partial answers: `Host` is the zero value (`Known()` is false) until the agent reports a host UUID; a node that does not exist (never did, or was deleted) is left out of the result, so the caller compares IDs; a repeated ID counts once.
  - `ids` holds 1 to 500 nodes, else `store.ErrNodeReadBatch`; the salt is 1 to 256 bytes, else `store.ErrIdentitySalt`. A call out of bounds is refused, never cut short.
- `NodeInventory(ctx, cursor, limit)` returns one page `store.NodeInventoryPage{Nodes, Next}` in ID (enrollment) order: each node's ID, UUID, display name (the name an operator gave it, otherwise its hostname), enrollment time, `HostReported` (whether its host UUID is known) and the cards it may use as `{Index, Model, Serial}`, with no raw serial. Pass 0 as `cursor` for the first page and the previous page's `Next` after that; a `Next` of 0 means this was the last page. `limit` is 1 to 500, else `store.ErrNodeReadBatch`. Node IDs are never reused, so a node enrolled or deleted between pages neither shifts nor repeats the others.
- `NodeNamesByID(ctx, ids)` returns the display name (the name an operator gave the node, otherwise its hostname) of each named node that still exists, keyed by ID; a node that does not exist (never did, or was deleted) is absent, and a repeated ID counts once. Use it to label a few nodes instead of paging `NodeInventory`. No IDs returns an empty map without a query; more than 500 distinct IDs returns `store.ErrNodeReadBatch`, never cut short. It is one primary-key query on the read pool.
- Scale: `NodeIdentity` and `NodeInventory` are pinned reads, two indexed queries each on the read pool (nodes by primary-key lookup or range, cards through the `gpus` `(node_id, ident)` index), at most 500 nodes per call. Call `NodeIdentity` for the nodes a page shows, not for the whole fleet on every event. Identity changes (a new host UUID, a swapped card) are not published on `store.NodeFleetTopic`; a module that must follow them reads again when it reads the page.
- Plugins: an installed plugin reads the same two through the host API (`POST /host/v1/node-identity`, `GET /host/v1/nodes`; wire formats in the [plugin protocol](../reference/plugin-protocol.md#节点读取)), with the permissions `read:node_identity` / `read:node_inventory` and the capabilities `host.node_identity@1` / `host.nodes@1`. `pluginhost.IdentityKey` binds a plugin's salt to its package id (the HMAC key is `"pkg\0"+id+"\0"+salt`, with a salt of at most 219 bytes), so two packages with the same salt get different `H`, and one package gets the same `H` on different servers; the unsalted `Tail` is the same for every caller and still allows weak correlation across packages. `app.PluginReads` turns the store's results into wire objects, cutting names and models longer than 255 bytes at a character boundary.

### The management server's host identity

`HostIdentity(salt)` returns the salted identity of the server's own machine, `hostidentity.Salted{SystemUUID, BoardSerial, MachineID, RootFSUUID}`, each part an `hwident.IdentityPart` (the digest and tail of node identity; the zero part when unknown). The system UUID is digested as the same kind as a node's `Host` (`hwident.KindHost`), so under one salt it equals the `Host.H` of each node on the server's machine. The salt is 1 to 256 bytes, else `store.ErrIdentitySalt`. The facts are read once per process (`hostidentity.Local`).

- Sources (`internal/server/hostidentity`): on Linux, `/sys/class/dmi/id/product_uuid`, `board_serial` and `/etc/machine-id`, and the file system UUID of `/` found through `/proc/self/mountinfo` and `/dev/disk/by-uuid`; on macOS, `IOPlatformUUID` and the machine's serial number through `ioreg`. Empty values, firmware placeholders and all-0 or all-F values are unknown.
- The kernel lets only root read the system UUID and the board serial, and the server runs as its service account: the core install hook writes, in systemd mode, a systemd drop-in (from the command-line `install` and `upgrade` run as root; a console upgrade runs no install hook, and outside systemd hooks get an empty `Layout.UnitDir`) so that before every start systemd runs [`gpuworks host-identity`](../reference/cli.md#gpuworks-host-identity) as root, which writes those two to `/run/gpuworks/host-identity.json` (`root:<service group>`, 0440, in a root-owned 0755 directory). `hostidentity.Load` uses the file only to fill those two when it cannot read them itself; a value read directly always wins, and `MachineID` and `RootFSUUID` come only from direct reads. A missing file, one over 4 KiB or one that breaks the format fills nothing. `/run` is a memory file system, so a data directory copied to another machine does not carry the old machine's identity.
- The root pre-start step executes only the protected `host-identity-exec` in the unit directory, never the service-writable main program. Command-line install/upgrade publishes the running installer image and checks ownership, permissions and ACLs along the whole path. Console upgrades retain the existing copy. Its identity file format must remain compatible with later server versions.
- Plugins read the same facts through the host API, `POST /host/v1/host-identity` (wire format in the [plugin protocol](../reference/plugin-protocol.md#主机身份)), with the `read:host_identity` permission and the `host.host_identity@1` capability; `pluginhost.IdentityKey` binds the salt to the package id, as for the node reads.
- Diagnostics collections (the console's and `gpuworks support-bundle`) carry `server/host-identity.json`: the same facts salted with the fixed key `core\0support_bundle`, so one machine has the same digests in every collection and no raw value goes into the archive.

## Tables and migrations

Extension migrations serve compiled modules only (forks and tests). Plugin packages are to use storage in their own data directory, and the tables the core persists for plugins are core tables; neither uses this mechanism (ADR 0071).

Migration sets (`store.MigrationSet`) run after the core migrations, in the same `store.OpenWith`:

- Each step and its marker are written to `namespace_migrations` in one transaction. The table is created only when sets are passed; a community open never creates it and writes no schema.
- Every table, index, view and trigger a step creates, changes or drops must carry the set's prefix in its name and in the table it belongs to. Core tables never use the reserved prefix `ee_`.
- A namespace in the database that this build does not know only logs a warning and does not stop startup. A known namespace newer than the binary stops it, and so does a recorded step that is missing from the binary or renamed.
- Core sweeps over every table may reach extension tables, so extension tables store times in UTC and their migrations only add.

`ExtensionTables.Write` runs one transaction on the single writer connection; `Read` runs one read transaction on the read pool (an in-memory database has no read pool and reads on the writer). An extension reads and writes only its own prefixed tables, and reaches core data only through Store methods, the API and the hooks below.

## Hooks and call threads

| Hook | Called where | How often |
|---|---|---|
| `NodeEnrollmentGuard` | Inside the writer transaction of the three node insert paths (`CreateNodePending`, `EnrollKubeNode`, `RegisterNode`) | Every new node insert |
| `SchedulingHolder.Hold` | Inside the writer transaction of every status report and scheduling change; for every node at startup and in every `ApplySchedulingHolds` (including the passes modules ask for), and for the named nodes in `ApplySchedulingHoldsFor` | Every report of every node |
| `CardSchedulingHolder.HoldCard` | Inside the writer transaction of every status report that writes the node's cards, for each card; for every card at startup and in every `ApplySchedulingHolds`, and for the named nodes' cards in `ApplySchedulingHoldsFor` | Every report, for every card |
| `featuregate.Source.Availability` | Every request to an extension route, every console manifest request | Every request |
| `BannerSource.Banners` | Every console manifest request | Every request |
| `Service.Run` | A background loop | Continuously |

When an insert guard refuses, the API answers 403 with the guard's code. The instance rental and provider install paths run a pre-check that takes no writer lock first, so no machine is rented only to be refused. A node a holder holds is cordoned and never drained; an operator cannot make it schedulable (409 with the holder's code), but can still cordon or drain it. Likewise a card a card holder holds takes no new work and is never drained; an operator cannot make it schedulable (409 with the holder's code), but can still stop it.

## Purity rules

There is a single writer connection (`SetMaxOpenConns(1)` in `store/database_open.go`). A hook called inside a writer transaction (insert guards, scheduling holders, card holders) that calls the Store, `Tables()` or anything waiting on them waits for the connection it holds itself: a deadlock. These hooks therefore must:

- not call the Store or `Tables()`, do no I/O, and take no contended lock;
- be O(1): one atomic load plus one map lookup, with no allocation when they do not hold;
- answer from a snapshot the module maintains elsewhere (a `Service`, a bus subscription, an API write), and call `ReapplySchedulingHolds` there when the snapshot changes;
- return a prebuilt `*messages.ProductError` when they refuse.

Feature sources and banner sources are called on request threads and likewise answer from snapshots, never scanning nodes, deployments or tables. The contract tests in `testenv/extmodule` show what breaking the rule does: a guard that writes its own table cannot proceed until the insert transaction ends.

## Availability

Open-source code speaks only of availability: a feature source answers each feature key with full, read-only, locked or hidden, optionally with limits, a link and a coded notice.

- An extension route (`api.RouteSpec`) either names a `Feature` or is marked `Community`, exactly one of the two. The route inventory (`api.RouteInventory`) prints each extension route's access level, action and feature, and `[sign-in]` last on a sign-in route.
- Full: passes. Read-only: reads pass, and so do writes marked `WriteWhenReadOnly` (writes that only remove or relax); other writes get 403 `FEATURE_READ_ONLY`. Locked: 403 `FEATURE_UNAVAILABLE`. Hidden, or a locked feature declared with concealed presentation: 404. When the source supplies its own notice, its code is returned.
- Without a module that provides a feature source, every declared feature is locked (`FEATURE_PROVIDER_ABSENT`).
- Community paths never consult the gate, and running workloads are never interrupted by a change of availability. Core code never names a feature key in a literal: key-shaped literals handed to the gate, `featuregate.Key("…")` conversions, a literal assigned to a `featuregate.Key` constant, a feature declaration's `Key: "…"` (`featuregate.Feature{Key: …}`) and a route's `Feature: "…"` in non-test code are held at 0 by the architecture budget `feature_key_literals_in_core`.
- Extension GET routes are not readable by the AI assistant and MCP unless marked `AIReadable`.

### Availability of the whole server

A module may set the availability of the whole server with `Host.LimitPlatform` (`api.PlatformLimiter`): full, read-only or locked. Without a module that does, the server is never limited; the community server has no such source.

- Read-only: reads and the data plane (`/v1/*` and the console playground's `/api/v1/playground/v1/*`) work as usual, and every change gets 403 `PLATFORM_READ_ONLY`. Locked: reads and the recovery surface work as usual, and changes and the data plane get 403 `PLATFORM_LOCKED`; a refusal on the data plane has the gateway's error shape (`{"error":{"message","type","code"}}`) with the same code. When the source supplies its own notice, its code is returned. A source that answers hidden is read as locked.
- The recovery surface passes under any limit: signing in and the session, health checks, the console manifest and branding, the connection and health-check script of machines that already joined, the Kubernetes and Slurm enrollment routes (a rebuilt node of a cluster that joined gets its identity back there; a new node is a node insert, which the limiting module's insert guard refuses), the management of installed plugin packages (disable, restart, roll back, uninstall; installing or upgrading a package brings new code in and is refused like a change), the server's own upgrade, diagnostics bundles, and the routes of the module that registered the source (so that its page can lift the limit).
- The limit applies before any handler and classifies by the route's pattern rather than guessing from the path. A request under `/api/v1` is authenticated before it is refused: a caller who is not signed in gets the 401 an unlimited server gives, and learns neither which routes exist nor that the server is limited; the data plane and the addresses that hand out enrollment material carry credentials the guard cannot check and are refused to anyone. An extension route that needs no sign-in (a compiled module's `AccessPublic`, a plugin package's `public`) is classified by its method too: a read is served, a change is refused while read-only or locked, with 403 and the limit's code at once, without asking for authentication first (a public route is open to everyone anyway). Only a compiled module's route that declares `SignIn` is served under any limit, as signing in is, whatever its method. A plugin package cannot declare a sign-in route yet: that needs a new manifest schema and comes with step 3 of ADR 0085 ([ADR 0088](../adr/0088-extension-public-routes-under-a-platform-limit.md)). Routes under `/api/v1` are reads or changes by their method; a POST that stores no business state and changes nothing on a node (the assistant, previews of a model or a deployment, tests of unsaved cloud credentials and of an assistant model, the probe of a benchmark target, the SSH precheck of a machine to be added, marking notices read: eleven in all) is a read, named one by one; a check that makes a node open a listener or mount something (the network preflight of a deployment, the storage probe) is refused like a change; a GET that hands out what brings a machine in, or opens an interactive session, counts as a change and is refused under any limit; a GET that changes nothing but reaches outside the server to answer (a node's file and image lists, an instance's log stream, storage and batch file downloads, a cloud account's instance and cluster lists, a registry's tags) is a read, served as usual and named one by one in `platform_limit.go`; a new route outside `/api/v1` must be named in `internal/server/api/platform_limit.go`, or it is refused under any limit. Every route served under a limit is listed in `platform_limit_routes_test.go`: a new GET under `/api/v1` is a read by default, and the test fails because the list lacks it; before adding it, check that it changes nothing, starts nothing and hands out no enrollment material.
- The source is called on every request and follows the pure-function rules: one atomic load, no I/O, no lock, no allocation.
- A limit acts on new requests only: it never stops, evicts or restarts running workloads, and machines that already joined stay connected.

While the server is limited the console manifest carries `platform` (`access`, `link`, `message`). Locked and read-only, the pages show as usual (both serve reads); a notice above them says what is refused and carries a control that opens `link`. The page `link` points to states the matter itself and shows no notice.

The console renders `GET /api/v1/console/manifest` (fields in the [API reference](../reference/api.md)) and derives no availability itself: full shows as usual; read-only passes `readOnly` to the component; locked keeps the menu entry with a lock and shows the locked page with the server's link; a hidden key, or one the manifest does not list, shows nothing.

## Console overlay

The build-time overlay serves forks only; plugin package UIs are to be loaded at runtime (ADR 0071, not built yet).

A separately built server's UI modules are copied to `ui/src/extensions/<id>/` (git-ignored), registered in `ui/src/extensions/index.ts`, and may import only the components, hooks and API client that `ui/src/extensions/sdk.ts` re-exports (the `ee_ui_internal_imports` budget is 0). The build sets `GPUWORKS_UI_OUT_DIR` to write elsewhere; with UI modules checked out and the variable unset, `make ui` and `make release-packages` refuse to build, so nothing is overlaid onto the community edition. The modules' texts ship in their own locale bundles merged into `common`, checked by the same i18n check through `GPUWORKS_I18N_EXTRA_LOCALES` / `GPUWORKS_I18N_EXTRA_MESSAGES`.

The entries a UI module can add:

- Pages (`routes`), side menu entries (`navItems`, which need an icon), settings tabs (`settingsTabs`) and top bar controls (`headerItems`).
- Account menu entries: a `navItems` entry whose `group` is the reserved `account` goes to the account menu at the top right, after the built-in entries and before signing out, ordered by extension id, as text only. The account menu has no room for a locked entry, so a locked one is left out like a hidden one. `account` cannot be the id of a new menu group.
- Admin-only: each kind of entry above can be marked `admin: true` and then follows the role of the user the session check read. A user who is not an administrator sees no menu entry, tab or control, and the page answers as not found; a session whose user could not be read is treated the same way. This shapes only what is shown: the server's route access levels still guard the API.

Banners: a banner whose link opens the current page (query and fragment aside) is left out on that page, which states the matter itself.

A banner source can give a banner an `AckKey` so that the people who see it can acknowledge it: the console shows "Acknowledge" on that banner and, once it is used, no longer shows that user a banner with the same key. The source decides which banners can be acknowledged and what the key is; when the same matter should be raised again (a new stage, a new deadline), the source gives a new key and the banner shows again. A key must match `api.BannerAckKeyPattern`: 1 to 128 characters of letters, digits and `._:-`, starting with a letter or digit; a time in a key is written in UTC ending in `Z` (such as `2027-01-01T00:00:00Z`), since the `+` of a zone offset is outside the set. A key that does not match is dropped by the manifest without an error: the banner shows as usual but cannot be acknowledged. Acknowledgements are kept only in the browser (per user) and never stored by the server, so another browser, or clearing site data, shows the banner once more. Until the console knows who is signed in (right after a sign-in, before `/auth/me` answers) it shows no banner that has a key and records no acknowledgement. Overlay pages read and write the same record through the SDK's `useBannerAck` (which offers no acknowledgement while `ready` is false), so acknowledging the matter on a page also removes the banner above every page. Banners pushed by plugins (`table.banners@1`) carry no acknowledge key.

The SDK carries a major version, `SDK_MAJOR` (1 for now), which goes up only when an export is removed or changes meaning; adding an export leaves it alone, and `ui/tests/consoleExtensionsSdk.test.ts` pins the export list. Files go through the SDK's `downloadFile`, `saveBlob` and `sendForm`:

- `downloadFile` asks with the session and names the file from the answer's `Content-Disposition`, or else from the name the caller gives. An error answer decodes by its code into a localized `APIError`; the error body is never saved as the file.
- `saveBlob` hands a Blob to the browser as a download.
- `sendForm` sends a multipart form and decodes errors as `sendJSON` does.

A UI module's tests are copied to `ui/tests/ext-<id>/` (git-ignored) and run with `UI_EXTRA_TESTS='tests/ext-<id>/*.test.tsx' npm test`. They sit outside `ui/src/extensions/<id>/`, so their imports of `node:test` and testing-library do not count against the import budget, while the `c8 --all --include='src/**'` of `npm run test:coverage` still counts every source file of the module.

## Codes and texts

Failures and refusals an extension produces follow [ADR 0067](../adr/0067-runtime-outcomes-as-codes.md) too: UPPER_SNAKE codes, interpolated params, an English fallback. An extension's codes and their en/zh texts live in the extension's own code and UI modules, not in the open-source `messages` or locale files; without the overlay, the console shows the English fallback.

## Upgrades and installation

The edition checks below matter only to forks: official builds name no edition, and ADR 0071 removes this machinery from the official release path once the first availability authority runs as a plugin.

- A distribution index names its edition inside the signed payload; an index whose edition does not match the build's channel is refused, and the community edition accepts only indexes that name no edition or `community`.
- `upgrade` compares `gpuworks edition` of the installed binary and the staged one and refuses a different edition unless given `--allow-edition-change`; a console upgrade to another edition answers 409 `SERVER_EDITION_MISMATCH`.
- `upgrade` runs the install hooks of the binary doing the upgrade; after a cross-grade, run `install` with the new binary as prompted.
- Going back from an extended build to the community edition, the `extensions:` section of `server.yaml` only logs a warning, the extension tables are left as they are, and extension-held cordons are released at startup.

## Installable extensions

ADR 0071 decides that official and third-party extensions are installed as signed plugin packages (`.gwpkg`) instead of being compiled into the binary. The plugin host is connected to the contract on this page as a built-in module:

- The plugin host is the built-in module `plugins` (`app/plugins.go`, implemented in `internal/server/pluginbridge`). It registers the existing hooks through the `Host`: the node insert guard, the scheduling holder (source `plugins`) and banners. The feature keys installed packages declare go into the feature gate's dynamic set and are answered from tables plugins push and the core validates and persists; the keys compiled modules declare are still answered by the `ProvideFeatures` source, and the two coexist. A compiled module may no longer be named `plugins`.
- Runtime feature declarations: after a package is installed, upgraded, enabled, disabled or uninstalled, the dynamic set is replaced as a whole by the keys the packages installed then declare. The feature gate and the console manifest see them at once, and the `feature` topic tells the console to read the manifest again; no restart is needed. A gate lookup checks the keys compiled modules declare first, then the dynamic set (one atomic load, no lock, no allocation); the `features` of `GET /api/v1/console/manifest` is both merged and sorted by key. With nothing installed the dynamic set is empty and the manifest is byte-identical to before.
- A feature key has one owner: when a package declares a key a compiled module declares, or one another installed package declares (a package upgraded keeps its own keys), the install is refused with 409 `PLUGIN_FEATURE_TAKEN{id, feature, owner}` before approval is asked for, where `owner` is the module's name or that package's id, and nothing is installed. Clashing packages installed before this check still load at boot: the key stays with its earlier owner (the compiled module, or the package of lower id that has answered for it since both were installed), each clash is logged, and no key changes owner; such a package cannot be reinstalled or upgraded until a version drops the clashing key. An installed package that no longer verifies holds none of the keys it declares: while it is broken another package may install over such a key, and once it verifies again the key goes to the lower id of the two.
- A plugin is a child process of the server that talks to the core over `gwx/1` on inherited fds, never enters a hot path and never gets the Store.
- Whether a feature key is gated is declared by the plugin package itself; grant tables accept pushes only from the authority its publisher certificate names; while a plugin is down its restrictions stay enforced, its grants clamp to read-only after a bounded grace, and timelines keep firing on extclock.
- The hook rules on this page (hooks inside the writer transaction are pure, holders cordon without draining, running workloads are never interrupted) apply to the plugin host as well.

The wire formats of manifests, certificates, trust updates, `gwx/1` messages and pushed tables are frozen in `pkg/extapi`; see [Plugin Protocol and Wire Formats](../reference/plugin-protocol.md). Reading, verifying and trusting packages and the `gpuworks plugin` subcommands are in the [CLI Reference](../reference/cli.md#gpuworks-plugin) and the [Install Layout](../reference/install-layout.md).

### The plugin host module

Boot order (the `plugins` module's `Init`, before the router is built):

1. Remove what interrupted installs left and verify every installed package again (rehashing every file this time). A package that no longer verifies or is revoked counts as installed but not verified: the restrictions and banners it pushed stay enforced, its grants are dropped, and it does not start.
2. Load every pushed table with that package list (see "Pushed state" below), enforced before the server accepts its first request and before any plugin starts.
3. Put the feature keys installed packages declare into the dynamic set (a key a compiled module declares is logged and left to the module), and register the hooks.

The module's service (run with the server's other background loops) starts pushed state's writer tick, timelines and capacity selection, then starts every enabled package with a `resident` server part, in parallel and without holding up `serve`; an enabled `on_demand` package is handed to the supervisor and stays `idle` until its first call starts it. Only changed files are rehashed before a start. The supervisor tells pushed state each `resident` plugin's process state: `running` and `degraded` count as up, everything else as down; an `on_demand` plugin pushes no state, so its state is not reported to pushed state and its idle-and-restart cycles write no down records. When the server stops, the plugins stop first and pushed state after, so the moment each plugin went down is persisted and a restart never renews its grace.

The pusher's identity comes from the package verified when it was started: the supervisor numbers every process (a generation rising across all of them), and a push on the host API is taken as the package bound to that connection, with its certificate grants (including the authority `server.yaml` configures for a host-trusted issuer), the permissions its administrator approved, its manifest's codes and that process's generation. A push over the connection of a package that was replaced or changed version is refused (`PLUGIN_STATE_CONNECTION_REPLACED`). The core offers the capabilities `table.<kind>@1` for the first five kinds; packages' `requires` are verified against them and plugins negotiate them in the handshake.

When a package declares `storage`, its data carries a version marker `plugin.<id>.data` (in the core's `plugin_markers` table), which keeps a site from going back to an older build that cannot read that data:

- **Write**: the first time a plugin reaches `running` (handshake accepted) after each start, the host writes the marker as `max(stored version, storage.state_version)` and records the version of the plugin build that ran (which a refused older build names as the one to install); later returns to `running` within the same start read and write nothing. The marker is never lowered: an older build still running does not reduce it. A package without `storage` writes no marker, and neither does a package that has been uninstalled. A write that fails, or that finds the plugins directory held by an install or removal, is only logged and retried on the next health check; a marker that cannot be read keeps the plugin from starting, and the start is retried shortly.
- **Check**: when the marker is newer than the package's `storage.state_version` (0 without `storage`), installing that build is refused with `PLUGIN_STATE_TOO_NEW` and nothing is installed; an installed build (for example an older build installed again after a newer one ran) is checked before every start as well. It is not started, and the list shows it `failed` with `PLUGIN_STATE_TOO_NEW` until a build with a recent enough `state_version` is installed. Before a downgrade, `gpuworks plugin compat` runs the same check.
- **Lifetime**: a plain uninstall keeps the data and the marker, so reinstalling an older build is refused too; only `purge` deletes the marker, together with moving the data directory to `.trash/`.

### Management API

`/api/v1/plugins` are core routes, never feature-gated, and work with every plugin down, crashlooping or removed; the assistant and MCP cannot read them (they are not among the routes the assistant reads, and the handlers refuse the assistant's in-process reads):

| Method and path | Who may call it | What it does |
|---|---|---|
| `GET /api/v1/plugins` | administrators (session or token) | installed packages with their state, reason and the actions available (server-computed), and the `plugins` diagnostics check (see "Health: banners, the diagnostics check and metrics" below) |
| `GET /api/v1/metrics/plugins` | administrators (session or token) | plugin metrics, in the Prometheus text format |
| `POST /api/v1/plugins/install?approve=<approval>&enable=true` | an administrator's login session | the request body is the `.gwpkg`; without the approval, or with a wrong one, it answers 409 `PLUGIN_APPROVAL_REQUIRED` with the approval and the permissions and roles it covers in its params, and installs nothing |
| `POST /api/v1/plugins/{id}/enable`, `/disable` | an administrator's login session | enable and start; stop and keep from starting (its pushed tables stay enforced and it counts as down) |
| `POST /api/v1/plugins/{id}/restart` | an administrator's login session | stop and start again as a new instance, so its crash history and backoff start afresh; an `on_demand` package goes back to `idle`; a disabled package answers 409 `PLUGIN_DISABLED` |
| `POST /api/v1/plugins/{id}/upgrade?approve=<approval hash>` | an administrator's login session | the body is the new version's `.gwpkg`; snapshots the data, switches versions and waits out the health gate, rolling back automatically when the new version fails it (see below); without the approval hash, or with a wrong one, it answers 409 `PLUGIN_APPROVAL_REQUIRED` whose params carry the diff from the installed version, and nothing changes; a package whose id is not the path's answers 422 `PLUGIN_UPGRADE_ID_MISMATCH{id, package_id}` |
| `POST /api/v1/plugins/{id}/rollback?restore_state=true` | an administrator's login session | go back to the previous version (the list's `previous`); `restore_state` also restores that version's data and data marker from its snapshot; without a previous version it answers 409 `PLUGIN_NO_PREVIOUS_VERSION` |
| `GET /api/v1/plugins/{id}/impact` | administrators (session or token) | the uninstall impact preview: the tables deleted, the nodes whose hold is released, the features that become `locked`, the caps lifted or loosened, the banners and overrides, and the digest `confirm` to confirm it with |
| `DELETE /api/v1/plugins/{id}?confirm=<digest>&purge=true` | an administrator's login session | stop and uninstall (`purge` moves the data and snapshots to `.trash/` too and deletes the data marker), delete everything it pushed in one commit and reapply the holds; when `confirm` is not the digest of the impact now it answers 409 `PLUGIN_UNINSTALL_CONFIRM_REQUIRED` carrying the impact and removes nothing; a retry finishes an uninstall that failed halfway |
| `GET /api/v1/plugins/{id}/state` | administrators (session or token) | its part of pushed state: each table's generation, entries, push time and next step, when it went down and its grace, the overrides |
| `GET /api/v1/plugins/{id}/logs` | an administrator's login session | SSE: supervision events (`state` frames, the last 256) and stderr (`stderr` frames, the last 1 MiB), what is kept first, then what follows |
| `GET /api/v1/plugins/catalog` | administrators (session or token) | the imported package indexes and the installed packages: per package, the newest build this server can install, whether it is installable, and the newer builds it will not install with why |
| `GET /api/v1/plugins/overrides` | administrators (session or token) | the overrides in force, sorted by `id` |
| `POST /api/v1/plugins/overrides` | an administrator's login session | loosen one restriction a plugin pushed: delete a node hold, or delete or raise one fleet capacity cap; a reason is required |
| `DELETE /api/v1/plugins/overrides/{oid}` | an administrator's login session | withdraw an override, so the restriction the plugin last pushed is enforced again |

An uninstall is previewed first: a `DELETE` without `confirm`, or with a digest that does not match the impact now (the plugin pushed again, or another version was installed, after the preview), answers 409 and removes nothing; the console shows the administrator the impact and, once confirmed, sends the request again with its `confirm`. The impact reads memory only: pushed state is combined again without the owner and the result thrown away. An uninstall without `purge` keeps the data directory and the data marker (`plugin.<id>.data`), so a reinstall cannot load data an older build wrote; an uninstall with `purge` (also of a package already uninstalled whose data directory is all that is left) moves the directory, data and snapshots included, to `.trash/` and deletes the data marker. Everything the server keeps per package is listed in `pluginbridge.PerPackageArtifacts` (the pushed state tables, the data marker, the files under the package's directory, the in-memory structures keyed by package id); the contract test `TestUninstallLeavesNothingUnderThePackageID` installs a package that uses all of them, uninstalls it, then purges it, and checks each one is gone. New per-package state joins that list in the same change.

Upgrade (the old version stops before the snapshot, so the data does not change while it is copied): once the new package is verified, approved and unpacked (the same version number unpacked before with other contents is refused right then with 409 `PLUGIN_VERSION_CONFLICT`, leaving the old version alone), the upgrade marker `<id>/upgrade.json` (`from`, `to`, `snapshot`, `started`) is written, the running version is stopped, the data directory is copied into a snapshot (a failure answers 500 `PLUGIN_SNAPSHOT_FAILED`, the old version starts again and nothing changes), and the new version is made current and started; any failure after the switch is handled as a failed health gate and rolled back. The new version must come up within the health gate and stay up for 60 seconds in one process; a crash, a failed handshake, a refusal or a stop fails it at once: the new version is stopped, the old one made current again with its data and data marker restored from the snapshot, and started, and the answer is 422 `PLUGIN_UPGRADE_ROLLED_BACK{id, version, previous, cause_code}`. A disabled package, or one without a server part, has no health gate; a package whose upgrade adds host setup steps waits in `needs_host_setup`, which is not a failure. What the package pushed stays enforced throughout; the package counts as down from the moment the old version stops until a version is up again, and the restarts in between do not renew its grace. The request answers synchronously (in up to about 80 seconds) and the upgrade finishes even when the client goes away. A server that finds an upgrade marker at boot (an upgrade stopped in the middle) goes back to `from` and restores the data from the snapshot before it verifies packages, loads pushed state or starts any plugin, logging it with `audit=true`; when the new version had not become current yet it only removes the marker (and a restoring rollback stopped after it switched versions and before it recorded the approval gets the older version's own approval hash recorded). A snapshot is written as `.partial` and renamed, only the newest 2 are kept, an uninstall keeps them and a purge removes them with the data. A manual rollback without `restore_state` leaves the data as the newer version left it and answers 409 `PLUGIN_STATE_TOO_NEW` when the older version is older than the data marker; with it the data comes back from that version's snapshot (409 `PLUGIN_SNAPSHOT_MISSING` without one). While an upgrade is in progress, an install of the same id answers 409 `PLUGIN_UPGRADE_IN_PROGRESS`.

The administration calls lock per package id: an upgrade waiting out its health gate holds up only its own package, and disabling or uninstalling another package answers as usual; a disable or uninstall of the same package waits at most 10 seconds (while the old version is still stopping or its data still being copied, until the health gate begins), then ends the health gate (the upgrade rolls back) and goes on. Stopping the old version and copying its data hold no lock of the plugins directory, so other packages install, enable and disable meanwhile. `gpuworks plugin upgrade` and `rollback` work offline with the server stopped (with the snapshot, without a health gate; the new version runs from the next start), and refuse while the server runs, naming the API route instead.

Supervision events and stderr are kept per package, across processes, restarts and disable and enable, until the uninstall; a `restart` starts a new instance whose crash history starts afresh, and the events are kept.

Every call writes a structured log record with `audit=true` saying who did what to which package and how it ended; an override call also records the administrator's user id and username, and the reason.

An override (ADR 0071, decision 10) only loosens a restriction entry, and what decides is whether pushed state holds that entry: the owner may be running, down, `broken`, `revoked`, or uninstalled with its rows still stored (an unfinished uninstall), and a broken or revoked owner is the case that needs relief most. A rule it breaks answers 422 `PLUGIN_OVERRIDE_REJECTED` with `rule` `not_restrict`, `no_entry`, `action`, `not_looser`, `reason` (a reason over 1024 bytes; a missing reason is 400 `FIELD_INVALID`) or `superseded` (the owner pushed a change of that entry after the check and before the override was stored, and its new decision wins); withdrawing an override that does not exist (a superseded one included) answers 404 `PLUGIN_OVERRIDE_NOT_FOUND`. An override of a live owner is allowed too: when its next push changes that entry, its new decision wins, the override is superseded and deleted, administrators see the banner `plugins.override_superseded.<override id>` (`PLUGIN_OVERRIDE_SUPERSEDED`, held in memory only, not shown again after a restart) for 24 hours of extclock, and the audit log gets a record; a push that leaves the entry as it was keeps the override. A second override of the same entry replaces the first; uninstalling the owner deletes its overrides and their banners. A correction of extclock is audited the same way and shows the banner `plugins.clock_corrected`. States in the list: `disabled`, `installed` (no server part), `broken` (no longer verifies), `revoked` or `needs_host_setup` (see [Host setup](#主机配置)) when the server part cannot run; otherwise the supervisor's process state (`idle` for an `on_demand` package that is not running). The fields are in the [API Reference](../reference/api.md).

### Local administrator floor

`users.local_admin` marks who can still recover the site when every plugin is down, removed or crashlooping (ADR 0071, decision 11):

- Migration 225 sets it for every administrator the database already had; the administrator created at first start gets it; the offline `gpuworks reset-password` sets it again for an administrator (other users are unaffected). Users created later start without it, and changing one's own password in the console leaves it alone.
- The effective value is computed each time a request reads its user: `local_admin && role == admin && login session`. A demoted user loses the floor at once, without a write; a platform API token never carries it; it is never signed into a JWT nor read from one. `local_admin` in `GET /api/v1/auth/me` reports this effective value, for display only.
- The recovery surface is core routes: login, `/auth/me`, the password change and `/api/v1/plugins/*` pass no feature gate and ask nothing of plugin-fed sources (feature availability, banners). The contract test `TestContractLocalAdminDuringCrashloop` puts two plugins into crashloop (one holding every node, one pushing a fleet cap of zero cards), and the local administrator still signs in afresh, lists, raises the cards cap and overrides a node hold so that node is schedulable again, disables and uninstalls each package, every call answering within 10 seconds.

The evaluators of pushed constraints, admission holds and pushed roles do not exist yet, so nothing needs to exempt by this flag today; when they arrive, they check it first.

### Not built yet

- Host API endpoints not listed in this guide; per-action authorization and persisted route audit records (today audit is a structured log line only).
- Bundled packages in the catalog.
- Running plugins on the leader only under high availability.
- No capability of the manifest vocabulary other than `table.<kind>`, `host.node_identity`, `host.nodes`, `host.host_identity`, `host.clock`, `host.state_read`, `host.derive_key`, `host.setup` ([Host setup](#主机配置)) and `routes` is offered yet: the other read surfaces (`host.*`), `host.notify`, `metrics.expose`, settings (`host.config`), WebSocket routes (`routes.websocket`) and the part capabilities `node.app` (which `node:workloads` and `node:jobs` also name) and `ui.bundle`. A package that requires one is refused with `PLUGIN_INCOMPATIBLE`; one that only uses it installs but does not get the service ([Permissions](../reference/plugin-protocol.md#权限)).

### Trampoline: `gpuworks plugin-exec`

The server starts every plugin through its own binary: `gpuworks plugin-exec --profile <json> -- <executable> serve` (`internal/server/pluginexec`, a subcommand not meant for people). The trampoline applies the sandbox as the process-to-be and then `execve`s the plugin, so the host applies the sandbox and the plugin cannot undo it:

| Step | Linux | macOS |
|---|---|---|
| Parent-death signal | `PR_SET_PDEATHSIG(SIGKILL)`, and a check that the parent is still the server, or nothing starts | None; the SDK exits on fd EOF and by polling `getppid` |
| Privileges | `PR_SET_NO_NEW_PRIVS` | — |
| Process group | A group of its own, which the server signals as a whole | Same |
| Resources | `RLIMIT_CORE=0`, `RLIMIT_NOFILE=4096`, nice 5; `RLIMIT_NPROC` counts per user, so it is not set while the plugin shares the server's user, and the cgroup's `pids.max` bounds processes | Same (no cgroup) |
| Descriptors | fd 3 and 4 must be sockets; stdin and stdout are `/dev/null`; stderr is the log | Same |
| Environment | Only `PATH`, `TZ` and the proxy and CA variables are kept, plus `HOME` = the data directory, `TMPDIR`, `LANG=C.UTF-8` and `GWX_*`; the proxy variables pass unchanged, so a user name and password in a proxy URL reach every plugin too, and proxy credentials should grant only what a plugin may also use | Same |
| cgroup | When the server's cgroup is delegated, creates `<service cgroup>/plugins/<id>`, writes `memory.max` (`mem_mib`), `memory.high` (90% of it), `cpu.weight=50` and `pids.max=512`, and moves in; when it cannot, it only warns and the plugin starts anyway, its memory bounded by the RSS watchdog (see [Supervision](#监管)) | — |
| Landlock | ABI ≥ 1: the package directory readable and executable, the data and tmp directories with every right, CA files, `resolv.conf`, `hosts` and time zones read-only, the device nodes in the manifest's `devices` read-write; with [host setup](#主机配置) applied, also read, write, ioctl and listing for the nodes beneath the device directory, and the sysfs and udev paths device discovery reads; from ABI 6 also no signals to processes outside the domain (the server and other plugins) | None |

The server's cgroup counts as delegated when the host runs cgroup v2, `cgroup.subtree_control` is writable, and either systemd marked the cgroup delegated (the `user.delegate` or `trusted.delegate` attribute, systemd 251 and later) or the server runs as a non-root user in its own unit's cgroup, `gpuworks.service` (systemd hands a unit's cgroup to its `User=` only with `Delegate=yes`). Root can write any cgroup and a desktop session's scopes belong to their user, so being writable is not enough on its own. The unit `gpuworks install` writes carries `Delegate=yes` ([install layout](../reference/install-layout.md#systemd-单元)). With a delegated cgroup, the server moves itself into `<service cgroup>/server` before it starts the plugin host, because cgroup v2 lets only a cgroup without processes enable controllers for its children. A plugin's cgroup directory is removed when it stops or is uninstalled, and processes the plugin started outside its process group are ended with it through `cgroup.kill`. Once a plugin runs, the server checks that it really is in its own cgroup with the memory controller enabled before it counts as having one; otherwise the RSS watchdog takes over.

A package that requires Landlock (certificate `min_isolation: landlock`) does not start on a host without it: the trampoline exits with code 78 and writes `PLUGIN_ISOLATION_UNAVAILABLE` to stderr. The trampoline's other exit codes: 64 for a bad command line, 71 when applying the sandbox failed, 127 when the plugin could not be executed.

The Landlock cases in `internal/testenv/extplugin` (the plugin cannot read `master.key` or paths outside its package, and on Landlock ABI 6 or later cannot signal a sibling process) skip in the ordinary suite when Landlock is unavailable to the process. `make test-plugin-landlock` runs four named cases with `GPUWORKS_TEST_STRICT=1`, requires an explicit PASS for each, and rejects every SKIP; unavailable Landlock fails the gate. CI's mandatory `core-plugin-sandbox` job uses a dedicated runner bearing both `gpuworks-l3` and `gpuworks-plugin-sandbox` tags and runs this gate in a non-root Go container with the default seccomp profile (the ordinary L3 pool includes kernels without Landlock), without changing L2/L3 or production plugin isolation. On both success and failure it retains the kernel version, visible Seccomp/NoNewPrivs status, and complete test log. The tests also record the raw ABI probe result and errno to help distinguish kernel capabilities from filtering policy; an unavailable Landlock probe alone does not identify its cause.

### Supervision

`internal/server/pluginhost` runs one supervisor per package. The goroutine that starts a plugin stays locked to its OS thread for the plugin's whole life: on Linux the parent-death signal belongs to the thread that forked the child, and the Go runtime retires the thread of a locked goroutine that ends, so without the lock another goroutine ending could take the plugin with it.

| Item | Default |
|---|---|
| `initialize` deadline | 10 seconds |
| Health probe | Every 10 seconds, 3 seconds each; after 3 misses in a row the plugin is killed and restarted (`PLUGIN_UNHEALTHY`) |
| Stall detection | The fd 3 connection is checked every second: a write blocked longer than 10 seconds, or request body bytes waiting to be sent while no request body data (DATA frames) has been written for 10 seconds, kills and restarts the plugin (`PLUGIN_STALLED{id, stalled_s}`), whether or not its health probes pass |
| Crash restart | From 1 second, doubling, up to 5 minutes; 10 minutes of healthy running starts again from 1 second |
| Crashloop | 5 crashes within 10 minutes enter `crashloop` (`PLUGIN_CRASHLOOP`), retried every 5 minutes after that, so unattended offline sites recover by themselves |
| Stop | `POST /gwx/v1/shutdown`, SIGTERM to the process group 10 seconds later, SIGKILL 5 seconds after that |
| Memory | A plugin with a cgroup of its own is bounded by `memory.max`. For one without, the RSS watchdog sums the resident memory of the plugin's whole process group every 10 seconds (from `/proc/<pid>/stat` on Linux, `ps` on macOS); above 1.5 × `mem_mib` (default 128 MiB) it sends SIGKILL to the group and restarts the plugin as a crash (`PLUGIN_MEMORY_EXCEEDED`). When the memory cannot be read it logs one warning and leaves the plugin running. A child that moved to a process group or session of its own is not counted |
| Call concurrency | One slot pool for plain calls and one for streamed calls, each of at most `max_concurrency` (default 8); a quarter of each (at least 1, none when there is a single slot) is kept for administrator calls; beyond that `PLUGIN_BUSY` at once, never queued; health probes take no slot |
| Upgrade health gate | the new version comes up within 80 seconds and stays up for 60 (`AwaitHealthy`); the gate starts an `on_demand` plugin once |
| Calls without a deadline | 60 seconds; an `on_demand` plugin's cold start counts against this time too |
| stderr | Forwarded to the server log with `plugin=<id>`, JSON lines keep their level; 50 lines a second with bursts of 500, the excess counted and reported; the last 1 MiB kept for diagnostics |

A `resident` plugin starts as soon as it is added; an `on_demand` plugin starts on its first call (calls arriving together start it once and all wait for it), stops after 10 idle minutes (it first refuses new calls in the same step, so a call arriving at that moment waits for the next start instead of being refused), is refused at once while crashlooping, and may not push state (`PLUGIN_STATE_ON_DEMAND`); on install and on recheck, an `on_demand` package that holds a `push:` permission or declares features is refused with the same code. Only `running` and `degraded` count as up; every other state (`starting`, `backoff`, `crashloop`, `stopping`, `stopped`, `idle`, `failed`) counts as down; this is reported to pushed state for `resident` plugins only. A plugin that refuses the handshake for a reason retrying cannot change (such as `PLUGIN_CAPABILITY_MISSING`), answers for another build (`PLUGIN_HANDSHAKE_MISMATCH`) or picks a capability version that was not offered or approved (`PLUGIN_CAPABILITY_NOT_OFFERED`) enters `failed` and is not restarted until it is started again.

### Health: banners, the diagnostics check and metrics

The plugin host module judges every installed package again whenever a plugin's state changes or the installed packages change (install, enable, disable, uninstall, recheck). The result is held in memory only; it adds no persisted fact. Only enabled packages are judged: a package an administrator disabled reports nothing.

- **Banners**: for administrators only, carried in the console manifest; when they change, the console is told on the `feature` topic to read the manifest again. A crashlooping package raises `plugins.crashloop.<id>` (`PLUGIN_CRASHLOOP`, with its status params: `id`, `crashes`, `window_s`, `retry_s`); a revoked package raises `plugins.revoked.<id>` (`PLUGIN_REVOKED`, with the params of its list row: `id`, `version` and `replacement`, which is empty when no replacement is named), both `danger`. A package that stopped verifying because it needs a capability this server does not offer raises `plugins.incompatible.<id>` (`PLUGIN_INCOMPATIBLE`, with the params of the install refusal: `id`, `capability`, `version`, `offered`, `min_core_hint`), a `warning`. A banner goes away as soon as the plugin runs again, is disabled or is uninstalled. Pushed state's own down banner (`PLUGIN_DOWN`) stays as it is.
- **Diagnostics check `plugins`**: carried as `check` in `GET /api/v1/plugins` (absent with nothing installed, so that list is byte-identical to before), with a `proto.Check*` status. It is `fail` (`CHECK_PLUGINS_FAILING{ids}`) while an enabled package is crashlooping, `failed`, `broken`, revoked or `needs_host_setup`; otherwise `warn` (`CHECK_PLUGINS_INCOMPATIBLE{ids}`) while one is incompatible; otherwise `pass` (`CHECK_PLUGINS_OK`). `ids` lists the package ids, sorted and comma-separated.
- **Metrics**: `GET /api/v1/metrics/plugins` answers them in the Prometheus text format (0.0.4), scraped by administrators with a login session or a token (a read token too); the assistant cannot read it. These metrics are for external collection, are not persisted in the internal time-series store and are absent from the stored-metric catalog. Call latency exports histogram buckets, sum and count, with no stored quantile series. The `id` label takes only installed packages, and a package's series end when it is uninstalled:

| Metric | Type | Meaning |
|---|---|---|
| `gpuworks_plugin_up{id}` | gauge | Whether the plugin serves calls now (`running` or `degraded`) |
| `gpuworks_plugin_restarts_total{id}` | counter | Restarts after a crash or failed health checks; the first start and an `on_demand` plugin's start on demand are not counted |
| `gpuworks_plugin_state{id,state}` | gauge | One series per state, 1 for the current state and 0 for the others; the state is the list's `state`, including `needs_host_setup`, including `needs_host_setup` while host setup is missing |
| `gpuworks_plugin_calls_total{id,class,outcome}` | counter | Calls through the host; `class` is `member`, `admin`, `member_stream` or `admin_stream`, `outcome` is `ok` (the plugin answered, its own refusals included), `unavailable`, `busy`, `timeout` or `failed` (`PLUGIN_UNAVAILABLE`, `PLUGIN_BUSY`, `PLUGIN_TIMEOUT` and `PLUGIN_ROUTE_FAILED` respectively) |
| `gpuworks_plugin_call_seconds{id,class}` | histogram | Time from a call's start until the response headers or the refusal; bucket bounds 0.005, 0.025, 0.1, 0.25, 1, 2.5, 10, 30 and 60 seconds |
| `gpuworks_plugin_push_rows_total{id,kind}` | counter | Entries in committed pushes: `availability` rows, `node_holds` nodes, `banners` banners, `attestations` statements, and 1 per `fleet_capacity` push; a refused push or one a newer push superseded is not counted |

Counting on the call path is an atomic add to a fixed slot and allocates nothing; the counters survive a plugin's restarts, disable and enable, and are dropped when it is uninstalled.

### Calls and the route table

The plugin host module calls a plugin through `Bridge.Call`; plugin routes use it through `Bridge.Forward`, which signs the principal for the process that takes the call (see "Plugin routes" below). Only a package that is installed, enabled and verified, with a server part, is called; any other is refused before any process starts: `PLUGIN_NOT_INSTALLED` when it is not installed, otherwise `PLUGIN_UNAVAILABLE` whose params carry the package's state (`disabled`, `broken`, `revoked`, `installed`, or `failed` when its start was refused). The caller gives the call's class: administrator calls (`admin` and `admin_session` routes) may use the reserved slots, and streamed calls use the stream pool. A failed call falls in one of four classes, which tell the caller whether it may retry:

| Code | When | Retry |
|---|---|---|
| `PLUGIN_UNAVAILABLE{plugin, state}` | The request never reached the plugin: not supervised (`state` is `stopped`), not running, crashlooping, or the connection failed before the request headers were written. `state` is for programs only and the text does not interpolate it (pushed state's clamp banner uses the same code with only `plugin` and `feature`) | Yes |
| `PLUGIN_BUSY{id, limit}` | Every slot the call's class may use is taken; `limit` is the bound that refused the call: the pool size for an administrator call, the pool size less the reserved slots for any other | Shortly |
| `PLUGIN_TIMEOUT{id, timeout_s, state}` | The deadline passed: `state` is `starting` while an `on_demand` plugin was still cold starting, `running` while the plugin was handling the call | Depends on the call |
| `PLUGIN_ROUTE_FAILED{id}` | The call failed after the plugin had the request (it crashed or hung up), so the request may have been partly applied | No |

A write that fails after the request headers were written counts as delivered: the caller is told the request may have been partly applied rather than led into a blind retry. A plugin's own refusal is a normal response.

When a plugin stops reading request bodies, the unread bodies fill HTTP/2's connection-level flow-control window, so the bodies of other calls on the same connection cannot be sent either, while the health probes the SDK answers on another goroutine still pass. The host therefore meters progress per connection: while request body bytes wait on flow control in the HTTP/2 client, any DATA frame written on the connection counts as progress, so a plugin that reads slowly but keeps reading is not affected; a body small enough to fit the windows leaves no bytes waiting, so a handler that reads it later is not a stall. A stalled plugin is killed and restarted (`PLUGIN_STALLED`), and the stuck call answers `PLUGIN_ROUTE_FAILED`. The pending-byte count and progress timestamp change together under a connection-local lock, with no allocations per request or write. Stall durations use monotonic elapsed time, so host wall-clock corrections do not consume the budget. A delayed body reader cannot overwrite more recent DATA progress; cancellation clears its pending bytes.

The route table is compiled from the installed packages' manifests: an enabled package that verifies serves its routes; a disabled package has none (404, and absent from the route inventory), even when it is also revoked or no longer verifies; a revoked package, and one that no longer verifies but verified earlier at the same version, keeps its routes and answers them with 503 `PLUGIN_UNAVAILABLE` without calling the plugin (a package already broken at boot has no routes until it verifies again). After every install, enable, disable, uninstall or recheck the whole table is compiled again and swapped atomically; matching takes no lock, walks one path segment at a time with literal segments tried before `{name}` parameters, and allocates once, for the parameters only; a request keeps the table it started with. The authorization snapshot is bound to the verified manifest digest: if a different build is selected before forwarding, the request is refused with `PLUGIN_UNAVAILABLE` before signing or sending. A restart of the same build continues to work with its fresh process key.

### Host setup

Plugins never run as root. Host steps that need root come only from the manifest's closed `host_setup` vocabulary (today only `usb_device`), rendered by the core (`internal/server/extpkg/hostsetup`, which depends on `pkg/extapi` only); the rules are in the [plugin protocol](../reference/plugin-protocol.md#主机配置hostsetup1).

- **Who applies them.** `sudo gpuworks plugin host-setup [--id ID | --prune]` ([CLI Reference](../reference/cli.md#gpuworks-plugin)). The core install hook of `gpuworks install`, `upgrade` and `uninstall` also runs when a package on the host declares host steps or files this feature wrote are present: install and upgrade apply every installed package's steps and remove what is no longer needed, uninstall removes everything (the device group stays). On a host without such packages the hook is not added and the install flow is exactly as before.
- **Order.** Create the group (`plugin host-setup` also adds the service account to it; the install hook leaves the account alone and puts the group in the service unit's `SupplementaryGroups=`) → write the rule files → `udevadm control --reload-rules` and `udevadm trigger --action=change` (usb, hidraw) → write the records last. A failure on the way leaves no record, so the package keeps waiting and the next run redoes it; when nothing changed, no command runs.
- **Record.** `/etc/gpuworks/plugins/<id>.applied`, owned by root, 0644, holding the digest of the steps (not the approval hash: an upgrade that changes only permissions needs no new root step). It is not kept in `control.json`, which the service account owns; the server accepts only a record owned by root and writable by no group or other user.
- **Start gate.** When a package has host steps and the record is missing or differs, the plugin host does not start it: state `needs_host_setup`, `PLUGIN_HOST_SETUP_REQUIRED{id, command}`. After the command runs, the Enable action this state offers checks the record again and starts it, and so does restarting the server; an upgrade that changes the steps returns it to `needs_host_setup`. The device directories and read paths enter the sandbox only when the record matches.
- **Boundary.** A device's nodes appear when it is plugged in, under names nobody knows in advance (`/dev/hidrawN`, `/dev/bus/usb/<bus>/<device>`), so the sandbox grants the directory `/dev`, not nodes; the boundary between devices is file permissions: the rules set group `gpuworks-usb` and 0660 on the matched device's nodes only, and every other node keeps its own owner and mode. So the grant covers every node under `/dev` the service account can already open, not only the matched USB devices: the plugin runs as the service account, so it can open nodes that are 0666 by default (such as `/dev/nvidia*`, `/dev/fuse` and some `/dev/dri/renderD*`) and existing files in `/dev/shm` and `/dev/mqueue`; hidraw nodes sit directly in `/dev`, and Landlock cannot narrow a grant below a directory. No symlink is created, because Landlock checks the inode a path resolves to and a link grants nothing more. `TestDeviceDirGrantsNodesCreatedLater` proves it: a node created in the granted directory after the sandbox applies opens, so does a symlink to it outside the grant, and a node in a sibling directory gets `EACCES`.
- **Other platforms.** macOS has neither udev nor Landlock device rules; a package that declares host steps is refused at install with `PLUGIN_HOST_SETUP_UNSUPPORTED`.
- **One installation per host.** The rule files and records live at host-wide paths named by package id only, so `install`, `upgrade`, `uninstall` and `--prune` remove what another installation on the same host wrote (see [install layout](../reference/install-layout.md)).
- **Uninstall.** The rule files and records belong to root, so an offline `gpuworks plugin uninstall` cannot remove them; when they remain it prints `sudo gpuworks plugin host-setup --prune`.

### Plugin routes

The routes an installed package declares in its manifest are mounted under `/api/v1/x/<package id>/` (the core offers `routes@1`; a package declaring a WebSocket route needs `routes.websocket`, which the core does not offer, and is refused with `PLUGIN_INCOMPATIBLE`). One handler serves `/api/v1/x/*` and reads the route table above, so an install, enable, disable or uninstall takes effect at once, without a restart. The namespaces of extensions compiled into the server keep priority: their routes are registered first, and a request in such a namespace that none matches answers the core's 405 or 404 and never reaches a plugin. A package whose id equals a compiled module's name (or that of an extension it mounts) is refused at install with 409 `PLUGIN_ID_TAKEN{id}`; one already installed under that id is `broken` with this code at boot, and neither runs nor has routes.

Each request goes through these steps in order, and a step that refuses ends it:

1. **Canonical path**: the raw path (`RawPath` when set). A request with a `.` or `..` segment, an empty segment (`//`), a backslash, or an escaped `/`, backslash, `.` or `%` (`%2F`, `%5C`, `%2E`, `%25`, in either case) answers 400 `PLUGIN_ROUTE_PATH_INVALID`, before authentication, matching or any call. A path that names no package (`/api/v1/x`, `/api/v1/x/`, or one that starts `/api/v1/x//`) is no plugin's: it answers the core's 404, as without the mount. Matching and forwarding use the same string, and other escapes (such as `%20`) reach the plugin as sent.
2. **Match**: a path that no route has answers the core's 404 (401 when not signed in, byte for byte what the server answers without the mount), and a path with no route for the method the core's 405; neither calls the plugin.
3. **Authentication**, with the core's middleware by the route's `access`: `admin` needs an administrator (session or platform token; a read token only reads), `admin_session` an administrator's login session, `member` any signed-in user or platform token (a read token only reads). A session's cross-site write is refused and the session renewed as on core routes. `public` skips authentication and is rate-limited by client address: a token bucket per route and address (the manifest's `rate`, or 60 a minute with a burst of 30), with the address taken as for login throttling (forwarded only by a trusted proxy, IPv6 grouped by /64), in an LRU of at most 10,000 addresses. Over the rate it answers 429 `TOO_MANY_REQUESTS` with `Retry-After`.
4. **Authorization**: a seam right after authentication that passes every caller today; a later authorization layer goes here, so a hidden feature's 404 still comes before a forbidden action's 403.
5. **Feature gate**, for a route that names a `feature`, as for compiled extension routes: hidden answers 404, locked 403, and read-only passes reads and `write_when_read_only` writes only. An `ungated` route skips it.
6. **Assistant and MCP**: they read only GET routes marked `ai_readable`, any other answers 404; the assistant's route list names only those.
7. **Body cap**: a `Content-Length` above `max_body` answers 413 `REQUEST_TOO_LARGE{max_bytes}` before the plugin is called; a body without a length that outgrows the cap ends the forward and answers 413 too.
8. **Audit**: once a non-GET request ends, a write the body cap refused included, a structured log line with `audit=true`: package, version, action, method, route, the caller's kind and user, the status, the request id and the time taken.
9. **Deadline**: a plain route's whole request is bounded by `timeout_s`; on a `stream` route `timeout_s` bounds only the wait for the response headers, after which every write the plugin makes restarts an idle timer and a minute without one cuts the connection. An `on_demand` plugin's cold start counts against `timeout_s`.
10. **Inbound headers stripped**: a client's `Gwx-*`, `X-Gpuworks-*`, `Authorization`, hop-by-hop headers (including those `Connection` names), the address it claims (`Forwarded`, `X-Forwarded-*`, `X-Real-Ip`) and `Cookie` are not forwarded; a `cookies: true` route gets only the cookies whose names are not the core's (the session cookie and any name starting with `gpuworks`) and do not start with `__Host-`. A valid `traceparent` is forwarded as it is, otherwise a fresh one.
11. **Signed forward**: the plugin host signs `Gwx-Principal`, `Gwx-Principal-Mac` and `Gwx-Feature-Access` with the principal key of the process that takes the call (the principal carries `route_action` and `route` and expires `timeout_s` plus 5 seconds after signing; a request id that is too long or not printable is clipped to 128 bytes or replaced), and forwards to the plugin's `/r<route path>`. `admin` and `admin_session` routes count as administrator calls (they may use the reserved slots), and `stream` routes use the stream pool.

When the call fails:

| Condition | Status |
|---|---|
| `PLUGIN_UNAVAILABLE` (the request never reached the plugin, including routes of `broken` and `revoked` packages) | 503 with `Retry-After: 5` |
| `PLUGIN_BUSY` | 503 with `Retry-After: 1` |
| `PLUGIN_TIMEOUT` | 504 |
| `PLUGIN_ROUTE_FAILED` | 502, no `Retry-After` |
| a public route over its rate | 429 `TOO_MANY_REQUESTS` |
| no matching route | the core's 404; the plugin is never called |

A plugin's response reaches the client only through an allowlist:

- **Headers**: only `Content-Type`, `Content-Length`, `Content-Encoding`, `Content-Disposition`, `Cache-Control`, `ETag`, `Last-Modified` and `Retry-After` pass; `Location` passes only while it is a canonical path under `/api/v1/x/<id>/`; everything else (`Access-Control-*`, hop-by-hop headers, the plugin's own `Content-Security-Policy` and so on) is dropped. Every response carries `X-Content-Type-Options: nosniff` and `Content-Security-Policy: sandbox; default-src 'none'`.
- **Content type**: `text/html` and `application/xhtml+xml` pass only from a route that declares `html: true`, otherwise the answer is 502 `PLUGIN_ROUTE_RESPONSE_INVALID{id}`; a missing content type becomes `application/octet-stream`; one that is not a single media type (such as `text/plain, text/html`, whose last entry a browser takes) answers 502 too; any other is sent in normalized form.
- **Cookies**: only a `public` route with `cookies: true` sets cookies, each with its path forced to `/api/v1/x/<id>/` and its `Domain` removed; one named like a core cookie, or starting with `__Host-` or `__Secure-`, is dropped. The filter applies to the plugin's headers only: a renewed session cookie the core middleware already wrote still reaches the client.
- **Product errors**: a body marked `Gwx-Product-Error` must be a valid gwx/1 error (64 KiB at most) under a 4xx or 5xx status, with a code the package's manifest lists in `codes` or one of the wire's own `PLUGIN_WIRE_*` codes (a plugin never answers with a code the core or another package owns), and passes with the plugin's status, code and params; anything else answers 502 `PLUGIN_ROUTE_RESPONSE_INVALID`.
- **Body**: a `stream` route's body is forwarded chunk by chunk and flushed at once, any other one up to 64 MiB. A failure after the headers are sent (the plugin hangs up, the length does not match, the cap is passed, a stream goes idle) aborts the connection rather than leave a truncated 2xx.

The route inventory (`api.RouteInventory`) prints no line for the mount itself and one line per plugin route: `METHOD /api/v1/x/<id><pattern> [access=<access>] [action=<action>] [feature=<key>]` (`[ungated]` when ungated) `[x=<id>@<version>]`. With nothing installed the inventory is byte for byte what it is without the mount; a disabled package does not appear, a `broken` or `revoked` one does. The scale case `TestFleetPluginRouteLoad` (`GPUWORKS_TEST_PERF=1`) measures the server's CPU and memory per request under public traffic from 50,000 client addresses; run it after changing the plugin request path.

### gwx/1 connections and the host API

Each plugin gets two socketpairs. On fd 3 the server is the HTTP/2 client: first `initialize` (the server's capabilities, the approved grant, the tables it holds, and a principal key fresh per spawn), checking that the answer's `id`, `version` and `manifest_sha256` match the package and that `use` stays within what was offered and approved; then health probes, and forwarded routes and service calls, each with `Gwx-Deadline-Ms`. On fd 4 the server serves the host API: `PUT`/`PATCH /host/v1/state/{kind}` goes to the pushed-state implementation (an interface), `GET /host/v1/health`, and the reads `POST /host/v1/node-identity`, `GET /host/v1/nodes` and `POST /host/v1/host-identity` (`pluginhost.Reads`, which app implements with the store and this machine's host identity; without `Reads` they answer `PLUGIN_HOST_ROUTE_UNKNOWN`, a permission not approved answers `PLUGIN_HOST_NOT_PERMITTED`, a capability not negotiated `PLUGIN_CAPABILITY_NOT_OFFERED`, and arguments out of bounds `PLUGIN_HOST_ARGUMENT_INVALID`), and the clock, ready, own-state and key calls (`GET /host/v1/meta`, `POST /host/v1/clock/correct`, `POST /host/v1/state/ready`, `GET /host/v1/state/{kind}`, `POST /host/v1/derive-key`; the capabilities, roles and permissions each needs are in [Plugin Protocol](../reference/plugin-protocol.md#宿主接口的时钟就绪自有状态与派生密钥); `pluginbridge` connects them to pushed state, and `app` derives keys from the master key); 20 calls a second per plugin with bursts of 100 (`PLUGIN_HOST_RATE_LIMITED`), and on top a bucket of 2 inventory pages a second with bursts of 4. The owner is the package the connection was bound to at the handshake, and a push carries no identity field; a push that arrives before the handshake is accepted waits for the outcome and gets `PLUGIN_HOST_NOT_READY` if it fails.

### Plugin SDK: `pkg/pluginsdk`

Plugin authors use `pkg/pluginsdk` (a module of its own like `pkg/extapi`, depending only on the standard library and `pkg/extapi`): `pluginsdk.Main(cfg)` opens the two descriptors, answers the handshake (protocol and capability negotiation, refusing with `PLUGIN_PROTOCOL_UNSUPPORTED` or `PLUGIN_CAPABILITY_MISSING`), runs `Start` once the handshake is accepted, answers health, and hands every other call to the plugin's `http.Handler`; it exits on `shutdown`, when the server's end closes, or when its parent is gone. `PushState` skips a table the server already holds with the same digest and numbers pushes `max(own counter, held generation)+1`, so a plugin whose storage was restored or wiped is never refused as stale. `Meta`, `CorrectClock`, `Ready`, `PushedState` and `DeriveKey` make the host API calls above, and return `ErrNotNegotiated` at once when the handshake did not negotiate their capability. `pkg/pluginsdk/plugintest` plays the server in process over real socketpairs, for a plugin's own tests; it refuses these calls with the server's codes, reasons and params, and derives keys from a fixed test master key.

### Conformance suite: `pkg/pluginsdk/conformance`

A plugin author runs the conformance suite from a test in the plugin's own module, against the binary that goes into the package:

```go
func TestConformance(t *testing.T) {
	conformance.Run(t, conformance.Config{
		Binary:   "bin/linux_amd64/acme-backup",
		Manifest: "manifest.json",
	})
}
```

`Run` reads the manifest (it fails at once when the manifest does not decode, declares no `runtime`, or the binary is not executable), then starts the binary several times: as the server does, it hands the binary two socketpairs as fds 3 and 4 and only the environment the server gives (`PATH`, `TZ`, `HOME`, `TMPDIR`, `LANG` and the gwx/1 variables; `Config.Env` is only for the author's own test switches), without the sandbox, against `plugintest`'s fake server (`plugintest.StartProcess`). The handshake grants every permission and role the manifest declares, as after an administrator's approval and a certificate's grant. Each rule is a subtest named after it (`conformance.Rules()`), so a failing subtest names the broken rule, with the plugin's stderr attached:

| Rule | Check |
|---|---|
| `initialize_answers_as_the_installed_package` | `initialize` is answered within 10 s, with the manifest's `id`, `version` and `manifest_sha256` |
| `uses_only_declared_capabilities` | the answer's `use` holds only capabilities from the manifest's `requires` and `uses`, at offered versions no higher than the manifest allows; while the plugin runs, no host call is refused for an undeclared capability, permission or role (`PLUGIN_CAPABILITY_NOT_OFFERED`, `PLUGIN_HOST_NOT_PERMITTED`, `PLUGIN_ROLE_REQUIRED`), and no table it did not negotiate is pushed |
| `health_answers_within_the_timeout` | health is answered within 3 s |
| `shutdown_exits_within_the_deadline` | after `shutdown` the process exits with status 0 within 10 s, before the server would send SIGTERM |
| `unchanged_push_is_skipped` | restarted against a server that holds its tables with the same digests, the plugin pushes none of them again |
| `changed_push_is_applied` | restarted against a server that holds different tables, the plugin pushes each above the held generation and the push is applied |
| `on_demand_package_does_not_push` | an `on_demand` package pushes no table |
| `routes_serve_a_valid_principal` | each declared route, sent a request signed as the server signs it, does not answer 403 for the principal |
| `routes_refuse_a_missing_principal` | a request without a principal gets 403 |
| `routes_refuse_a_wrong_mac` | a request whose principal was signed under another key gets 403 |
| `routes_refuse_another_routes_action` | a request whose principal's `route_action` is another route's gets 403 (an action no route declares when every route shares one action) |

The two push rules first run the binary against a server that holds nothing and record each table applied within `PushWait` (2 s by default), then restart it twice from that; when the plugin pushed nothing in that time, or the package is `on_demand`, both rules are skipped. The route rules are skipped when no route is declared, and a `resident` package skips the `on_demand` rule. Route requests fill every path parameter with `conformance` and send `{}` for methods with a body; a public route gets a `public` principal, every other one a signed-in platform administrator, with the route's feature signed as `full`. The three time limits default to the server's and can be changed in `Config`. The in-repo fake plugin (`internal/testenv/extplugin/fakeplugin`) runs the suite in CI, which proves that a correct binary passes every rule; its fault switches `--skip-principal-check`, `--ignore-route-action`, `--hang health` and an extra `--requires` each fail only the matching rules.

### Pushed state

The core's part of the first five pushed tables (`availability`, `fleet_capacity`, `node_holds`, `banners`, `attestations`) is in `internal/server/pushstate`; through the plugin host module above it takes plugins' pushes, is loaded at boot and is registered as the hooks:

- **Intake**: `State.Push` takes the pusher's identity as the supervisor binds it to the connection (package id, publisher, certificate grants, approved permissions, the manifest's codes, process generation) and one push. Each `availability` row is accepted only from the key's authority (for `gate: authority`, the `authority_id` the declaring publisher's certificate names; for `gate: self`, the declaring package), and a bare key only from a package of the project publisher; `fleet_capacity` needs the certificate's grant of that privileged table, `attestations` needs the pusher to be the authority of its own namespace, the rest need the approved `push:<kind>`; a notice's code must be listed in the package's manifest. A generation not above the one held or about to commit is refused (`PLUGIN_STATE_STALE`); a push over the connection of a replaced process is refused too (`PLUGIN_STATE_CONNECTION_REPLACED`).
- **Writes**: pending pushes are coalesced per owner and kind (the later replaces the earlier, which is answered `superseded_by`); each tick (at most 1 Hz) writes every owner's changes in one writer transaction, then swaps one snapshot pointer; when the write fails nothing applies (`PLUGIN_STATE_NOT_SAVED`). Persistence uses the typed `pstate_*` tables a core migration creates: a timeline is rows of steps, a notice is a code column plus typed parameter rows, and no column holds JSON; display-only `display` data is not kept. Writes are diffs by entry (feature key, node id, banner id, attestation name).
- **Boot**: every pushed table is read at boot, before any plugin starts; a version marker (`pstate.<kind>`) newer than this build refuses the start with `PLUGIN_STATE_TOO_NEW`, a stored restriction that does not decode refuses it rather than loosen it, and a stored grant that does not decode is left out (absent).
- **Enforcement**: the feature source, the node insert guard, the scheduling holder (source `plugins`) and the banners are each one atomic load plus map lookups, with no allocation. A key no package answers for is `locked` with `FEATURE_PROVIDER_ABSENT`. All four are methods of the interface `pushstate.Evaluator`, and `app.NewPushedStateHooks` adapts them to the `Host` hook types through that interface only; `TestHooksDoNotAllocate` measures the allocations of each method, and `TestEveryEvaluatorIsMeasured` reads every method of the interface by reflection and checks that each has a case, so a new hot-path answer cannot go unmeasured.
- **Time**: timelines fire at their instants on extclock (`max(wall clock, high-water mark)`), with the plugin dead too; the high-water mark is saved every minute and at shutdown. `State.CorrectClock` moves it by at most 366 days per correction, reports it for the audit and shows an administrator banner; steps are not consumed, so a correction backwards un-fires the steps after it. A plugin holding the root-granted `clock_authority` role takes the same path over the host API, `POST /host/v1/clock/correct` (see [Plugin Protocol](../reference/plugin-protocol.md#宿主接口的时钟就绪自有状态与派生密钥)), and `GET /host/v1/meta` reads the wall clock, extclock and the stored high-water mark.
- **Down**: `State.OwnerDown` records a persisted down start (a restart never renews it); grants clamp to `min(current, down_access)` with `PLUGIN_UNAVAILABLE` after the grace (1 hour by default, 60 seconds to 24 hours as the plugin pushes, only lowered by the site through `plugins.policy.max_down_grace` in `server.yaml`, see [Configuration Reference](../reference/configuration.md#插件的信任与站点策略)) plus a 2-minute boot extension (added to every down period, a plugin that crashes mid-run included, so a boot and a crash need not be told apart; counted from the persisted down start, so neither a server nor a plugin restart adds another; once a process has called `POST /host/v1/state/ready`, `State.Ready` drops the extension from its later down periods, a mark that is not persisted and never moves the down start); restrictions never lapse. A package that is installed but no longer verifies keeps its restrictions and banners and loses only its grants.
- **Offline maintenance**: `gpuworks plugin clock reset` and `gpuworks plugin pstate clear`, run by root with the server stopped, only append entries to `<data>/plugins/maintenance.jsonl` (`pluginbridge.MaintenanceEntry`); at the next start `pluginbridge` applies them in order after `Load` and before starting any plugin: a clock correction through `State.CorrectClock` (the same bound, audit and banner; plugins read the new high-water mark through `GET /host/v1/meta`), a clear through `State.ClearTable` (one commit deletes one restriction table of the owner and its overrides and asks for one pass of the scheduling holds; grants cannot be cleared). Each applied entry writes an `audit=true` log line; an entry that fails and the ones after it stay for the next start, with the banner `PLUGIN_MAINTENANCE_FAILED` (see [CLI Reference](../reference/cli.md#维护日志)).
- **Overrides and uninstall**: `State.Override` only loosens one restriction entry (a node hold, or one fleet cap), is persisted and bound to the owner's generation, and an older override of the same entry is deleted in the same commit; it is superseded, reported and shown in an administrator banner for 24 hours when the owner pushes that entry again with a change. `State.RemoveOverride` withdraws one override in one commit and asks for one pass of the scheduling holds. `State.Uninstall` deletes every row of the owner in one commit and asks for one pass of the scheduling holds.
- **Capacity selection**: `fleet_capacity` gives caps and an optional `keep` list of nodes to keep first (unioned across owners); the core selects the nodes, kept nodes first and the rest in enrollment order (`internal/server/fleetcap`: once a cap is full, every node after it is held), and selects again when `keep` changes. It reads the node list once after coalesced node fleet topic changes, at least 2 seconds apart, so nodes that join while the plugin is down are placed by the same rule. A node not placed yet is held only if counting it would exceed a cap. Whenever the caps change (their owner uninstalled included), the selection asks for a pass of the holds, so such a node is released too.

## What the open-source code does not do

- It interprets no feature key and never branches on the edition name; core code names no feature.
- It calls no out-of-process extension on any request, insert, status report, claim or placement path, and runs no extension code such as WASM inside the writer transaction.
- It never hands extensions the Store or a raw database handle.
- It has no `init()` registration and no new package globals: everything comes in through `cli.Options` and `app.Config`.
- It never interrupts running workloads: the gate acts only on new requests and writes, and holders cordon without draining.

<!-- translation-source-sha256: 600e483e0b83247381c3c8f35d4c3a9cd2add820dab7afdc49b2c72a818f3699 # Extension Points: The Module Contract and Installable Extensions -->
<!-- translation-source-sha256: 6e2c8b3b69d01b86bf00ccf832608a3f9f438723c909f6c6ff0f4fb9bf33a032 ## Tiers -->
<!-- translation-source-sha256: c038a66ecb98cf6640ab0f130c8fdecc416cee3f7838156205d8e11c42acf375 ## How a build composes -->
<!-- translation-source-sha256: 954dc4f2be636870d83f560ec4496a7298ac54de1ac0d8eb66e666257694f02c ## Modules and the Host -->
<!-- translation-source-sha256: 17af93631f553ac2ba0f5326de9162d3e284e453aaf99937205830d33cb8318e ## Reapplying holds and the node fleet -->
<!-- translation-source-sha256: f918d39747723d26b216444e3c7d418882aca42ea66fb74fbb6167f3f724eca6 ## Card holders -->
<!-- translation-source-sha256: 2943f81690cda9ea5bf5f35a0039d7d1e8340998af623c72f252ab2b4f93de83 ## Node identity and node inventory -->
<!-- translation-source-sha256: 9ecb281ae30f5bf0846d54c4585c8451f859dab2bb0912982eb541919e54d968 ### The management server's host identity -->
<!-- translation-source-sha256: 4ae6c783942232690aaca56e9e4a5788c45162e113ef31e26f3d30165bb45d96 ## Tables and migrations -->
<!-- translation-source-sha256: 191416e1552e2dd08bfb393247073d478d665d64f255c1d4249e957211f051c2 ## Hooks and call threads -->
<!-- translation-source-sha256: be530f73e0c666ae072bec0b9e53f42c2dbf56c02ee693e60fe50bfe16d85a0f ## Purity rules -->
<!-- translation-source-sha256: 4618dce3be58d3f7e61c7af168ab924406d537b428f5ccaaaaab5c37400e81bc ## Availability -->
<!-- translation-source-sha256: 61939bb6673b4f3b746230492f088bdfb2c598290bf7e4cde67f51eb63faeaec ### Availability of the whole server -->
<!-- translation-source-sha256: 2668224de1381f7abfff8f6a8f7e79d9a0ba9de9f06163994fc1054e81dda1a0 ## Console overlay -->
<!-- translation-source-sha256: e9fcf7fac20aca4c21f4dfcc449146cf5207289490a5ff3489950b61e20a7fac ## Codes and texts -->
<!-- translation-source-sha256: 6fa8885393d900cd0bd114408d5214c5a1e5d017fd8793abcb7c51e96cdf5e7c ## Upgrades and installation -->
<!-- translation-source-sha256: 2f477a9aaaaec4ed58b18dac635e88c1238e37faa62845c0718220b02d485332 ## Installable extensions -->
<!-- translation-source-sha256: 5d53140ea1b81ddd5aa82bf77688c09e587d5ce5e3f061758fbd16b7a4e158cd ### The plugin host module -->
<!-- translation-source-sha256: f661b22d129a49d5c35e13fd3d2c39f0e0780184211c3d25b1fcfdec1a76912c ### Management API -->
<!-- translation-source-sha256: ee1585e74c9cef0b04f22119405aae3edd9a2fa237b17373f4924915a347e4ed ### Local administrator floor -->
<!-- translation-source-sha256: 11a2a79af68dee78e6260046e43fb148f11bb660e4b545aa1331fd11dafe52f8 ### Not built yet -->
<!-- translation-source-sha256: 363fc87d5e625d6ac6f187464547bf40922265cd3965db0d0008e7ba40cc8b0b ### Trampoline: `gpuworks plugin-exec` -->
<!-- translation-source-sha256: 798dbe0813cad6921845730325e9ed3b7823a4c799e890df606828eb7d1d7b8c ### Supervision -->
<!-- translation-source-sha256: c9070a14f05dd8d51d1cb641518d156440d04fb476df691531cbd8e070ced948 ### Health: banners, the diagnostics check and metrics -->
<!-- translation-source-sha256: 58ed25f22c31370bdecb8fb867663dc480e9591da9543ed7940805a7a71b6574 ### Calls and the route table -->
<!-- translation-source-sha256: 0f233829a95996c53912e15001b159b3f1da7a84878fdbfdd06a0298c40096bc ### Host setup -->
<!-- translation-source-sha256: 29247820fde4c506e37bba67d73af32aab517446768b216ff0de25f1b96d4d0f ### Plugin routes -->
<!-- translation-source-sha256: 14fa2e1c9fb41cb50efb0b831f714c10a449381862dbcb9478707c85e83d3919 ### gwx/1 connections and the host API -->
<!-- translation-source-sha256: aaf8f3059021b513660e26edfea7ef83fdc92d07e7b4972eacfb805de2622b1c ### Plugin SDK: `pkg/pluginsdk` -->
<!-- translation-source-sha256: 023cdd527089a4ca4ae0c9d9cb96a646ca41eaf425c51a7e34899709efb9e3bb ### Conformance suite: `pkg/pluginsdk/conformance` -->
<!-- translation-source-sha256: f929facbfa47fb6dc7de5c2e929e913eeabbfc8cc6121f8c93a8f1beeddd49d1 ### Pushed state -->
<!-- translation-source-sha256: 3a9a24030110d056398b971f646b463c808967a03ef33c0b91a8c948e97d89ed ## What the open-source code does not do -->
