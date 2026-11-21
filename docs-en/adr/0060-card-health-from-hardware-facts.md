# ADR 0060: A card's health is judged only from hardware facts; event-type errors need evidence of recovery, and operator decisions come first

Date: 2026-11-01 · Status: Accepted (supplements 0043)

## Background

A card's health used to come only from the vendor tool's state fields (ECC counters, memory rows pending replacement, `npu-smi`'s Health and so on) and the platform's own runtime check. The signals being added (PCIe links and AER counters, NVIDIA Xid, NVLink state) are of two kinds: states, like the old ones, saying how things are now; and events, saying something just happened, which have no reading that goes back to normal on its own.

[ADR 0043](0043-health-by-probe-not-rules.md) removed the rules and thresholds that inferred health from metrics, because the guesses were unreliable. The new signals have to say why they are not that kind of guess.

## Decision

1. **Only facts count**: only three things may change a card's health level — hardware error counters growing (AER, ECC, RAS), a recovery the vendor states explicitly (`gpu_recovery_action`, PPU's `xid.*` fields, the official "immediate action" class of an Xid), and a link negotiated below what both its ends support. Temperature, throttle reasons and utilization remain metrics and do not judge health.
2. **Debouncing only filters sampling noise**: a degraded link is raised after 3 reports in a row and cleared after 3 good ones; link speed is judged only on a card one of this node's instances uses and that is not asleep, because idle cards and switch uplinks lower their speed on purpose. This is not threshold tuning but "when a reading means something".
3. **State signals clear on the next normal reading**; **event signals need evidence of recovery**: a runtime check passing on the card after the event, or a host reboot or driver reload. Time passing is not evidence — the driver logs once when something happens, and no new log does not mean the card is well. Non-fatal, recovered errors (AER non-fatal) only warn for a while and need no evidence.
4. **Operator decisions come first**: automatic verdicts are defaults. Explicitly enabling or disabling a card persists until an administrator changes the decision, across reports and server restarts, with no expiry. Health and outside use remain visible; passed-through or driverless cards still cannot be enabled. Cards without an explicit decision follow automatic verdicts. Node automatic stop/resume rules are unchanged, and the platform never lifts an operator's stop. (2026-11-18, GWKS-389: existing enable/disable actions replace the temporary card allowance window.)
5. **Running instances on a faulty card are left alone**: verdicts only affect new placements; what happens to running instances belongs to the high-availability policy, designed separately.

## Trade-offs

- Requiring evidence of recovery for event signals keeps a card on a node without a runtime check (no runtime on the host and no inference image on the node to borrow) stopped after a fatal error until the host reboots; an administrator can open it by hand to verify. Better to stop one card too many than to give a card that just had a fatal error to a new deployment a minute later.
- AER counters are counted from when the agent starts: an agent restart loses the increases before it, and does not take the history since boot as new errors.

<!-- translation-source-sha256: eecce64fc3697357f7e2c7848488978bfef0e7aa2a7d90fbf8beee12b7b9d7c1 # ADR 0060: A card's health is judged only from hardware facts; event-type errors need evidence of recovery, and operator decisions come first -->
<!-- translation-source-sha256: 959b297e5f3839baaf0a9a431c0b3028884566852d59d38455e1d1ea25a18f8e ## Background -->
<!-- translation-source-sha256: f1e13755a88cd487aeed72e295eab73b009dfd1f4f3962b11bc4c6957f67cdb0 ## Decision -->
<!-- translation-source-sha256: 6e4552129f36ddda58d245a8cc2a3d6064f629b755c851603d1f9e1e03e81a75 ## Trade-offs -->
