# 0014 — The measured effects checkpoint runs at the release milestone

Status: accepted (owner decision 2026-10-30, DEC-064).

## Context

`boxset-m00a-effects` carries AC-024's resource clause and AC-035's measured delivery clause as an owner action-time checkpoint (HZN-002). The seven-position job-start/writer series was executed on the development host and judged `EFFECTS-BENCH-INVALID` on host eligibility only (`numa_nodes=0` on a kernel without NUMA sysfs, 5,485–5,613 foreign threads allowed on CPUs 0–3, and swap activity; EVID-040/041). The measured job-start values were clean (p95 5.60–10.81 ms on four CPUs, 4,482–4,817 healthy frames, zero gaps, ≥1,015 in-load writer samples). Exact zero swap is unreachable on that host: 54.5 GiB sits in zram against 23 GiB available, and pausing 705 swapped tasks still left 133 swap-in pages per 30 s.

## Decision

1. The measured checkpoint is **deferred to the release milestone** (sequence position 24, `sequence_role: release`). The effects workspace seals with the row visibly deferred and reports **NOT-RUN**, never PASS.
2. The release milestone re-adds exactly this runnable gate to its ledger and runs it on an eligible host: one NUMA node, four physical cores reserved so no foreign task is allowed on them or their SMT siblings, no swap activity, local NVMe ext4/XFS data directory, empty `/config` (HZN-002).
3. The release run uses the seven positions (`AB/'' BA/'' AB/'' BA/'' AB/'' AB/1 AB/64`) with `BOXSET_BUILD_VERSION=bench` (one SvelteKit app version so the embedded shell and the harness binary are byte-identical across the series) and `BOXSET_BENCH_CGROUP_PARENT` pointing at the reserved slice (so the container cannot escape into a shared slice). The judge is:
   `scripts/dev-run.sh -- sh -euc 'bun --cwd=web run build; env CGO_ENABLED=0 go build -trimpath -o /tmp/bench-effects ./bench/effects; exec /tmp/bench-effects -judge "$0"' "$RUNS"`.
   `EFFECTS-BENCH-PASS` is required; anything else is NOT-RUN/INVALID, never PASS.
4. The release workspace must carry the gate text, the exact command, the eligible-host contract and the `RUNS` binding in its spec, test plan and gates ledger. The carry is recorded in DEC-064, this ADR, the effects workspace `seal.md`/`gates.md` and `review.md`.
5. No acceptance criterion changes: the behavioural AC-024/AC-035 gates remain met; only the owner's measured observation moves. The measured checkpoints stay NOT-RUN until the release run passes.

## Consequences

The measured resource/delivery observation is not claimed before release. The release milestone cannot seal until the series passes on an eligible host. The harness/runner repairs (deterministic reader stop, slow-clause causal split, pinned shell version, no-NUMA single-node record, reserved cgroup placement, mount-table fallback with device identity, observed slow opens, admission receipt timing) are prerequisites for that run.

## Measured on homecloud (2026-10-30)

The owner supplied `homecloud` (95.211.154.3): zram swap off, `bench.slice` 0–3, host and Incus cgroups on 4–47,52–95, data on XFS/NVMe md2. The seven-position series ran with one digest (`d96d52d3…`) and the judge returned `EFFECTS-BENCH-PASS job_start_p95_a1_ns=10321816 job_start_p95_a2_ns=13791422 noise_ns=3469606`; every host record shows `numa_nodes=1`, foreign allowed/runnable 0, unchanged swap, XFS/NVMe data. Position 7's retention recorded 2100 peer requests against 2080 logical (20 same-host safe-read retries on the slower reserved cores); the judge now allows up to `retentionRetryAllowance=64` such retries while the logical probes, keys and budget refusal stay exact (DEC-067). The deferral above is therefore closed by measurement, not carried to the release milestone.
