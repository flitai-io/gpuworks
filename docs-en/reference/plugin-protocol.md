# Plugin Protocol and Wire Formats (gwx/1)

The wire formats of plugin package manifests, publisher certificates, trust updates, revocation lists, package indexes, signature envelopes, the `gwx/1` handshake, health and error messages, and the tables plugins push are defined in a separate Go module, `pkg/extapi` (module path `gpuworks.ai/gpuworks/pkg/extapi`). It uses only the standard library, so a plugin depends on it and not on the server.

These formats are frozen, and this page describes each of them. The server verifies, installs and removes plugin packages and imports trust updates and revocation lists as this page describes (the `gpuworks plugin` subcommands, see [CLI Reference](cli.md)); the plugin host starts and supervises plugin processes and takes and enforces pushed tables, including the merge, grace and timeline semantics this page mentions. What the plugin host does today and what is not built yet is in [Extension Points](../dev/extensions.md#installable-extensions); the design and its trade-offs are in [ADR 0071](../adr/0071-installable-extensions.md).

The golden corpus `pkg/extapi/testdata/golden/` is the authority on the formats. Every JSON example on this page is byte-for-byte identical to a file in the corpus, named in a comment above the example in the page source, and a test in `pkg/extapi` (`TestReferencePageExamplesAreGoldenFiles`) compares each one. That is why the examples are canonical JSON (one line, sorted keys).

## General rules

- **Strict decoding.** A reader decodes an object only at a version it knows and refuses any field that version does not define (`PLUGIN_WIRE_UNKNOWN_FIELD`). Keys are case-sensitive; duplicate keys, data after the JSON value, values of the wrong type, invalid UTF-8 and an escaped UTF-16 surrogate that is not half of a pair (a lone `\ud800`) are refused (`PLUGIN_WIRE_MALFORMED`).
- **`display` is the only lenient part.** Manifests, certificates, revocation entries, availability rows and banners may carry a `display` object: any JSON object of at most 8 KiB encoded, kept as received so a relay loses nothing. It is for display only, never changes what the core enforces, and an older reader may drop it.
- **Numbers, times and bytes.** Every number is an integer of magnitude at most 2^53−1; every time is an RFC 3339 UTC timestamp with whole seconds (`2026-01-02T03:04:05Z`), and any other spelling of the same instant is refused; every byte string is standard padded base64.
- **Sorted sets.** Lists that are sets (roles, permissions, codes, files, root key ids, namespaces and so on) must be sorted by bytes without repeats, so a set has exactly one encoding.
- **Codes and parameters.** A `code` is an UPPER_SNAKE code of [ADR 0067](../adr/0067-runtime-outcomes-as-codes.md); `params` has at most 16 entries, with lower_snake keys and values that are strings of at most 512 bytes, integers or booleans.

### Versions and compatibility

| Object | Where its version comes from | Current version | When a reader meets a newer one |
|---|---|---|---|
| Package manifest | `schema` | 1 | `PLUGIN_MANIFEST_TOO_NEW`, with the manifest's `min_core_hint` among its parameters |
| Publisher certificate, trust update, revocation list | `format` | 1 | `PLUGIN_SCHEMA_TOO_NEW` |
| Envelope, handshake, health, error, push and acknowledgement | Protocol major | 1 | `PLUGIN_PROTOCOL_UNSUPPORTED` at the handshake |
| Rows of a pushed table | The push's `schema`, the negotiated capability version `table.<kind>@<n>` | 1 | `PLUGIN_SCHEMA_TOO_NEW` |

- Protocol major 1 is frozen: the shapes of the envelope, handshake, error and push envelope never change.
- Capabilities (`routes`, `table.availability` and so on) carry their own versions; a capability version released within major 1 is never removed.
- A field that changes what the core enforces can only arrive as a new version of its object (in Go, a new type beside the old one), so an older reader refuses it instead of silently dropping a restriction.
- A released golden file is never edited or removed (see [Golden files](#golden-files)).

## Canonical JSON and digests

Signed payloads and pushed-state digests use one byte form, so a signature or a digest names exactly one document. Canonical JSON is UTF-8 JSON with:

- no insignificant whitespace;
- object members sorted by the UTF-8 bytes of their keys, with no duplicate keys;
- integers only, in decimal, without a plus sign, leading zeros, a fraction or an exponent, and zero without a minus sign;
- strings escaped only where JSON requires it: `\"` and `\\`; the controls U+0008, U+0009, U+000A, U+000C and U+000D as `\b`, `\t`, `\n`, `\f` and `\r`; every other control below U+0020 as `\u00xx` in lowercase hex; every other character as its UTF-8 bytes.

A digest is written `sha256:<lowercase hex>`, the SHA-256 of the canonical bytes.

## Signature envelope

Signed documents travel as envelopes: the payload bytes and the signatures over them sit in one file, so a reader never pairs a document with a signature of another version of it. `manifest.sig`, each certificate in `publisher.crt`, and trust updates are envelopes. The shape is the same as the server's release channel envelope.

<!-- golden: envelope/v1/valid/two_signatures.json -->
```json
{"payload":"eyJjcmVhdGVkIjoiMjAyNi0xMS0xN1QwMDowMDowMFoiLCJmaWxlcyI6W3siZXhlYyI6dHJ1ZSwicGF0aCI6ImJpbi9saW51eF9hbWQ2NC9hY21lLWJhY2t1cCIsInNoYTI1NiI6IjUxYTFmMDVhZjg1ZTM0MmUzYzg0OWI0N2QzODcwODY0NzYyODJkNWY1MGRjMjQwYzE5MjE2ZDZlZGZiMWViNWEiLCJzaXplIjo0MDk2fV0sImlkIjoiYWNtZV9iYWNrdXAiLCJwcm90b2NvbCI6eyJtYXgiOjEsIm1pbiI6MX0sInB1Ymxpc2hlciI6ImFjbWUiLCJzY2hlbWEiOjEsInZlcnNpb24iOiIxLjAuMCJ9","signatures":[{"keyid":"acme-2026a","sig":"EREREREREREREREREREREREREREREREREREREREREREREREREREREREREREREREREREREREREREREREREREREQ=="},{"keyid":"acme-hsm-1","sig":"MEQiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIg=="}]}
```

| Field | Rules |
|---|---|
| `payload` | The signed bytes exactly as signed (base64); must be canonical JSON, at most 4 MiB. An encoded envelope is at most 6 MiB and `publisher.crt` at most 24 MiB; anything longer is refused before parsing (`PLUGIN_WIRE_MALFORMED`, rule `range`) |
| `signatures` | 1–8 `{keyid, sig}` entries; no `keyid` repeats within an envelope; each `sig` is 1–512 bytes |

The signatures in the example are filler bytes that only show the shape.

The algorithm belongs to the trusted key, never to the signature: a reader verifies with the algorithm of the key it trusts under that key id, so a signature cannot choose a weaker one.

| `alg` | Public key | Signature |
|---|---|---|
| `ed25519` | A 32-byte Ed25519 public key | A 64-byte pure Ed25519 signature over the payload (RFC 8032) |
| `ecdsa-p256-sha256` | A P-256 point in uncompressed SEC 1 form (65 bytes, `0x04‖X‖Y`) | An ASN.1 DER signature over the SHA-256 of the payload, the form hardware keys and cloud key services return; it must be minimal DER with nothing after it and `s` ≤ n/2 (low-S), or it does not verify, so a signature has one encoding; `Seal` replaces a high `s` a signer returns with n−s |

A key id matches `^[a-z0-9][a-z0-9._-]{1,63}$` and names one key forever; it is never reused. A project root's key id starts with `gwroot-` (`gwroot-1`, `gwroot-2`), and a certified key's with `<publisher>-` (never with `gwroot-`), and `gwroot` is not a valid publisher name, so a root and a publisher key, or two publishers' keys, never share an id. Verification rules:

- Signatures by key ids the reader does not know are skipped, so a signer can sign with the old and the new key before readers switch to trusting the new one.
- When a signature by a trusted key does not verify, the whole envelope fails (`PLUGIN_SIGNATURE_INVALID`): a key id names one key, so a failure is tampering or corruption, never a rotation.
- At least one trusted signature must verify, or the result is `PLUGIN_SIGNATURE_UNTRUSTED`; a payload that is not canonical JSON gives `PLUGIN_WIRE_NOT_CANONICAL`.

## Package manifest

`manifest.json` is the package's manifest, stored as canonical JSON; its bytes are the payload of `manifest.sig`. `publisher.crt` is a JSON array of 1–4 certificate envelopes, so a publisher rotating keys can ship the old and the new certificate together.

<!-- golden: manifest/v1/valid/minimal.json -->
```json
{"created":"2026-11-17T00:00:00Z","files":[{"exec":true,"path":"bin/linux_amd64/acme-backup","sha256":"51a1f05af85e342e3c849b47d387086476282d5f50dc240c19216d6edfb1eb5a","size":4096}],"id":"acme_backup","protocol":{"max":1,"min":1},"publisher":"acme","schema":1,"version":"1.0.0"}
```

An example that uses every field is `manifest/v1/valid/full.json` in the corpus.

| Field | Rules |
|---|---|
| `schema` | 1 |
| `id` | The package id, `^[a-z][a-z0-9_]{1,31}$`; also the route segment `/api/v1/x/<id>/` and a directory name. It must belong to `publisher`: a third party's ids start with `<publisher>_`, and the project publisher's have no `_` or start with `gpuworks_`, so a project id never equals a third party's; which prefixes it may use is a grant of its certificate |
| `publisher` | `^[a-z][a-z0-9]{1,15}$` (no underscore, so `<publisher>_` splits a package id unambiguously; `gwroot` is reserved for the root keys); must equal the subject of the signing certificate |
| `version` | A semantic version without a leading `v` |
| `min_core_hint` | Optional, at most 32 bytes; names the server release needed, for messages only. Compatibility is decided by `requires` |
| `created` | Must lie inside the signing certificate's signing window |
| `protocol` | The range of `gwx` majors the package speaks, `{min, max}` |
| `requires` | Capabilities the package cannot run without → the lowest version it needs; a server that lacks one judges the package incompatible and names the capability |
| `uses` | Optional capabilities → the highest version the package can use, negotiated down at run time; a capability cannot also appear in `requires` |
| `roles` | Privileged roles the package claims, sorted; a claim counts only if the certificate grants it |
| `permissions` | What the administrator approves, `<verb>:<object>`, sorted; the only verbs are `crypto`, `events`, `http`, `metrics`, `node`, `notify`, `push`, `read` and `svc`, and each verb's objects come from a closed vocabulary, see [Permissions](#permissions) |
| `features` | Declared feature keys `{key, presentation, gate}`: `presentation` is `promote` (shown as locked) or `conceal` (hidden); `gate` is `none` (always full), `self` (this package answers) or `authority` (only the namespace authority answers) |
| `routes` | The package's HTTP routes; see the table below |
| `ui` | The console bundle: `mode` is `module` (a same-origin ES module, which needs the certificate grant) or `iframe`; `entry` is a member under `ui/`; `sdk` is the range of console SDK majors; `nav` are menu entries |
| `node` | The node part: `app` is the application description under `node/`, `images` are image archives for offline sites under `node/images/` |
| `runtime` | The server part: `lifecycle` is `resident` (started with the server) or `on_demand` (started on use, stopped when idle; the server's policy lets it hold no `push:` permission and declare no feature, see [The server's install policy](#the-servers-install-policy)); `mem_mib`, `cpu_milli` and `max_concurrency` (≤1024) take defaults at 0; `devices` are device nodes under `/dev`, sorted |
| `storage` | The data directory: `kind` is `sqlite` or `files`; `ha` is `shipped` (replicated with the server database) or `leader_local` (a rebuildable cache); `state_version` is the data format version this build writes (≥1); `cold_copy_safe` means the directory may be copied while the plugin is stopped |
| `settings` | Values an administrator configures for the package, see [Settings](#settings) |
| `host_setup` | Host steps from a closed vocabulary, never a script; today only `usb_device` (`vendor_id` and `product_id`, four lowercase hex digits each) |
| `codes` | Every code the package can emit, sorted; each has en and zh text in the package's locales |
| `files` | Every member except `manifest.json`, `manifest.sig` and `publisher.crt`, sorted by path: `{path, size, sha256, exec}`; paths are relative, every segment starts with a letter, digit or underscore (no `.`, `..` or hidden files), at most 512 bytes |

When a part of the manifest is present, `requires` must list its capability, so a server without it refuses the package by capability instead of mishandling it:

| Part | Must `require` |
|---|---|
| Non-empty `routes` | `routes` |
| `ui` | `ui.bundle` |
| `node` | `node.app` |
| Non-empty `host_setup` | `host.setup` |
| Non-empty `settings` | `host.config` |

Routes are served at `/api/v1/x/<id><pattern>`:

| Field | Rules |
|---|---|
| `method` | `GET`, `POST`, `PUT`, `PATCH` or `DELETE`; `method` plus `pattern` is unique |
| `pattern` | Starts with `/` and has only literal segments (lowercase letters, digits, `_`, `.` and `-`) and parameters `{name}` without a regular expression; `/` alone is the package's root. Regular-expression parameters (such as `{id:[0-9]+}`), empty segments, `.` and `..` segments, percent-escapes and a parameter named twice are refused; two patterns of one `method` that differ only in parameter names (such as `/jobs/{id}` and `/jobs/{name}`) match the same paths and count as duplicates. So the server and the plugin's own router never match a path differently |
| `action` | `<package id>.<verb>`, the action name an authorizer grants |
| `access` | `admin` (administrators, by session or platform token), `admin_session` (administrator sessions only), `member` (any signed-in user or platform token), `public` (no authentication) |
| `feature` / `ungated` | Exactly one is set; `feature` must be a key this manifest declares |
| `write_when_read_only` | Lets this non-GET write through while the feature is read-only (a write that only removes or relaxes); needs `feature` |
| `max_body` | Request body limit; 0 means 1 MiB, at most 1 GiB; above 1 MiB the package needs the permission `http:uploads` |
| `timeout_s` | The request time limit in seconds: 0 means 60 seconds, at most 600; on a stream route it bounds the wait for the response's first byte |
| `stream` / `websocket` | A streamed response / a WebSocket upgrade (GET only, not together with `stream`); `websocket` also needs `routes.websocket` in `requires` or `uses`. On a server that offers only `routes`, a package that has it in `requires` is incompatible; one that has it only in `uses` installs as usual, but that route is not served |
| `html` | Lets the route answer `text/html`; only `public`, `admin` and `admin_session` routes may set it |
| `ai_readable` | The assistant and MCP may read it; GET only |
| `cookies` / `rate` | `public` routes only: may set cookies (a single sign-on callback) / a token bucket per client address `{per_minute, burst}`, each 1–100000 |

### Permissions

Each permission names the capability that serves it. A manifest that holds a permission must list its capability in `requires` or `uses`: a server that does not offer a required capability refuses the package as incompatible (`PLUGIN_INCOMPATIBLE`); when only an optional one is missing, the package installs but does not get that service. An object outside the vocabulary (a misspelled read surface, an unknown table kind or event topic) makes the manifest invalid (`PLUGIN_WIRE_INVALID` with `rule` `enum`), a missing capability is `relation`, and `field` names the permission in both cases. The risk class comes from the server with [`GET /api/v1/plugins`](api.md); the console only shows it.

| Permission | Capability | Risk |
|---|---|---|
| `read:features` | `host.features` | `low` |
| `read:fleet` | `host.fleet` | `elevated` |
| `read:node_clocks` | `host.node_clocks` | `low` |
| `read:host_identity` | `host.host_identity` | `high` |
| `read:directory` | `host.directory` | `high` |
| `read:usage` | `host.usage` | `elevated` |
| `read:audit` | `host.audit` | `high` |
| `read:attestations` | `host.attestations` | `elevated` |
| `read:node_identity` | `host.node_identity` | `high` |
| `read:node_inventory` | `host.nodes` | `elevated` |
| `push:availability` | `table.availability` | `high` |
| `push:fleet_capacity` | `table.fleet_capacity` | `high` |
| `push:node_holds` | `table.node_holds` | `high` |
| `push:banners` | `table.banners` | `low` |
| `push:attestations` | `table.attestations` | `low` |
| `push:allowances`, `push:insert_caps`, `push:roles.core_actions` | `table.<kind>` | `high` |
| `push:prices` | `table.prices` | `elevated` |
| `events:node_fleet` | `host.fleet` | `elevated` |
| `http:routes` | `routes` | `low` |
| `http:public` | `routes` | `elevated` |
| `http:uploads` | `routes` | `low` |
| `node:workloads`, `node:jobs` | `node.app` | `high` |
| `notify:raise` | `host.notify` | `low` |
| `metrics:expose` | `metrics.expose` | `low` |
| `crypto:derive_key` | `host.derive_key` | `elevated` |

- The object of `push:` is a table kind this module defines, or a kind reserved for privileged tables: `allowances`, `insert_caps`, `prices` and `roles.core_actions` (no table schema yet; pushing them needs a certificate grant). `fleet_capacity` and the reserved kinds all need a certificate grant.
- `events:` has only `node_fleet`: the change feed of the fleet read, so it shares `host.fleet` with `read:fleet`. `svc:` has no object yet. New objects arrive only with new capabilities.
- A permission the server does not know is shown as `high`.

### Settings

`settings` declares the values an administrator configures for the package, at most 64, and needs `host.config`. The server validates a value against its declaration before storing it and hands the stored values to the plugin.

| Field | Rules |
|---|---|
| `key` | lower_snake, unique in the manifest |
| `type` | `string`, `integer`, `boolean` or `enum` |
| `label_key` | The locale key of the label |
| `required` | A configuration must set it |
| `secret` | `string` only: the server stores it sealed, never shows it back and hands it only to this plugin |
| `values` | `enum` only, and required there: the allowed values, lower_snake, sorted, at most 64 |
| `min` / `max` | `integer` only: inclusive bounds, absent for none; `max` is not below `min` |
| `max_length` | `string` only: a limit in bytes; 0 means 4096, at most 65536 |

### The server's install policy

Beyond the manifest, the server applies its own policy after the capability check and before the site policy. It applies on install and on every recheck, so an installed package is refused on recheck as well:

- A package with routes must hold `http:routes`, and one with a `public` route also `http:public`; otherwise `PLUGIN_POLICY_REFUSED`, with the missing permission as `rule` and the route (such as `routes.0`) as `value`.
- An `on_demand` package is not running most of the time, so it may hold no `push:` permission and declare no feature (no one would answer for its keys while it is stopped); otherwise `PLUGIN_STATE_ON_DEMAND` with `id` and `permission` or `feature`.
- Only Linux can apply host steps: on any other platform a package that declares `host_setup` gets `PLUGIN_HOST_SETUP_UNSUPPORTED` with `id`.

### Host setup (`host.setup@1`)

The server always offers `host.setup@1`, but only Linux can apply it; elsewhere install is refused with `PLUGIN_HOST_SETUP_UNSUPPORTED`. The manifest's `host_setup` is a declaration, never a script; root renders it by running `gpuworks plugin host-setup` (or `gpuworks install` or `upgrade`) under these rules. What a `usb_device` step means:

| Product | Rule |
|---|---|
| udev rules | One file per package; two lines per step, matching `vendor_id` and `product_id`, one hitting the device's `usb_device` node (libusb's `/dev/bus/usb/<bus>/<device>`) and one its hidraw nodes, setting only `GROUP="gpuworks-usb"` and `MODE="0660"`; no `SYMLINK`, `RUN` or `OWNER` |
| Applied record | A root-owned file holding the digest of the steps: the SHA-256 of the canonical JSON `{"host_setup":[{"type","vendor_id","product_id"}...]}`, the list sorted by type, vendor and product with duplicates removed, so the order of the steps does not change it; an upgrade that keeps the steps needs no new root step |
| Sandbox | When the record matches the current steps, the plugin's Landlock rules also grant read, write and ioctl on device nodes under `/dev` (and listing it), and read-only `/sys/bus/usb`, `/sys/class/hidraw`, `/sys/devices` and `/run/udev/data` for device discovery |

When the record is missing, differs, or could have been written by the service account (not owned by root, or writable by group or others), the plugin does not start: its state is `needs_host_setup` with reason `PLUGIN_HOST_SETUP_REQUIRED`, params `id` and `command` (what to run). The boundary between devices is the nodes' file permissions: the sandbox grants a directory, because a device's nodes appear only once it is plugged in, under names nobody knows in advance. So the plugin can open every node under `/dev` the service account can already open, not only the matched USB devices.

## Publisher certificate

A certificate is a publisher's signing key and its grants. It travels as the payload of an envelope signed by a project root key (or, for a publisher the host operator trusts, as the operator configures). The issuer is the key id whose signature verified; the payload has no issuer field. A certificate for the project publisher, or one granting the bare namespace or `ui_module`, needs a project root's signature to verify; signed only by issuers the host operator configures, it fails with `PLUGIN_SIGNATURE_UNTRUSTED`, whose `field` parameter names the grant.

<!-- golden: publisher_cert/v1/valid/authority.json -->
```json
{"alg":"ed25519","format":1,"grants":{"authority_id":"acme_quota","feature_ns":["acme"],"id_prefixes":["acme_"],"min_isolation":"landlock","roles":["availability_authority","clock_authority"],"tables":["fleet_capacity"]},"keyid":"acme-2026a","not_after":"2027-02-01T00:00:00Z","not_before":"2026-11-17T00:00:00Z","public_key":"1+5QRW1qr6d5EuAxQt77BRh28RH6+gBNDvAKPKdkHV4=","publisher":"acme","serial":17}
```

| Field | Rules |
|---|---|
| `format` | 1 |
| `serial` | Unique among the certificates one trust anchor issues to the publisher, whichever of the anchor's keys signed them. The project roots are one anchor; each issuer the host operator configures is another and picks its publisher names and serials itself, so anchor + `publisher` + `serial` names one certificate; ≥1 |
| `publisher` | The subject; manifests signed with this key must name it |
| `keyid` / `alg` / `public_key` | The publisher's signing key, with the rules of the [signature envelope](#signature-envelope); `keyid` starts with `<publisher>-` and not with `gwroot-` |
| `not_before` / `not_after` | The signing window; `not_after` must be later than `not_before`. A package whose `created` lies in the window stays valid after `not_after`, so installed packages never expire |
| `grants.id_prefixes` | Required: the package ids the key may sign, sorted, each with the package id grammar. An entry ending in `_` covers every id that starts with it; any other entry covers exactly that id. Every entry belongs to the subject, by the rule for a manifest's `id` (a third party's start with `<publisher>_`; the project publisher's have no `_` or start with `gpuworks_`), so a certificate never covers another publisher's ids (`acme_` does not cover `acmebank_pay`, and a project certificate cannot list `acme_`) |
| `grants.feature_ns` | Namespaces its packages may declare feature keys in, sorted: only the publisher's own name, plus `""` (the bare namespace) for the project publisher |
| `grants.authority_id` | The one package id that may answer `gate: authority` keys of `feature_ns`; needs `feature_ns` and must be covered by `id_prefixes` |
| `grants.roles` / `grants.tables` | Privileged roles its packages may claim / privileged tables they may push, sorted |
| `grants.ui_module` | Its packages may ship same-origin console modules |
| `grants.min_isolation` | Required: `landlock` (runs only where Landlock confines it) or `none` |

The project publisher is named `gpuworks`; its own packages use bare feature keys and ids without a publisher prefix:

<!-- golden: publisher_cert/v1/valid/project.json -->
```json
{"alg":"ed25519","format":1,"grants":{"authority_id":"quota","feature_ns":["","gpuworks"],"id_prefixes":["quota","sso"],"min_isolation":"landlock","roles":["availability_authority","clock_authority"],"tables":["fleet_capacity"]},"keyid":"gpuworks-2026a","not_after":"2027-02-01T00:00:00Z","not_before":"2026-11-17T00:00:00Z","public_key":"0RZF/KSeB5I5emmRFX7vHLklBOUA2A8ot0mLncySqxs=","publisher":"gpuworks","serial":1}
```

## Trust updates and revocation lists

A trust update is signed by at least one project root key the reader trusts now, and can be imported offline. A reader keeps the highest `serial` it has applied and refuses a lower or equal one, so an old update cannot be replayed.

<!-- golden: trust_update/v1/valid/rotate.json -->
```json
{"add_roots":[{"alg":"ed25519","keyid":"gwroot-3","public_key":"eC0hm7/hzMkrq452w7FbsvdumRZzFEroSPiJYIFGCqs="},{"alg":"ecdsa-p256-sha256","keyid":"gwroot-4","public_key":"BMX+c4VTtfKGKA3l0EjO3d6IXwcBTVbSabaijGatYQCn+OSfRFHDO2UzUlWZu07cpYIEmdreHZH0eYnGPrSojuY="}],"created":"2026-12-01T00:00:00Z","floors":[{"id":"acme_backup","min_version":"1.1.0"},{"id":"acme_quota","min_version":"2.0.3"}],"format":1,"retire_roots":["gwroot-1"],"revoke":[{"keyid":"acme-2025a","publisher":"acme"},{"cert_serial":9,"publisher":"acme"}],"serial":3}
```

| Field | Rules |
|---|---|
| `format` / `serial` / `created` | 1 / an increasing serial / when it was issued |
| `add_roots` | Root public keys to trust from now on, sorted by key id, each key id starting with `gwroot-`; none may also be in `retire_roots` |
| `retire_roots` | Root key ids to stop trusting, sorted, each starting with `gwroot-` |
| `revoke` | Revocation entries, see below; at most 4096 |
| `floors` | Raise the lowest installable version of packages, `{id, min_version}`, sorted by id; a floor only rises |

At least one of the four lists is non-empty. Every revocation names its `publisher`, which scopes it, and at most one of `cert_serial`, `keyid` and `manifest` narrows it; an entry does not repeat within a list:

| Selector | Revokes |
|---|---|
| `publisher` | Every certificate of the publisher |
| `publisher` + `cert_serial` | One certificate |
| `publisher` + `keyid` | One of the publisher's keys, in every certificate that certifies it; the key id starts with `<publisher>-` |
| `publisher` + `manifest` | One package build of the publisher, by manifest digest, applied only to a package whose manifest names that publisher; only this kind of entry may carry `replacement` (the version to install instead) |

Revoking by manifest digest rather than by package file hash means re-compressing the tar does not escape a revocation.

Publisher names, key ids and serials are assigned by the anchor that certifies the publisher, so an entry by `publisher`, `cert_serial` or `keyid` applies only to certificates of the same anchor as the list carrying it: a root-signed trust update or revocation list (no `publisher`) to certificates the project roots issued; a publisher's own list to that publisher's certificates under the anchor that certified the key that signed it. The host operator withdraws what its own issuers certified in its configuration. An entry by `manifest` names one build by its content, so it applies under every anchor.

Revocation lists are cumulative: a reader keeps the highest `serial` per issuer. A list issued under a project root has no `publisher`; a publisher may issue one about its own certificates, keys and builds, naming itself, and then every entry must name that same publisher, so one publisher's list never revokes another's keys or builds. `revoked` must not be empty.

<!-- golden: crl/v1/valid/root.json -->
```json
{"created":"2026-12-01T00:00:00Z","format":1,"revoked":[{"cert_serial":9,"publisher":"acme"},{"keyid":"acme-2025a","publisher":"acme"}],"serial":12}
```

## Package index

A package index (`plugins/index.json`) lists the package builds of one publisher that a server may install. It travels as the payload of an envelope signed by that publisher's certified key, next to the publisher's `publisher.crt`, and imports offline. Each build states the capabilities it needs, so a server decides which version it can run before downloading anything: for each package it offers the newest version whose `requires` it satisfies, and lists newer versions it cannot run with `PLUGIN_NEEDS_NEWER_CORE` (parameter `min_core_hint`). A reader keeps the highest `serial` it applied per publisher and refuses a lower or equal one.

<!-- golden: index/v1/valid/minimal.json -->
```json
{"created":"2026-12-01T00:00:00Z","format":1,"packages":[{"files":[{"path":"acme_backup_1.0.0.gwpkg","sha256":"63a5bce3a60970c25511673e2619ae3b92035a85548c97ff5b479598e3d18de3","size":8203411}],"id":"acme_backup","manifest":"sha256:b7fcf4401d9ce4bf107ece16db8c71123ef334473ab62dbced6283cd0f4a002f","protocol":{"max":1,"min":1},"version":"1.0.0"}],"publisher":"acme","serial":1}
```

The example using every field is `index/v1/valid/catalog.json` in the corpus.

| Field | Rule |
|---|---|
| `format` / `serial` / `created` | 1 / an increasing serial / when it was published |
| `publisher` | The publisher name; the certificate of the signing key must be this publisher's |
| `packages` | At most 4096 builds, sorted by `id` and, within one `id`, by version precedence; each build once |
| `packages[].id` / `version` | The package id (inside `publisher`'s names, as for a manifest) / a semantic version |
| `packages[].manifest` | The digest of the build's `manifest.json`; the downloaded package must carry exactly that manifest |
| `packages[].min_core_hint` / `protocol` / `requires` | The same as in the manifest, for the verdict before download |
| `packages[].files` | 1–16 downloads `{platform, path, size, sha256}`, sorted by `platform` without repeats: `platform` is `<os>_<arch>`, or absent for a full package holding every platform; `path` is relative to the index's location and ends in `.gwpkg`; `size` ≥1 |
| `packages[].display` | Display only |
| `revoked` | Optional revocation entries, each naming the publisher, applied like the publisher's own revocation list |
| `floors` | Optional; raise only the lowest version of the publisher's own packages, sorted by id |

## gwx/1 transport and handshake

Under ADR 0071, a plugin is a child process of the server and runs plaintext HTTP/2 over each of two inherited socketpairs: on one the plugin serves the server, on the other it calls the host API. These constants are frozen with major 1 in `pkg/extapi`:

| Name | Value | Purpose |
|---|---|---|
| Environment variable `GWX_PROTOCOL` | The major | The protocol major in the plugin's environment |
| Environment variables `GWX_SERVE_FD` / `GWX_HOST_FD` | Descriptor numbers | Where the plugin serves / calls the host API |
| `POST /gwx/v1/initialize` | Handshake | The server's first call to the plugin |
| `/gwx/v1/shutdown` | Stop | Asks the plugin to exit |
| `GET /gwx/v1/health` | Health | The server's periodic probe |
| `PUT /host/v1/state/{kind}` | Push | The plugin pushes the whole table of one kind |
| `GET /host/v1/health` | Host health | The plugin probes the host API; the answer is a health object |
| `POST /host/v1/node-identity` | Node identity | The plugin reads the salted hardware identity of named nodes; see [Node reads](#node-reads) |
| `GET /host/v1/nodes` | Node inventory | The plugin pages through the node inventory in enrollment order |
| `GET /host/v1/state/{kind}` | Own state | The plugin reads back one table the server holds for it (see [Host API clock, readiness, own state and derived keys](#host-api-clock-readiness-own-state-and-derived-keys)) |
| `POST /host/v1/state/ready` | Ready | The plugin reports that it has finished starting |
| `GET /host/v1/meta` | Meta | The server version, boot id and clocks |
| `POST /host/v1/clock/correct` | Clock correction | A package holding `clock_authority` corrects extclock |
| `POST /host/v1/derive-key` | Derived key | A key bound to the package and a label |
| `POST /host/v1/host-identity` | Host identity | The plugin reads the salted identity of the management server's machine; see [Host identity](#host-identity) |
| Request header `Gwx-Deadline-Ms` | Unix milliseconds | The call's deadline |
| Response header `Gwx-Product-Error` | — | Marks a plugin route's response body as an error object |
| `/r/<path>` | Route | A request to one of the plugin's routes ([route requests](#route-requests-and-the-principal)) |
| Request headers `Gwx-Principal`, `Gwx-Principal-Mac`, `Gwx-Feature-Access` | — | Who calls a route, and the access of its features |

The handshake request:

<!-- golden: handshake_request/v1/valid/first_start.json -->
```json
{"clock":{"extclock":"2026-12-01T00:00:00Z","wall":"2026-12-01T00:00:00Z"},"core":{"arch":"amd64","ha_role":"leader","install_id":"inst_7f3a9c2e41b8","offer":{"host.events":{"max":1,"min":1},"host.fleet":{"max":1,"min":1},"routes":{"max":1,"min":1},"table.attestations":{"max":1,"min":1},"table.availability":{"max":1,"min":1},"table.banners":{"max":1,"min":1},"table.fleet_capacity":{"max":1,"min":1},"table.node_holds":{"max":1,"min":1}},"os":"linux","version":"v26.12.0"},"plugin":{"id":"acme_quota","permissions_granted":["http:routes","push:banners","read:fleet"],"version":"2.0.4"},"principal_key":"AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=","protocol":{"max":1,"min":1}}
```

| Field | Rules |
|---|---|
| `protocol` | The range of majors the server speaks; without 1 the result is `PLUGIN_PROTOCOL_UNSUPPORTED` |
| `core` | `version`, `install_id`, `os`, `arch`, `ha_role` (`leader` or `single`; plugins run only where the server leads), and `offer`: every capability the server offers → its version range, never empty |
| `plugin` | What the administrator approved: `id`, `version`, `permissions_granted`, `roles_granted`, `authority_namespaces` (namespaces this package is the authority for, `""` being the bare namespace); every list is sorted |
| `held` | Each table the server holds for this package, `{kind, schema, generation, digest}`, with unique kinds; the plugin may skip a push whose digest matches |
| `clock` | The `wall` clock and `extclock` (the clock timelines follow, which never runs backwards) |
| `principal_key` | A fresh 32-byte key per spawn, which authenticates the principal headers on proxied routes |

The plugin's answer:

<!-- golden: handshake_response/v1/valid/initialized.json -->
```json
{"id":"acme_quota","manifest_sha256":"sha256:69a8fc5cf9fa0174e3dfe74b1c9c1f5f4f64710afa362419883f19c6b42ee422","protocol":1,"services":["backup","support"],"use":{"host.fleet":1,"table.attestations":1,"table.availability":1,"table.banners":1,"table.fleet_capacity":1},"version":"2.0.4"}
```

`protocol` is the major the plugin chose from the server's range (only 1 today); `manifest_sha256` is the digest of the manifest the plugin was built with, by which the server refuses a binary that does not belong to its package; `use` maps each capability the plugin will use to the version it chose, which must lie within the server's `offer`; `services` are the optional services the plugin serves, sorted.

## Route requests and the principal

A plugin's routes live at `/api/v1/x/<id><pattern>`. Once the server has authenticated, matched and checked a request, it forwards it to `/r<path>` on the serve connection (for example `/api/v1/x/acme_backup/jobs` to `/r/jobs`). Before forwarding it strips any `Gwx-*` header the client sent, then sets three headers that tell the plugin who is calling and which route the server matched:

| Header | Content |
|---|---|
| `Gwx-Principal` | The principal: canonical JSON, base64url-encoded without padding |
| `Gwx-Principal-Mac` | `base64url(HMAC-SHA256(principal_key, <Gwx-Principal value> + "\n" + <Gwx-Feature-Access value>))`, without padding; the feature access is signed together with the principal |
| `Gwx-Feature-Access` | The current access level of each feature the matched route declares; an ungated route sends an empty value |

`principal_key` is the key the handshake request generates fresh for each spawn, so after a plugin restarts, headers signed for the previous spawn no longer verify; the server signs for the process that takes the call, including an `on_demand` plugin that this call starts.

The server offers `routes@1`, not `routes.websocket`. A forwarded request: its path is the same canonical string the server matched (a request with a `.` or `..` segment, an empty segment, a backslash, or an escaped `/`, backslash, `.` or `%` is refused with 400 before matching; other escapes are kept as sent), with the original method, query and body; the client's `Gwx-*`, `X-Gpuworks-*`, `Authorization`, hop-by-hop headers and `Cookie` are removed (a `cookies` route keeps only cookies whose names are not the core's and do not start with `__Host-`), and `traceparent` is set, with `Gwx-Deadline-Ms` on a route that does not stream. The plugin's response:

- Only a route that declares `html` may answer `text/html` or `application/xhtml+xml`; otherwise the server answers 502 `PLUGIN_ROUTE_RESPONSE_INVALID`. Every response gets `Content-Security-Policy: sandbox; default-src 'none'` and `X-Content-Type-Options: nosniff`.
- Only a `public` route that declares `cookies` may set cookies: the path is forced to `/api/v1/x/<id>/`, and one named like a core cookie or starting with `__Host-` or `__Secure-` is dropped.
- A body marked `Gwx-Product-Error` must be a gwx/1 error object under a 4xx or 5xx status; the server passes its code and params to the client.
- Headers pass by an allowlist only, and `Location` only while it stays under the package's prefix.

The order of the steps, the statuses and the timeouts of stream routes are in [Extension Points](../dev/extensions.md#plugin-routes).

The principal:

<!-- golden: principal/v1/valid/admin_session.json -->
```json
{"exp":"2026-12-01T00:01:05Z","kind":"session","local_admin":true,"platform_admin":true,"request_id":"gw-01/9f2c4e7a-000042","roles":["admin"],"route":"/reports/{period}","route_action":"acme_reports.read","user_id":1,"username":"admin"}
```

| Field | Rule |
|---|---|
| `kind` | `session` (a console login session), `token` (a platform token, on behalf of its user) or `public` (a `public` route, no user) |
| `user_id` / `username` | The user; absent for `public`, required otherwise |
| `platform_admin` | The user is a platform administrator |
| `local_admin` | The break-glass local administrator; can be true only for a platform administrator signed in with a session |
| `roles` | The user's roles, sorted, at least one; currently the single core role |
| `scope_projects` | Optional: the projects the user's readable data is limited to, sorted; empty means no limit |
| `api_key_id` | The platform token's id; required when `kind` is `token`, absent otherwise |
| `request_id` | The server's request id, for correlating logs, at most 128 bytes |
| `route_action` | The `action` of the route the server matched (`<package id>.<verb>`) |
| `route` | The manifest `pattern` of the route the server matched |
| `exp` | The expiry: the route's `timeout_s` (default 60) plus 5 seconds, rounded up to the second; a stream route's `timeout_s` bounds its first byte, and the same rule applies |

The `Gwx-Feature-Access` grammar is `<feature key>=<level>` entries joined by `,`, sorted by key, without spaces, where the level is `full`, `read_only` or `locked` (a route of a `hidden` feature answers 404 and never reaches the plugin). It lists only the keys the route declares, and has exactly one spelling.

Signing the principal above with the handshake golden file's `principal_key` (bytes 0 to 31) and `Gwx-Feature-Access` set to `acme.reports=read_only` gives these three headers (the vectors under `principal/v1/signed/` are frozen with the golden files):

<!-- golden: principal/v1/signed/admin_session.txt -->
```text
Gwx-Feature-Access: acme.reports=read_only
Gwx-Principal: eyJleHAiOiIyMDI2LTEyLTAxVDAwOjAxOjA1WiIsImtpbmQiOiJzZXNzaW9uIiwibG9jYWxfYWRtaW4iOnRydWUsInBsYXRmb3JtX2FkbWluIjp0cnVlLCJyZXF1ZXN0X2lkIjoiZ3ctMDEvOWYyYzRlN2EtMDAwMDQyIiwicm9sZXMiOlsiYWRtaW4iXSwicm91dGUiOiIvcmVwb3J0cy97cGVyaW9kfSIsInJvdXRlX2FjdGlvbiI6ImFjbWVfcmVwb3J0cy5yZWFkIiwidXNlcl9pZCI6MSwidXNlcm5hbWUiOiJhZG1pbiJ9
Gwx-Principal-Mac: P6RhOXLT3lxJuvyWlcXPsyKVm5hEWHTalujCg6k7XYw
```

`extapi.VerifyPrincipal(key, principal, MAC, feature access, now)` compares the MAC in constant time first and decodes only after it matches: strictly, requiring canonical JSON and refusing unknown fields, then checking the rules above. It refuses an `exp` before `now`, or later than `now` plus the longest route timeout (600 + 5 seconds). A missing header, a MAC mismatch and an expired principal are `ErrPrincipalMissing`, `ErrPrincipalMAC` and `ErrPrincipalExpired` (one too far ahead is `ErrPrincipalTooLong`); a malformed one is a coded `*extapi.Error`.

A plugin built with the SDK wraps each route where it registers it with `pluginsdk.RequirePrincipal(action, h)`:

```go
mux.Handle("POST /r/jobs", pluginsdk.RequirePrincipal("acme_backup.jobs_create", createJob))
```

It verifies the three headers with this spawn's key and requires `route_action` to equal the `action` of the route the plugin's own router matched. If the server's router and the plugin's match the same path to different routes, the request is therefore refused rather than served under the other route's access level and feature checks. A request that fails gets 403 and an error object marked with `Gwx-Product-Error` (`reason` `permission_denied`, `object` `principal`; a `PLUGIN_WIRE_*` code; a MAC mismatch and a route other than the one the server matched are both `PLUGIN_WIRE_INVALID` with `rule` `relation`, `field` `_root` and `route_action` respectively), and the handler is not called. The handler reads the verified caller with `pluginsdk.PrincipalFrom(r)` and `pluginsdk.FeatureAccessFrom(r)`. `plugintest` signs with a fixed key (`Harness.PrincipalKey`): `Harness.Principal(action, route)` returns an administrator session principal, and `Harness.Sign(req, p, access)` sets the three headers as the server does.

## Health and errors

The answer to a health probe: `status` is `ok` or `degraded`; `degraded` must carry a `code` that says why, and `params` may only appear with a `code`.

<!-- golden: health/v1/valid/degraded.json -->
```json
{"code":"ACME_QUOTA_STORE_LOCKED","params":{"path":"data/quota.db","retries":3},"status":"degraded"}
```

Every `gwx/1` error, in both directions, and every product error a plugin route returns, is the same object:

<!-- golden: error/v1/valid/busy.json -->
```json
{"code":"PLUGIN_BUSY","message":"the plugin is busy","reason":"busy","retry_after_ms":500}
```

| Field | Rules |
|---|---|
| `code` / `params` | The ADR 0067 code and parameters |
| `message` | The required English fallback, at most 2048 bytes |
| `reason` | A handling class independent of the code, closed within major 1: `invalid_argument`, `permission_denied`, `not_found`, `failed_precondition`, `conflict`, `busy`, `unavailable`, `unsupported`, `cancelled`, `deadline_exceeded`, `internal` |
| `retry_after_ms` | A suggested retry delay, 0–86400000, only with `busy` and `unavailable` |

The HTTP status of an error response follows its `reason` (`extapi.StatusForReason`): `invalid_argument` 400, `permission_denied` 403, `not_found` 404, `conflict` 409, `failed_precondition` 412, `busy` 429, `cancelled` 499, `unsupported` 501, `unavailable` 503, `deadline_exceeded` 504, `internal` 500. Readers go by the body; the status only lets a generic HTTP client tell success from failure and retry from refusal.

A plugin that finds, during the handshake, no version of a capability it cannot run without in the server's `offer` (or none within the range it speaks) refuses `initialize` with `PLUGIN_CAPABILITY_MISSING`:

<!-- golden: error/v1/valid/capability_missing.json -->
```json
{"code":"PLUGIN_CAPABILITY_MISSING","message":"the server offers no version of table.availability from 2 to 2","params":{"capability":"table.availability","max":2,"min":2},"reason":"unsupported"}
```

## Pushes and acknowledgements

A plugin pushes the whole table of one kind to the server. The owner is the package bound to the connection; no field of the push names it. A whole push is at most 16 MiB encoded.

<!-- golden: state_push/v1/valid/empty_table.json -->
```json
{"body":{},"generation":1,"kind":"node_holds","schema":1}
```

| Field | Rules |
|---|---|
| `kind` | The table kind, the same as the path segment |
| `schema` | The version of the rows in `body`, the `table.<kind>@<schema>` the plugin negotiated |
| `generation` | Strictly increasing per owner and kind, ≥1 |
| `base` | Only for kinds that define a patch form; no kind of major 1 does, so it must be absent |
| `body` | A JSON object, decoded strictly at `kind` and `schema` |

The server's acknowledgement says either that it applied the push (`applied_at` and `digest`) or that a newer push of the same owner and kind replaced it before it was committed (`superseded_by`, greater than `generation`); exactly one of the two:

<!-- golden: state_ack/v1/valid/applied.json -->
```json
{"applied_at":"2026-12-01T00:00:00Z","digest":"sha256:b4267ce93cbba4e415504feb895f662dcced5d7aa406f27e8447c8fd5f0d48c8","generation":42,"kind":"availability"}
```

<!-- golden: state_ack/v1/valid/superseded.json -->
```json
{"generation":42,"kind":"availability","superseded_by":43}
```

Each kind belongs to a class, which decides how the server combines the rows of several owners:

| Class | Kinds | Combination |
|---|---|---|
| Restrict | `fleet_capacity`, `node_holds` | Any approved owner may push; rows combine by intersection (the tightest cap wins; `keep` lists are unioned, since they only order the selection and never loosen a cap) and do not lapse while the owner is down |
| Grant | `availability` | Only from the namespace authority (or, for `gate: self` keys, the declaring package); tightens once the owner has been down longer than the grace |
| Display | `banners` | Shown, never enforced |
| Opaque | `attestations` | Stored and relayed, never interpreted |

## Node reads

A plugin reads node identity and the node inventory through the host API. These are the core's pinned reads (at most 500 nodes at a time), never a scan of the whole fleet. Each read needs a permission the administrator approved and a capability negotiated at the handshake:

| Endpoint | Permission | Capability |
|---|---|---|
| `POST /host/v1/node-identity` | `read:node_identity` | `host.node_identity@1` |
| `GET /host/v1/nodes?cursor=&limit=` | `read:node_inventory` | `host.nodes@1` |

Reads are not pushes, so an `on_demand` package may read too.

### Node identity

The body names a salt and node ids. It is a POST so the salt never appears in a URL:

<!-- golden: node_identity_request/v1/valid/two_nodes.json -->
```json
{"node_ids":[3,7],"salt":"acme-quota-2026"}
```

- `salt`: 1–219 bytes. The server keys the HMAC with "`pkg`, a zero byte, the package id, a zero byte, the salt", so two packages with the same salt get unrelated digests, and one package gets the same digests on every server; a package id is at most 32 bytes, so the key never exceeds 256 bytes.
- `node_ids`: the server's node ids, 1–500, without repeats.

The answer lists the named nodes that exist, in ascending id order; a node that does not exist (never did, or was deleted) is left out, and the caller compares ids:

<!-- golden: node_identity_answer/v1/valid/partial.json -->
```json
{"nodes":[{"cards":[{"h":"1b9e4d0a77c2f310","tail":"e7f8"},{"h":"c0a9e81f2b3d4e56","tail":"5555"}],"cards_without_serial":0,"host":{"h":"5f0c8e2a9d413b77","tail":"3432"},"node_id":3},{"cards":[],"cards_without_serial":8,"node_id":7}]}
```

- `host` is the machine's system UUID, absent until the agent reports one; logical nodes on one machine share it.
- `cards` are the cards the node may use that carry a hardware serial, ordered by `h`; cards whose id the probe made up from a slot or an index are only counted in `cards_without_serial`.
- Each `h` is the first 16 hex characters of an HMAC-SHA256 of the raw value; `tail` is the raw value's last four characters, unsalted, so a person can check it on the machine, and absent for a raw value shorter than eight characters. Raw serials never leave the server.

### Node inventory

`GET /host/v1/nodes` returns one page of nodes in ascending id (enrollment) order. The first page has no `cursor`; after that pass the previous page's `next` unchanged; an absent `next` means the last page. `limit` is 1–500 and defaults to 500. Node ids are never reused, so a node enrolled or deleted between pages neither shifts nor repeats the rest.

<!-- golden: nodes_page/v1/valid/first_page.json -->
```json
{"next":"7","nodes":[{"cards":[{"index":0,"model":"NVIDIA H800","serial":true},{"index":1,"model":"NVIDIA H800","serial":true}],"enrolled_at":"2026-11-02T08:15:00Z","host_reported":true,"id":3,"name":"gpu-a01","uuid":"6f1d2c3b-8a9e-4b7c-9d0e-1f2a3b4c5d6e"},{"cards":[{"index":0,"model":"Ascend 910B","serial":false}],"enrolled_at":"2026-11-05T10:00:00Z","host_reported":false,"id":7,"name":"npu-b02","uuid":"0b9c8d7e-6f5a-4b3c-2d1e-0f9a8b7c6d5e"}]}
```

Each node gives its id, `uuid`, `name` (the name an operator gave it, otherwise its hostname; at most 255 bytes), `enrolled_at`, `host_reported` (whether the machine's UUID was reported), and the cards it may use as `{index, model, serial}`: `index` is the vendor CLI's device index (-1 when the CLI does not list the card), and `serial` only says whether the card has a serial, and so appears in the node's identity; it never holds the serial.

### Rate limits and refusals

The host API allows each plugin 20 calls a second with a burst of 100; the node inventory has its own bucket of 2 pages a second with a burst of 4, and answers 429 `PLUGIN_HOST_RATE_LIMITED` beyond it (`limit` 2, with `retry_after_ms`). Read the nodes a console page shows, not the whole fleet on every event.

| Code | Status | Meaning | Params |
|---|---|---|---|
| `PLUGIN_HOST_NOT_PERMITTED` | 403 | The administrator did not approve the read's permission | `id`, `permission` |
| `PLUGIN_CAPABILITY_NOT_OFFERED` | 412 | The plugin did not negotiate the capability at its handshake | `id`, `capability`, `version` |
| `PLUGIN_HOST_ARGUMENT_INVALID` | 400 | The salt, the node id list, `cursor` or `limit` is out of bounds or breaks its grammar; refused before any read, and a refused inventory query costs no page | `id`, `field` |
| `REQUEST_TOO_LARGE` | 413 | The node identity request body is over 32 KiB; refused before decoding | `max_bytes` |
| `PLUGIN_HOST_ROUTE_UNKNOWN` | 404 / 501 | The server offers no reads, or the method is wrong | `method`, `path` |

A body that is not the object at all (invalid JSON, an unknown field) keeps its wire code, such as `PLUGIN_WIRE_UNKNOWN_FIELD`.

## Host API clock, readiness, own state and derived keys

These calls are on the host API side (fd 4). The caller is the package bound to the connection at its handshake; no request carries an identity field:

| Call | Needs | Answer |
|---|---|---|
| `GET /host/v1/meta` | capability `host.clock` | `host_meta` |
| `POST /host/v1/clock/correct` | capability `host.clock` and role `clock_authority` | `clock_corrected` |
| `POST /host/v1/state/ready` | the base protocol, no capability | 204, no body |
| `GET /host/v1/state/{kind}` | capability `host.state_read` | `pushed_state` |
| `POST /host/v1/derive-key` | permission `crypto:derive_key` and capability `host.derive_key` | `derived_key` |

A call whose capability the handshake did not negotiate gets 412 `PLUGIN_CAPABILITY_NOT_OFFERED` (`id`, `capability`, `version`); a missing role 403 `PLUGIN_ROLE_REQUIRED` (`id`, `role`); a missing permission 403 `PLUGIN_HOST_NOT_PERMITTED` (`id`, `permission`); an argument that breaks a rule 400 `PLUGIN_HOST_ARGUMENT_INVALID` (`id`, `field`); a body over 4 KiB 413 `REQUEST_TOO_LARGE` (`max_bytes`).

Meta: `version` is the server version (as in the handshake), and `boot_id` changes every time the server starts. `wall` is the wall clock, `extclock` the monotone clock timelines follow, and `hwm` the high-water mark the server last stored, absent before it stored one.

<!-- golden: host_meta/v1/valid/stored.json -->
```json
{"boot_id":"5b0c9e7a41d24f3c8e6a0b1d2c3f4e5a","extclock":"2027-03-01T00:00:00Z","hwm":"2027-03-01T00:00:00Z","version":"v26.12.0","wall":"2027-01-15T00:00:00Z"}
```

Clock correction: `to` is a time the plugin has verified (for example from a correction code its publisher signed), and `reason` is 1–256 bytes of explanation for the audit log. One correction moves extclock at most 366 days either way; beyond that the answer is 400 `PLUGIN_CLOCK_CORRECTION_TOO_LARGE` (`max_days`). Steps are not consumed, so moving back un-fires the steps after `to`. The server writes an audit log line and shows an administrator banner.

<!-- golden: clock_correction/v1/valid/vendor_code.json -->
```json
{"reason":"acme correction code 7F3A-22C1","to":"2027-01-15T00:00:00Z"}
```

The answer is extclock before and after the correction as it took effect: extclock never runs behind the wall clock, so when `to` is earlier than the wall clock, the answer's and the audit line's `to` is the wall clock.

<!-- golden: clock_corrected/v1/valid/back.json -->
```json
{"from":"2027-03-01T00:00:00Z","to":"2027-01-15T00:00:00Z"}
```

Ready: each down period's grants get a boot extension of 2 minutes after the grace. Once a plugin process calls `POST /host/v1/state/ready`, a later down period of that process gets no boot extension and tightens as soon as the grace ends. A second call from the same process, a call from an `on_demand` plugin, or one from a process that has been replaced changes nothing. It never changes the persisted down start, so it cannot renew the grace. A new process may call it before the server counts it up; that call applies only to the new process's own later down periods and does not shorten the boot extension of the previous process's down period. The mark is not kept across a server restart: the boot extension is computed again from the persisted start, so it cannot stack.

Own state: `GET /host/v1/state/{kind}` answers with the committed table of that kind the server holds for the caller itself, so a restarted plugin can reconcile. It is read from memory and never returns another package's rows. `body` is the whole table as the server holds it (canonical JSON; `display` contents, which the server does not keep, are absent), `digest` the digest of the table as it was pushed, the same as the handshake's `held` reports, and `pushed_at` the extclock at which the push was accepted. When nothing is held, only `kind` is set:

<!-- golden: pushed_state/v1/valid/held.json -->
```json
{"body":{"nodes":{"9":{"code":"ACME_QUARANTINE"}}},"digest":"sha256:9d48b3877a0b899cd055d9b094a38a3499e96cdad91bbb965c227098d95b9053","generation":4,"kind":"node_holds","pushed_at":"2026-12-01T00:00:00Z","schema":1}
```

<!-- golden: pushed_state/v1/valid/not_held.json -->
```json
{"kind":"banners"}
```

Derived keys: `label` is 1–64 of `[a-z0-9._-]`, and `length` is 16–64 bytes.

<!-- golden: key_request/v1/valid/challenge.json -->
```json
{"label":"usb.challenge","length":32}
```

<!-- golden: derived_key/v1/valid/thirty_two.json -->
```json
{"key":"AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="}
```

The key is HKDF-SHA256 (no salt) of the server's `master.key`, with `info` being `"gpuworks plugin key"`, a zero byte, the package id, a zero byte and the label, taking `length` bytes. One package and label get the same key for the life of the master key; other packages or labels get unrelated keys; the plugin never sees the master key itself.

## Host identity

`POST /host/v1/host-identity` returns the salted identity of the machine the management server runs on. It needs the `read:host_identity` permission and the `host.host_identity@1` capability; an `on_demand` package may read it too. The body holds only the salt, with the rules and package binding of [Node identity](#node-identity) (1–219 bytes, bound to the package id), so one package with one salt gets one set of digests from both reads:

<!-- golden: host_identity_request/v1/valid/salt.json -->
```json
{"salt":"acme-quota-2026"}
```

Each part of the answer is `{h, tail}`; a part the server could not read is absent (a firmware placeholder, a container, a root file system without a UUID), and when it reads nothing the answer is `{}`:

<!-- golden: host_identity_answer/v1/valid/linux_server.json -->
```json
{"board_serial":{"h":"9e1c0b7a4d2f6e83","tail":"BC9X"},"machine_id":{"h":"03b5d7f91a2c4e68","tail":"cdef"},"root_fs_uuid":{"h":"7a1f3c5e9b0d2486","tail":"0a2b"},"system_uuid":{"h":"5f0c8e2a9d413b77","tail":"3432"}}
```

- `system_uuid`: the firmware system UUID (SMBIOS `product_uuid` on Linux, `IOPlatformUUID` on macOS), digested as the same kind as a node's `host`, so under one salt it equals the `host` of each node on the server's machine.
- `board_serial`: the mainboard serial (the machine's serial number on macOS).
- `machine_id`: `/etc/machine-id`.
- `root_fs_uuid`: the file system UUID of the block device mounted at `/`.

`h` and `tail` mean what they mean for node identity. On Linux the kernel lets only root read the system UUID and the board serial, and the server runs as its service account: before every start of the server, systemd runs [`gpuworks host-identity`](cli.md#gpuworks-host-identity) as root from the protected executable copy, which writes those two to `/run/gpuworks/host-identity.json`. The server uses the file only to fill those two when it cannot read them itself; a value it reads directly always wins, and `machine_id` and `root_fs_uuid` come only from direct reads. Refusals are those of the [node reads](#rate-limits-and-refusals); a body over 4 KiB answers 413 `REQUEST_TOO_LARGE` (`max_bytes` 4096).

## Pushed tables

A row's `notice` is a code with parameters, and the code must be listed in the pushing package's manifest `codes`. Timelines are made of steps: only the first step may omit `at` (it holds from the push), later `at` values strictly increase, and a timeline has 1–64 steps; the server follows the steps on its own clock, and they take effect even while the plugin is down.

### `table.availability@1`

The availability timeline of each feature key the pusher is the authority for (or declared with `gate: self`).

<!-- golden: availability/v1/valid/authority.json -->
```json
{"down_grace_s":3600,"rows":[{"down_access":"read_only","feature":"acme.backup","steps":[{"access":"full","limits":[{"dim":"concurrent_jobs","max":4},{"dim":"gpus","unlimited":true}]},{"access":"full","at":"2027-01-01T00:00:00Z","link":"/x/acme_quota/renew","notice":{"code":"ACME_QUOTA_GRACE","params":{"days":30}}},{"access":"read_only","at":"2027-02-01T00:00:00Z","link":"https://acme.example/renew","notice":{"code":"FEATURE_EXPIRED"}}]},{"display":{"plan":"pro"},"down_access":"locked","feature":"acme.reports","steps":[{"access":"locked","notice":{"code":"ACME_NOT_PURCHASED"}}]}]}
```

- `down_grace_s`: how many seconds the rows stay as pushed after the owner goes down; 0 takes the server's default, otherwise 60–86400, and a site may only lower it.
- `rows`: at most 4096, with unique `feature`; `down_access` is `read_only` or `locked`, the ceiling the row tightens to after the grace.
- Steps: `access` is `full`, `read_only`, `locked` or `hidden`; an optional `notice`; `link` is an https URL or a console path (at most 2048 bytes); at most 16 `limits`, sorted by `dim`, each setting exactly one of `max` and `unlimited`.

### `table.fleet_capacity@1`

Fleet caps: the server refuses enrollments over a cap and holds over-cap nodes from scheduling. The pusher sends caps and, optionally, the nodes to keep first; the server still picks the nodes.

<!-- golden: fleet_capacity/v1/valid/authority.json -->
```json
{"hold":{"code":"ACME_OVER_CAPACITY"},"refuse_card":{"code":"ACME_CARD_LIMIT","params":{"max":64}},"refuse_node":{"code":"ACME_NODE_LIMIT","params":{"max":8}},"select":"enrollment_order","steps":[{"max_cards":64,"max_nodes_with_cards":8},{"at":"2027-02-01T00:00:00Z","max_cards":16,"max_nodes_with_cards":2}]}
```

`select` is only `enrollment_order` today (keep nodes in the order they joined and hold the rest once a cap is full); `refuse_node` and `refuse_card` explain a refused enrollment, and `hold` is the reason shown on a held node; an absent `max_nodes_with_cards` or `max_cards` in a step means no cap.

The optional `keep` lists the nodes to keep schedulable first when the fleet is over its caps:

<!-- golden: fleet_capacity/v1/valid/keep.json -->
```json
{"hold":{"code":"ACME_OVER_CAPACITY"},"keep":[3,7,12],"refuse_card":{"code":"ACME_CARD_LIMIT","params":{"max":64}},"refuse_node":{"code":"ACME_NODE_LIMIT","params":{"max":8}},"select":"enrollment_order","steps":[{"max_cards":64,"max_nodes_with_cards":8}]}
```

- `keep` holds server node ids (JSON integers, at least 1), strictly ascending with no duplicates, at most 10000; absent or empty means no preference, and nodes are selected in enrollment order.
- Selection order: the nodes in `keep` that exist and have cards (by ascending id) come first, then the other counted nodes in enrollment order; the rule is unchanged: a node is kept while both caps still fit, and the first node that does not fit and every counted node after it are held. A `keep` larger than the caps is therefore not an error, and its overflow is held; an id naming no node matches nothing (as in `node_holds`).
- The `keep` lists of several owners are unioned (the caps still take the tightest).
- A plugin should list only nodes it read from the fleet: a node listed before it joins is judged like any other new node until the next selection, and only then moves ahead, which may hold an earlier node.

### `table.node_holds@1`

Nodes the pusher holds from scheduling (maintenance, quarantine), each with its reason. A hold never stops running work; rows naming a node the server does not have are kept and match nothing.

<!-- golden: node_holds/v1/valid/maintenance.json -->
```json
{"nodes":{"12":{"code":"ACME_MAINTENANCE","params":{"ticket":"OPS-118"}},"9":{"code":"ACME_QUARANTINE"}}}
```

The keys of `nodes` are the server's node ids in decimal (never reused), at most 10000.

### `table.banners@1`

Banners above every console page, at most 16.

<!-- golden: banners/v1/valid/authority.json -->
```json
{"banners":[{"audience":"admin","id":"grace","steps":[{"link":"/x/acme_quota","notice":{"code":"ACME_QUOTA_GRACE","params":{"days":30}},"severity":"warning"},{"at":"2027-02-01T00:00:00Z","hidden":true}]},{"audience":"all","display":{"icon":"key"},"id":"usb_key_missing","steps":[{"at":"2026-12-01T00:00:00Z","notice":{"code":"ACME_USB_KEY_MISSING"},"severity":"danger"}]}]}
```

`id` is the owner's own and unique; `audience` is `admin` or `all`; each step is either `hidden: true` (with no other field) or carries a `severity` (`info`, `warning`, `danger`) and a `notice`, plus an optional `link`.

### `table.attestations@1`

Opaque statements the server stores and relays to the same publisher's other packages, never interpreting them.

<!-- golden: attestations/v1/valid/authority.json -->
```json
{"attestations":[{"blob":"c3RhdGVtZW50LTE=","feature":"acme.backup","format":"acme.grant-statement.v1","name":"grant_backup"},{"blob":"c3RhdGVtZW50LTI=","feature":"acme.reports","format":"acme.grant-statement.v1","name":"grant_reports"},{"blob":"AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8gISIjJCUmJygpKissLS4vMDEyMzQ1Njc4OTo7PD0+P0BBQkNERUZHSElKS0xNTk9QUVJTVFVWV1hZWltcXV5fYGFiY2RlZmdoaWprbG1ub3BxcnN0dXZ3eHl6e3x9fn+AgYKDhIWGh4iJiouMjY6PkJGSk5SVlpeYmZqbnJ2en6ChoqOkpaanqKmqq6ytrq+wsbKztLW2t7i5uru8vb6/wMHCw8TFxsfIycrLzM3Oz9DR0tPU1dbX2Nna29zd3t/g4eLj5OXm5+jp6uvs7e7v8PHy8/T19vf4+fr7/P3+/w==","format":"acme.binding.v1","name":"install_binding"}]}
```

At most 1024; `name` is unique; `feature` is optional; `format` tells a reader how to verify `blob`; `blob` is non-empty and at most 64 KiB.

## Error codes

Every decoding, validation and verification failure is an error with a stable code and parameters; the English message is only a fallback, and the console carries en and zh text for every code.

| Code | Meaning | Parameters |
|---|---|---|
| `PLUGIN_WIRE_MALFORMED` | Not one JSON value of the expected shape | `object`, `field`, `rule` |
| `PLUGIN_WIRE_UNKNOWN_FIELD` | A field this version does not define | `object`, `field` |
| `PLUGIN_WIRE_INVALID` | A field breaks a rule of its object | `object`, `field`, `rule` |
| `PLUGIN_WIRE_NOT_CANONICAL` | A signed payload is not canonical JSON | `object` |
| `PLUGIN_SCHEMA_TOO_NEW` | The object's version is newer than this build knows | `object`, `version`, `known` |
| `PLUGIN_MANIFEST_TOO_NEW` | The manifest schema is newer than this build knows | `version`, `known`, `min_core_hint` |
| `PLUGIN_PROTOCOL_UNSUPPORTED` | The peer's protocol range lacks this build's major | `min`, `max`, `supported` |
| `PLUGIN_SIGNATURE_UNTRUSTED` | No signature by a trusted key | `object` |
| `PLUGIN_SIGNATURE_INVALID` | A signature by a trusted key does not verify | `object`, `keyid` |
| `PLUGIN_STATE_REJECTED` | A pushed table row breaks a rule of its kind | `kind`, `field`, `rule`, `row` |
| `PLUGIN_STATE_TOO_LARGE` | A pushed table exceeds a size limit of its kind | `kind`, `field`, `max` |
| `PLUGIN_STATE_TOO_NEW` | A persisted version marker is newer than this build knows | `marker`, `version`, `known`, `needs`, `count` |
| `PLUGIN_CAPABILITY_MISSING` | The plugin refuses the handshake: the server offers no version of a capability it cannot run without within the range it speaks | `capability`, `min`, `max` |
| `PLUGIN_HOST_SETUP_REQUIRED` | Root has not applied the package's host steps on this host (or applied other steps), so its server part does not start | `id`, `command` |
| `PLUGIN_HOST_SETUP_UNSUPPORTED` | This platform cannot apply host steps; the package was not installed | `id` |

Values of `rule`: `required` (missing or empty), `grammar` (does not match its grammar), `range` (a number or length out of bounds), `enum` (not in the closed set), `duplicate`, `order` (a list that must be sorted is not), `exclusive` (sets fields that exclude each other), `relation` (contradicts another field of the object), `syntax`, `type`, `number` (not an integer, or outside the safe range) and `trailing`. `field` is a dotted path with list indexes, for example `routes.0.feature`.

`PLUGIN_STATE_TOO_NEW` comes from `extapi.CheckMarkers`: it compares persisted version markers (name, version, and the version of the build that wrote it: the server version for a core marker, the plugin build version for a plugin data marker) with the highest versions this build reads, and `needs` is the newest version among the writers of those markers, the version to run instead. A core marker missing from the known set counts as newer; a plugin data marker (`plugin.<id>.data`, at the `storage.state_version` of the build that wrote it) is compared only with the version the caller lists for an installed package, and the data of a package not listed never stops the core.

## Golden files

`pkg/extapi/testdata/golden/<kind>/v1/` is the frozen wire corpus:

- every file under `valid/` decodes, validates and re-encodes to the same bytes;
- every file under `invalid/<CODE>/` fails with exactly that code; a name `<field>--<rule>[--note].json` also gives the failing field and rule, and `_root` means the object itself;
- `principal/v1/signed/<name>.txt` holds the three headers `valid/<name>.json` signs to under the key of bytes 0 to 31, and `SignPrincipal` must give the same values byte for byte;
- `testdata/golden/SHA256SUMS` may only grow: a released golden file is never edited or removed, which `make check-architecture` enforces against its baseline; a new version of an object gets its own `v2/` directory.

From the repository root, `make test-extapi` runs the format check, vet and tests of this module; `make test` includes it.

<!-- translation-source-sha256: 80b29d1d2dc89880de610a200e6dc0ea7e80342edc835a0e1ecc4567a70318ac # Plugin Protocol and Wire Formats (gwx/1) -->
<!-- translation-source-sha256: e9b5a7bf20292b136e80cf55be445d96c675de7e4b6148ff7281dc52645b86f7 ## General rules -->
<!-- translation-source-sha256: 8b136dc0debcdfbbdca9200785c639fa215682a293534e151bd8e07a460462bf ### Versions and compatibility -->
<!-- translation-source-sha256: a5f9b8f0d6a6953f9c61c29c9e4580b68a3514f9258d084b5d3bf2ede552cb6f ## Canonical JSON and digests -->
<!-- translation-source-sha256: 6a1c40c3d1a7c4be25ea4270c3dad3578f8bd3151b9c3f039ae98e76756b9799 ## Signature envelope -->
<!-- translation-source-sha256: dfa574d853e0a9108c680b9b77a6de3b9c14ac34fd7ffbda54f21c0e0472c2f4 ## Package manifest -->
<!-- translation-source-sha256: b4623ce7398e720066aaeedddf5b810e745b126eb11943f5c7ef791d44027b4d ### Permissions -->
<!-- translation-source-sha256: 8cf3c55887fe1b1ede51fd944b8811f314fadc35724c0b7eda2a7aace8fdc87b ### Settings -->
<!-- translation-source-sha256: 3bc06d9fe6534bba808559a8262cff3b5136f1fbfa2c33acc48d69dbc0aa40bc ### The server's install policy -->
<!-- translation-source-sha256: 5109bed984ba15e4de0fa6195573f81d54118c85fe9a8431615f04a2522aac31 ### Host setup (`host.setup@1`) -->
<!-- translation-source-sha256: b6198a8f5efb9d9cc63ac6ff8aa9da9778589f4084169a2a0ce21a4f2e21b3bc ## Publisher certificate -->
<!-- translation-source-sha256: 50ab7af1d05ed35f14f5c6704971222b5b3f5915dcc27417c08d58aaf80e0ac7 ## Trust updates and revocation lists -->
<!-- translation-source-sha256: 49757da2cc185aff721a27affee3e747f6bd5346e17dbd49dcf9a61eabeca813 ## Package index -->
<!-- translation-source-sha256: 432b2239d6f3b263ba53321eb780dc7ca55238dc95cb17e9d8a15f35104ebe30 ## gwx/1 transport and handshake -->
<!-- translation-source-sha256: b493f0efb30b06cdf37b0caea876c4f017b8181cbc29c66cdaf3f3d932eced88 ## Route requests and the principal -->
<!-- translation-source-sha256: 2b3ff811a753947e0540897fb65172f852c291cb8a7d04b13b3de943f2e8eac9 ## Health and errors -->
<!-- translation-source-sha256: 93722f818870ffa714f7dec647d64496ed65658396e76f6bbf47203c624e0e17 ## Pushes and acknowledgements -->
<!-- translation-source-sha256: d7b6821f5b00b3040c4b975ed8ff0add5597a1406a8170dc647d35e1a91d88b2 ## Node reads -->
<!-- translation-source-sha256: bd39d657a8ef284e4596ffe2a7bab7be1974ddb4dabfa10a76f6831a9aaf048b ### Node identity -->
<!-- translation-source-sha256: 103602c378efc7c335540ccd8b8c8fba292d876e971b737d4fc140164b4baeca ### Node inventory -->
<!-- translation-source-sha256: 98db0211de8eb6f45236dfd31b9e26993d6e55c223abf18fc2ad6eabd108a74f ### Rate limits and refusals -->
<!-- translation-source-sha256: 83356c25f5d761f027aec9917af762be420d6a362eebd18061488da85d745b9a ## Host API clock, readiness, own state and derived keys -->
<!-- translation-source-sha256: e96d417555671f3bc74df4f6cf60d8348daf2cc6eeadbcc8ccee4476d8aa5cd0 ## Host identity -->
<!-- translation-source-sha256: 02d76dd9df6371ee363fc260804f8ffa741a5634920eb5c57f285604acfa30d7 ## Pushed tables -->
<!-- translation-source-sha256: 1466103f63782beca86ef7918d600ba97f34253e3cd161e0ebd833cb6f390095 ### `table.availability@1` -->
<!-- translation-source-sha256: 1cde251b8c098bcf0b6afb9fb39562182551cf96070f681bdcf78be560c3420a ### `table.fleet_capacity@1` -->
<!-- translation-source-sha256: ae222980d1484fd48651f759138a38299f82696259eb016cb4cbfb2b35a909a6 ### `table.node_holds@1` -->
<!-- translation-source-sha256: 2bedc50a6eee5da507dfcd4d085c4851025d3ab71384e4f0da25fe9dc2480477 ### `table.banners@1` -->
<!-- translation-source-sha256: 7aa0b67f4f6a1ff8ef74fef6166b463aa8d01a8ebe206ee20813d34f96f6391a ### `table.attestations@1` -->
<!-- translation-source-sha256: d96c495b834dadf07218106deb5d56f05c1ea07cd68bd3a807e62e4d36228b7f ## Error codes -->
<!-- translation-source-sha256: 9a7cddd7f8fda7aa69021189a02513fed46d5acbdde7bd96eab859eee7248d4e ## Golden files -->
