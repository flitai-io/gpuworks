# ADR 0060: A card's health is judged only from hardware facts; event-type errors need evidence of recovery, and operator decisions come first

Date: 2026-11-01 · Status: Accepted (supplements 0043)

## Background

A card's health used to come only from the vendor tool's state fields (ECC counters, memory rows pending replacement, `npu-smi`'s Health and so on) and the platform's own runtime check. The signals being added (PCIe links and AER counters, NVIDIA Xid, NVLink state) are of two kinds: states, like the old ones, saying how things are now; and events, saying something just happened, which have no reading that goes back to normal on its own.

[ADR 0043](0043-health-by-probe-not-rules.md) removed the rules and thresholds that inferred health from metrics, because the guesses were unreliable. The new signals have to say why they are not that kind of guess.

## Decision

1. **Only facts count**: only three things may change a card's health level — hardware error counters growing (AER, ECC, RAS), a recovery the vendor states explicitly (`gpu_recovery_action`, PPU's `xid.*` fields, the official "immediate action" class of an Xid), and a link negotiated below what both its ends support. Temperature, throttle reasons and utilization remain metrics and do not judge health.
2. **Debouncing only filters sampling noise**: a degraded link is raised after 3 reports in a row and cleared after 3 good ones; link speed is judged only on a card one of this node's instances uses and that is not asleep, because idle cards and switch uplinks lower their speed on purpose. This is not threshold tuning but "when a reading means something".
3. **State signals clear on the next normal reading**; **event signals need evidence of recovery**: a runtime check passing on the card after the event, or a host reboot or driver reload. Time passing is not evidence — the driver logs once when something happens, and no new log does not mean the card is well. Non-fatal, recovered errors (AER non-fatal) only warn for a while and need no evidence.
4. **Operator decisions come first**: automatic verdicts are defaults. An administrator can set a card or node back to schedulable at any time; while the platform's reason for stopping scheduling remains, the next report stops it again, and that window is enough to verify by hand. The platform lifts only the scheduling stops it set itself and never one an operator set.
5. **Running instances on a faulty card are left alone**: verdicts only affect new placements; what happens to running instances belongs to the high-availability policy, designed separately.

## Trade-offs

- Requiring evidence of recovery for event signals keeps a card on a node without a runtime check (no runtime on the host and no inference image on the node to borrow) stopped after a fatal error until the host reboots; an administrator can open it by hand to verify. Better to stop one card too many than to give a card that just had a fatal error to a new deployment a minute later.
- AER counters are counted from when the agent starts: an agent restart loses the increases before it, and does not take the history since boot as new errors.

<!-- translation-source-sha256: 8df92de8e08e009da35393c77eb14527a9dde33c27ba4efd48cd8bcca1afa48b -->
