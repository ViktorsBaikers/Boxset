## Recording and replaying outbound fixtures

Provider and contract tests replay recorded wire exchanges instead of calling a
provider. A recording is scrubbed before it is saved and re-read for a value the
scrub left behind, and it is published only when its command exits 0, so a
refused recording leaves the committed set exactly as it was.

Record the set (the project has no live recording; the set's provenance is
`synthetic`, generated in this repository, so no account, credential or `.env`
is needed):

```sh
make fixtures-record SET=outbound-contract
```

The same command without `make`:

```sh
scripts/dev-run.sh --write internal/outbound/testdata/fixtures/outbound-contract -- env CGO_ENABLED=0 go run ./internal/fixture/record -set outbound-contract
```

Replay it offline:

```sh
scripts/dev-run.sh -- env CGO_ENABLED=0 go test -count=1 ./internal/fixture/... ./internal/outbound/...
```

The recording lands in
`internal/outbound/testdata/fixtures/outbound-contract/exchanges.json`, one file
per set, at most 8 MiB. Two recordings of the same set are the same bytes, so a
rerun of the command above leaves `git diff` empty.

Prerequisites: run from the repository root (`not_repo_root` otherwise); the host
needs `make`, `sh`, a reachable Docker daemon (`docker_unavailable`) and the
runner's own host tools (`host_tool_missing`); no host Go or Bun is used. The
throwaway recording service listens on `127.0.0.1:8080`, the address and port the
saved request URLs name: a host that already holds it reports
`recording_incomplete` rather than saving a set nothing replays. A cold or changed
`work` image build needs daemon network — see the
[Cold bootstrap section](docs/spikes/s5-version-admission.md#cold-bootstrap-and-lock-generation-owner-docker-and-network-required).
The record and replay child always runs `--network none`, with its throwaway
services on its own loopback.

Recovery: in every case nothing is published, the committed set is kept, and the
output carries only a fixed reason word, never a request, a header or a body.
`TestFixtureRecordSafety` asserts every refusal below except `scrub_incomplete`,
which `TestScrubIncompleteRefusesAndKeepsPrior` owns; `internal_error` is the
command's fallback for a failure none of them classifies.

- `scrub_incomplete` — a value the scrub must remove survived into the saved
  bytes. Fix the scrub rule or the synthetic data, then rerun the same command.
- `target_unclassified` — a call reached an origin the recorder did not start.
  Register it as a recorder-started synthetic service; never reclassify a target
  or point a retry at a live or write target.
- `live_method_denied` — a non-GET call addressed a live target. Only GETs may
  reach a live service; record the write against a throwaway service instead.
- `provenance_required` — the set does not carry the provenance the recording
  proves (a live capture is not the repository's synthetic set), or names none.
  Record the set from the source it actually drove.
- `set_invalid` — the set name is not one lower-case `[a-z0-9-]` word of at most
  32 characters. Record under a set name this harness accepts.
- `set_unknown` — no recipe exists for that set name. Use a set this repository
  produces.
- `recording_incomplete` — the recipe did not leave the exchanges it names, a
  body passed the file bound, an answer arrived with a content-encoding the scrub
  cannot read, a registered target names a socket the recorder does not serve, the
  throwaway service could not take its port, or a call failed without the signature
  of a peer that read the request. Fix the recipe or the service and rerun.
- `internal_error` — the failure was not one of the refusals above; nothing is
  published. Rerun once to see whether it reproduces, then report it.
