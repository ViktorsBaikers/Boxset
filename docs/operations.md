# Operating a Boxset installation

This page is what an operator needs when a start does not reach ready: where the
installation keeps its state, what each refusal means, and the three stopped commands
that recover it. Every command runs with the server stopped, holds the data
directory's lifetime lock, and prints a fixed reason code on failure.

## Data directory

`BOXSET_DATA_DIR` (default `/config`) holds:

| Path | What it is | Removed by |
| --- | --- | --- |
| `boxset.db` | the SQLite database (WAL mode) | only a restore replaces it, after the recovery copy exists |
| `boxset.lock` | the lifetime lock: one server, one stopped command | nothing |
| `backups/pre-migration-<from>-<UTC>.db` | the snapshots published before a migration; the newest three are kept | retention |
| `restore.journal` | the record of a restore that has not finished | the restore itself, once reconciliation committed |
| `recovery/restore-<UTC>.db` | the copy of the database a restore replaced | nothing, ever |
| `keys/<key_id>.key` | one 32-byte key per file, named by its SHA-256 | nothing, ever |
| `setup-token` | the first-admin token while setup is open | the first-admin flow |

Nothing in Boxset deletes a key file, a key version or a recovery copy. A key file
that no record names is ignored, and an operator may move a misplaced one aside.

## States

`GET /ping` reports the lifecycle state in `X-Boxset-State`. An authenticated
`GET /api/v1/system/status` reports the state and, while recovering, the reason.

| State | Meaning | What answers |
| --- | --- | --- |
| `starting` | the lock is held, the store is not open yet | `/ping` only |
| `recovering` | the installation cannot authorize anyone yet | `/ping`, and a current credential's own status outside `restore_in_progress` |
| `ready` | current authority decides every protected request | everything |
| `draining` | a stop is in progress | reads only; writes answer 503 `draining` |

`recovering` reasons:

| Reason | Cause | Recovery |
| --- | --- | --- |
| `key_missing` | a record names a key file that is not there | `boxset key import FILE` with the matching key, or `boxset secret reenter` for that record |
| `key_mismatch` | a key file exists but does not hold the bytes its name promises | restore the right file from a backup of `keys/`, or re-enter the affected records |
| `restore_in_progress` | a `restore.journal` could not be completed | read the start log, fix what it names, and start again |

While a reason is reported, login answers 503 and every protected route answers 401 or
503. A `key_missing` or `key_mismatch` installation still answers one request to a
current credential, its own status read, so an operator can see the reason there. A
`restore_in_progress` installation serves no credential at all, because every one of
them was issued at or below the epoch the pending restore is about to pass; the start
log line that names `restore_in_progress`, with the error that stopped the resume, is
the record of that state until the journal is finished or discarded. Nothing is
repaired automatically: a record is never re-encrypted over a key that is not there.

## Restoring a snapshot

```sh
docker stop boxset
docker run --rm -it -v boxset_config:/config -v /host/backup.db:/snapshot.db:ro \
  <image> /boxset restore --from /snapshot.db
docker start boxset
```

The command is refused, with nothing changed, when:

| Reason | Exit | Means |
| --- | --- | --- |
| `lock_held` | 3 | a server (or another stopped command) uses the directory |
| `restore_in_progress` | 3 | a journal from an unfinished restore is present |
| `restore_unsupported` | 3 | this platform cannot make a restore durable (Windows) |
| `snapshot_invalid` | 2 | the file is not a database this binary can restore |
| `snapshot_newer` | 2 | a newer Boxset wrote it; use that version |
| `installation_mismatch` | 2 | the snapshot belongs to another installation |
| `authorization_state_unavailable` | 2 | the current database holds no identity or access state to restore over |

On success it prints `restored epoch=<N> recovery=<path>`. Keep that path: it is the
previous state, and the only way back is a second `restore --from` it.

What a restore preserves and what it ends:

- The current database is copied with `VACUUM INTO` first, so committed writes still in
  the write-ahead log are in the copy; the restored file's own log is removed before
  the replacement so no frame of the old file can be replayed into it.
- The snapshot's data replaces the current data. Then the current installation's own
  accounts are applied over it by id: a name, verifier, role or disable that the
  installation holds now wins over the snapshot's, an account created after the
  snapshot is kept, and an account only the snapshot has is disabled.
- `setup` never reopens: a consumed first-admin credential stays consumed.
- `restore_epoch` rises above both sides, so every session and every Boxset API key
  from before the restore stops working. Browser users sign in again; integrations
  need new API keys.
- Separately encrypted provider credentials stay in the database and keep their keys.
  A restore never regenerates a key.
- An interrupted restore resumes at the next start: before the replacement, the staged
  copy and the journal are discarded; after it, the reconciliation is repeated before
  anything is ready.

## Recovering a key

```sh
boxset key import /host/<key_id>.key
boxset secret reenter --id 3 --from /host/credential
```

`key import` accepts exactly 32 bytes whose SHA-256 is a key the installation already
records (current or archived) and publishes them at `keys/<key_id>.key`. It never
rewrites bytes already there. A fingerprint no record names is refused with exit 2
`key_unknown`; bytes at that path that differ are refused with exit 2 `key_unsafe` —
move the file aside yourself, Boxset never does.

`secret reenter` replaces one record's plaintext from a file only its owner can read
(`mode` 0600, 1 byte to 1 MiB). It encrypts under the current key when that key is
usable, and otherwise publishes a fresh key and points the installation at it; in both
cases the key passes its platform barrier before anything references it. Version N+1
becomes current, version N and every earlier version keep their key, nonce and
ciphertext, and `session`, `api_key` and `restore_epoch` are untouched, so old Boxset
credentials stay denied. The supplied bytes never appear in a command line, an output
or a log.

A platform barrier that does not complete exits 1 with
`key_sync_failed stage=<file|dir|metadata>`: nothing references the key until a retry
completes it. Exit 2 `key_unsafe` means the file is not an owner-only regular file of
the bytes its name promises; exit 2 `input_invalid` means the input file is not
readable as an owner-only regular file of the allowed size.

## Windows

`boxset restore` and a start that finds `restore.journal` exit 3
`restore_unsupported` on Windows, before anything is created or changed. Key import
and `secret reenter` are available, with the metadata barrier described in
`docs/portability.md`; their Windows behaviour is compile-only and not run.
