# ADR 0013: Benchmark storage predicate accepts XFS beside ext4

- **Status:** Accepted owner decision, 2026-10-28. Owner directive: "We do not need ext4. XFS is totally fine, adjust please."
- **Amends:** ADR 0010's measurement-host storage clause and ADR 0005 T10's eligibility wording, which name ext4 only. No other clause of either ADR moves.
- **Preserves:** the reference profile's four physical same-NUMA cores without SMT siblings, 4 GiB with no container swap, the frozen swap counters, the fixed timing protocol, every budget, the sole-host policy, the local-non-rotational-NVMe requirement, and exact Git approval boundaries.

## Decision

The `/config` eligibility witness and the run's own data directory must sit on a **local, non-rotational NVMe block device** formatted **ext4 or XFS**. Any other filesystem — network, FUSE, 9p, overlay, tmpfs, and every other local filesystem — stays ineligible and keeps its existing reason token.

- **Recorded words.** V1 `config_fs` and V2 `data_fs` gain `xfs` beside `ext4`; `other` stays the word for everything else. Decoders accept the three words and refuse anything else as `record_malformed`; judges accept `ext4` and `xfs` and refuse `other` with the unchanged tokens (`filesystem` for `/config`, `data_filesystem` for the run directory).
- **Runner.** The host classification records the filesystem it observed instead of only asking "ext4 or not", and refuses the run before measurement when it is neither ext4 nor XFS.
- **Unchanged.** Nothing about device backing: `backing_nvme` still resolves md and device-mapper members to their disks and requires each to be non-rotational, local and NVMe. Nothing about ownership, emptiness, read-only mounting or identity binding.

## Why

The predicate exists to keep budget samples on storage the product actually used: local, non-rotational, no network or overlay indirection. XFS satisfies that as fully as ext4 — it is a local journaling filesystem on the same class of device, with no measured property in the protocol (start, key, hash, abuse, gzip, RSS) that distinguishes them by filesystem. The ext4-only wording excluded the owner's own development server for no measurement benefit: every ext4 filesystem there is the `/boot` RAID member, so the sole host that ADR 0010 designates could not produce a single valid sample, which is the opposite of what the profile is for.

## Consequences

- A run measured on XFS is valid under the protocol but is not byte-identical to an ext4 run; comparisons across this amendment name the filesystem they came from.
- The eligibility gate stays falsifiable: `other` still fails, and the judge still refuses a run whose storage it cannot read, whose device is not NVMe, or whose directory is not the mount created for it.
- The ADR 0001 current-authority index row for this amendment is owed to the next session that edits that file; this ADR is authority on its own terms until then.

**Proof owed:** judge cases that accept `xfs` and refuse `other` for both `/config` and the data directory, runner classification cases for all three words, and one measured window whose records carry `data_fs: "xfs"` and are judged by the unchanged budgets.
