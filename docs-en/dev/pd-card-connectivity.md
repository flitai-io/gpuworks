# Accelerator card connectivity in PD evaluation

An accelerator compute network declares which fabric a deployment uses; it
does not guarantee that every card port reaches every other card port. In
particular, matching card indexes do not establish reachability.

For a cross-node PD mode whose KV transport uses HCCL, deployment evaluation
reads the latest card test of the selected compute network. It checks the
explicitly selected prefill and decode ports using the network's current
RoCE or HCCS addresses. A card ping includes a reply, so the card tester stores
one round-trip observation per unordered node pair; either stored orientation
is accepted. Host TCP and host RDMA tests are not
accelerator-port evidence.

| Warning | Meaning |
| --- | --- |
| `PD_CARD_NETWORK_UNREACHABLE` | A matching probe recorded an unreachable selected port pair. |
| `PD_CARD_NETWORK_UNTESTED` | The selection has no complete current evidence, including automatic card selection, a missing port pair, a tool failure, changed addresses or a card test older than 24 hours. |

Warnings include the network, selected port pairs and bidirectionally verified
alternatives from the same test (up to eight pairs). Reachability does not mean
the cards are idle; placement checks their availability separately.
Warnings remain non-blocking: `ok: true` means
the request is accepted, not that its physical fabric or inference engine is
guaranteed to start. Pin the intended cards and run a card matrix test before
deployment. Connectivity evidence cannot diagnose every local Mooncake
initialization failure, and does not replace engine startup validation.

<!-- translation-source-sha256: cc365f7cb34920f2a378004cfae68b401b3120928de30227f63ea03f2ea9c4c0 # Accelerator card connectivity in PD evaluation -->
