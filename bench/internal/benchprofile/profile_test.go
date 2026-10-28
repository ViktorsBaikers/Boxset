package benchprofile

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// v1Before is one V1 record of the parser judge's cohort, byte for byte: the
// before record of its eligible control. The bytes are the wire the host runner
// writes and every judge reads, so they are a fixture and not the encoder's
// output: a changed key, tag or order is a changed fixture.
const v1Before = `{"schema":1,"stage":"before","run":"bench-20261017T101500Z","image":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","inputs":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","start_unix":1792232100,"end_unix":1792232100,"observations":1,"cpus":4,"cores":4,"threads_per_core_max":2,"siblings_outside_set":4,"numa_nodes":1,"sibling_runnable_max":0,"foreign_allowed_max":0,"foreign_runnable_max":0,"load1_milli_max":20,"mem_available_kib_min":16777216,"swap_in_start":7,"swap_in_end":7,"swap_out_start":9,"swap_out_end":9,"mem_limit_bytes":4294967296,"memswap_limit_bytes":4294967296,"cpuset_matches":true,"config_fs":"ext4","config_nvme":true,"config_empty":true,"config_readonly":true}`

// TestV1WireUnchanged: the moved schema still answers as bench/parser's did. The
// recorded V1 record decodes to exactly these values and re-encodes to exactly
// these bytes, the date layout names the UTC calendar day of a record instant and
// parses its own output back, and a mutated key set, a duplicate key, a bad shape
// and a bad stage keep their reason tokens.
func TestV1WireUnchanged(t *testing.T) {
	want := Facts{
		Schema: FactSchema, Stage: StageBefore, Run: "bench-20261017T101500Z",
		Image: "sha256:" + strings.Repeat("a", HexSHA256Len), Inputs: strings.Repeat("b", HexSHA256Len),
		StartUnix: 1792232100, EndUnix: 1792232100, Observations: 1,
		CPUs: 4, Cores: 4, ThreadsPerCoreMax: 2, SiblingsOutsideSet: 4, NUMANodes: 1,
		Load1MilliMax: 20, MemAvailableKiBMin: 16 << 20,
		SwapInStart: 7, SwapInEnd: 7, SwapOutStart: 9, SwapOutEnd: 9,
		MemLimitBytes: RequiredMemory, MemSwapLimitBytes: RequiredMemory, CpusetMatches: true,
		ConfigFS: ConfigFSExt4, ConfigNVMe: true, ConfigEmpty: true, ConfigReadOnly: true,
	}
	got, err := DecodeFacts([]byte(v1Before))
	if err != nil {
		t.Fatalf("the recorded record must decode: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded = %+v, want %+v", got, want)
	}
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != v1Before {
		t.Fatalf("the wire changed:\n got %s\nwant %s", data, v1Before)
	}

	// The date layout is the UTC calendar day, and it crosses midnight into a
	// different day: the pair a rollover is judged on.
	for _, tc := range []struct{ instant, date string }{
		{"2020-01-13T23:59:59Z", "20200113"},
		{"2020-01-14T00:00:00Z", "20200114"},
		{"2026-10-17T10:15:00Z", "20261017"},
	} {
		at, err := time.Parse(time.RFC3339, tc.instant)
		if err != nil {
			t.Fatal(err)
		}
		if got := FactsUTCDate(at.Unix()); got != tc.date {
			t.Errorf("%s: date = %s, want %s", tc.instant, got, tc.date)
		}
		day, err := time.Parse(DateLayout, tc.date)
		if err != nil {
			t.Fatalf("%s: parse: %v", tc.date, err)
		}
		if !day.Equal(at.Truncate(24*time.Hour)) || day.Location() != time.UTC || day.Format(DateLayout) != tc.date {
			t.Errorf("%s parsed to %s (%s)", tc.date, day, day.Location())
		}
	}

	for _, tc := range []struct {
		name   string
		data   string
		reason string
	}{
		{"the recorded record", v1Before, ""},
		{"unknown key added", strings.Replace(v1Before, `"schema":1`, `"schema":1,"host":"example"`, 1), "record_incomplete"},
		{"key renamed out of the schema", strings.Replace(v1Before, `"cores":4`, `"host_cores":4`, 1), "record_incomplete"},
		{"missing key", strings.Replace(v1Before, `"swap_in_end":7,`, "", 1), "record_incomplete"},
		{"another schema", strings.Replace(v1Before, `"schema":1`, `"schema":2`, 1), "record_malformed"},
		{"config filesystem xfs", strings.Replace(v1Before, `"config_fs":"ext4"`, `"config_fs":"xfs"`, 1), ""},
		{"config filesystem neither ext4 nor xfs nor other", strings.Replace(v1Before, `"config_fs":"ext4"`, `"config_fs":"btrfs"`, 1), "record_malformed"},
		{"another stage", strings.Replace(v1Before, `"stage":"before"`, `"stage":"later"`, 1), "record_malformed"},
		{"number as text", strings.Replace(v1Before, `"cpus":4`, `"cpus":"4"`, 1), "record_malformed"},
		{"null scalar", strings.Replace(v1Before, `"cpuset_matches":true`, `"cpuset_matches":null`, 1), "record_malformed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeFacts([]byte(tc.data))
			if tc.reason == "" {
				if err != nil {
					t.Fatalf("DecodeFacts = %v, want valid", err)
				}
				return
			}
			reason, ok := InvalidReason(err)
			if !ok || reason != tc.reason {
				t.Fatalf("verdict = %v, want INVALID %s", err, tc.reason)
			}
		})
	}

	// A duplicate key is neither an unknown nor a missing one: the key set is the
	// same and encoding/json keeps the last value. That is the decoder's recorded
	// answer, so a decoder that started refusing duplicates would fail here.
	dup := strings.Replace(v1Before, `"cpus":4`, `"cpus":4,"cpus":3`, 1)
	if dupRec, err := DecodeFacts([]byte(dup)); err != nil || dupRec.CPUs != 3 {
		t.Fatalf("duplicate key = %d, %v, want the last value", dupRec.CPUs, err)
	}
}

// v2Before is one V2 record of the access judge's cohort, byte for byte: the
// before record of an eligible run. Its bytes are the wire the runner writes and
// every judge reads, so they are a fixture and not the encoder's output: a changed
// key, tag, order or nesting is a changed fixture.
const v2Before = `{"schema":2,"stage":"before","run":"bench-20261017T101500Z","image":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","inputs":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","host":{"start_unix":1792232100,"end_unix":1792232100,"observations":1,"cpus":4,"cores":4,"threads_per_core_max":2,"siblings_outside_set":4,"numa_nodes":1,"sibling_runnable_max":0,"foreign_allowed_max":0,"foreign_runnable_max":0,"load1_milli_max":20,"mem_available_kib_min":16777216,"swap_in_start":7,"swap_in_end":7,"swap_out_start":9,"swap_out_end":9,"mem_limit_bytes":4294967296,"memswap_limit_bytes":4294967296,"cpuset_matches":true,"config_fs":"ext4","config_nvme":true,"config_empty":true,"config_readonly":true},"data":{"data_fs":"ext4","data_nvme":true,"data_writable":true,"data_bound":true,"data_mounted":true}}`

// v2Want is that record decoded: its five envelope keys, the host evidence the V1
// record carries under the same names, and the five data facts.
func v2Want() Record {
	return Record{
		Envelope: Envelope{
			Schema: RecordSchema, Stage: StageBefore, Run: "bench-20261017T101500Z",
			Image: "sha256:" + strings.Repeat("a", HexSHA256Len), Inputs: strings.Repeat("b", HexSHA256Len),
		},
		Host: Host{
			StartUnix: 1792232100, EndUnix: 1792232100, Observations: 1,
			CPUs: 4, Cores: 4, ThreadsPerCoreMax: 2, SiblingsOutsideSet: 4, NUMANodes: 1,
			Load1MilliMax: 20, MemAvailableKiBMin: 16 << 20,
			SwapInStart: 7, SwapInEnd: 7, SwapOutStart: 9, SwapOutEnd: 9,
			MemLimitBytes: RequiredMemory, MemSwapLimitBytes: RequiredMemory, CpusetMatches: true,
			ConfigFS: ConfigFSExt4, ConfigNVMe: true, ConfigEmpty: true, ConfigReadOnly: true,
		},
		Data: Data{FS: ConfigFSExt4, NVMe: true, Writable: true, Bound: true, Mounted: true},
	}
}

// v2Stages is one run's three records as the runner writes them: the same wire
// with each stage's own stage, window and observation count.
func v2Stages() [3][]byte {
	during := strings.NewReplacer(
		`"stage":"before"`, `"stage":"during"`,
		`"start_unix":1792232100,"end_unix":1792232100,"observations":1`,
		`"start_unix":1792232101,"end_unix":1792232109,"observations":9`,
	).Replace(v2Before)
	after := strings.NewReplacer(
		`"stage":"before"`, `"stage":"after"`,
		`"start_unix":1792232100,"end_unix":1792232100,"observations":1`,
		`"start_unix":1792232110,"end_unix":1792232110,"observations":1`,
	).Replace(v2Before)
	return [3][]byte{[]byte(v2Before), []byte(during), []byte(after)}
}

// TestV2EnvelopeStrict: the V2 envelope carries the V1 host evidence in its own
// block and the data evidence beside it. The recorded record decodes to exactly
// these values and re-encodes to exactly these bytes; each block holds exactly its
// own keys, derived from the struct and never written down again; and a mutated key
// set, a duplicate key, a null value, a bad stage, a wrong schema both ways,
// another version's record and trailing data each keep their reason token.
func TestV2EnvelopeStrict(t *testing.T) {
	want := v2Want()
	got, err := DecodeRecord([]byte(v2Before))
	if err != nil {
		t.Fatalf("the recorded record must decode: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded = %+v, want %+v", got, want)
	}
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != v2Before {
		t.Fatalf("the wire changed:\n got %s\nwant %s", data, v2Before)
	}

	// The key sets are the schemas', derived from the structs: the five envelope
	// keys above the two blocks, the host evidence exactly as V1 names it, and the
	// five data facts. Each list is a literal, so a renamed tag reds here; the order
	// and the names of the host half are pinned by TestV1WireUnchanged's bytes.
	if !reflect.DeepEqual(RecordKeys, []string{"schema", "stage", "run", "image", "inputs", "host", "data"}) {
		t.Fatalf("record keys = %v", RecordKeys)
	}
	if !reflect.DeepEqual(HostKeys, FactKeys[5:]) {
		t.Fatalf("host keys = %v, want the V1 record's host half %v", HostKeys, FactKeys[5:])
	}
	if !reflect.DeepEqual(DataKeys, []string{"data_fs", "data_nvme", "data_writable", "data_bound", "data_mounted"}) {
		t.Fatalf("data keys = %v", DataKeys)
	}
	// Both schemas name one stage order, and the key derivation follows the one form
	// this package uses: an anonymous pointer embed or a field with no JSON name is
	// refused rather than answered with a key list that is not the wire's.
	if FactStages != RecordStages {
		t.Fatalf("stage orders diverge: %v vs %v", FactStages, RecordStages)
	}
	if keys := JSONKeys(reflect.TypeFor[struct {
		*Envelope
		Host
	}]()); keys != nil {
		t.Fatalf("an anonymous pointer embed derived %v, want no key set", keys)
	}
	if keys := JSONKeys(reflect.TypeFor[struct {
		Envelope `json:"meta"`
		Host
	}]()); !reflect.DeepEqual(keys, append([]string{"meta"}, HostKeys...)) {
		t.Fatalf("a tagged anonymous struct derived %v", keys)
	}
	if len(data) > 2048 || strings.ContainsAny(string(data), "\n\\") {
		t.Fatalf("record is not compact fixed text: %d bytes", len(data))
	}

	// object is the recorded record with one mutation of its keyed members: the
	// cases below name a place in the schema, not a byte offset in the fixture.
	object := func(mutate func(m map[string]any)) string {
		t.Helper()
		m := map[string]any{}
		if err := json.Unmarshal([]byte(v2Before), &m); err != nil {
			t.Fatal(err)
		}
		mutate(m)
		out, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}
	block := func(m map[string]any, name string) map[string]any {
		b, ok := m[name].(map[string]any)
		if !ok {
			t.Fatalf("%s is not an object", name)
		}
		return b
	}
	for _, tc := range []struct {
		name   string
		data   string
		reason string
	}{
		{"the recorded record", v2Before, ""},
		{"unknown key added to the envelope", strings.Replace(v2Before, `"schema":2,`, `"schema":2,"host_name":"example",`, 1), "record_incomplete"},
		{"key renamed out of the host schema", strings.Replace(v2Before, `"cores":4`, `"host_cores":4`, 1), "record_incomplete"},
		{"unknown key added to the host block", object(func(m map[string]any) { block(m, "host")["cmdline"] = "x" }), "record_incomplete"},
		{"missing host key", object(func(m map[string]any) { delete(block(m, "host"), "swap_in_end") }), "record_incomplete"},
		{"missing data key", object(func(m map[string]any) { delete(block(m, "data"), "data_bound") }), "record_incomplete"},
		{"unknown key added to the data block", object(func(m map[string]any) { block(m, "data")["data_mount"] = "other" }), "record_incomplete"},
		{"another schema", strings.Replace(v2Before, `"schema":2`, `"schema":3`, 1), "record_malformed"},
		{"the V1 schema value", strings.Replace(v2Before, `"schema":2`, `"schema":1`, 1), "record_malformed"},
		{"another stage", strings.Replace(v2Before, `"stage":"before"`, `"stage":"later"`, 1), "record_malformed"},
		{"another run name", strings.Replace(v2Before, `"run":"`+recRun+`"`, `"run":"bench-x"`, 1), "record_malformed"},
		{"number as text", strings.Replace(v2Before, `"cpus":4`, `"cpus":"4"`, 1), "record_malformed"},
		{"number below zero", strings.Replace(v2Before, `"start_unix":1792232100`, `"start_unix":-1`, 1), "record_malformed"},
		{"boolean as text in the data block", object(func(m map[string]any) { block(m, "data")["data_nvme"] = "true" }), "record_malformed"},
		{"null scalar in the host block", object(func(m map[string]any) { block(m, "host")["cpuset_matches"] = nil }), "record_malformed"},
		{"null scalar in the data block", object(func(m map[string]any) { block(m, "data")["data_writable"] = nil }), "record_malformed"},
		{"null host block", object(func(m map[string]any) { m["host"] = nil }), "record_malformed"},
		{"null data block", object(func(m map[string]any) { m["data"] = nil }), "record_malformed"},
		{"host block is not an object", object(func(m map[string]any) { m["host"] = []any{1} }), "record_malformed"},
		{"unknown data filesystem", object(func(m map[string]any) { block(m, "data")["data_fs"] = "/dev/nvme0n1p2" }), "record_malformed"},
		{"xfs config filesystem", object(func(m map[string]any) { block(m, "host")["config_fs"] = ConfigFSXFS }), ""},
		{"xfs data filesystem", object(func(m map[string]any) { block(m, "data")["data_fs"] = ConfigFSXFS }), ""},
		{"unknown config filesystem", object(func(m map[string]any) { block(m, "host")["config_fs"] = "btrfs" }), "record_malformed"},
		{"trailing second object", v2Before + v2Before, "record_malformed"},
		{"truncated", v2Before[:len(v2Before)/2], "record_malformed"},
		{"empty", "", "record_malformed"},
		// A V1 record is not a V2 record, whichever decoder reads it.
		{"a V1 record", v1Before, "record_incomplete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeRecord([]byte(tc.data))
			if tc.reason == "" {
				if err != nil {
					t.Fatalf("DecodeRecord = %v, want valid", err)
				}
				return
			}
			reason, ok := InvalidReason(err)
			if !ok || reason != tc.reason {
				t.Fatalf("verdict = %v, want INVALID %s", err, tc.reason)
			}
		})
	}

	// A duplicate key is neither an unknown nor a missing one: the key set is the
	// same and encoding/json keeps the last value, the answer the V1 decoder records.
	// A later value that is not the schema's does refuse, on its own token.
	dup := strings.Replace(v2Before, `"cpus":4`, `"cpus":4,"cpus":3`, 1)
	if dupRec, err := DecodeRecord([]byte(dup)); err != nil || dupRec.Host.CPUs != 3 {
		t.Fatalf("duplicate key = %d, %v, want the last value", dupRec.Host.CPUs, err)
	}
	dup = strings.Replace(v2Before, `"stage":"before"`, `"stage":"before","stage":"later"`, 1)
	if _, err := DecodeRecord([]byte(dup)); err == nil {
		t.Fatal("a duplicate key whose last value is no stage decoded")
	} else if reason, ok := InvalidReason(err); !ok || reason != "record_malformed" {
		t.Fatalf("duplicate key = %v, want INVALID record_malformed", err)
	}
	// The other direction: the V1 decoder keeps refusing a V2 record, on the token its
	// key set earns.
	if _, err := DecodeFacts([]byte(v2Before)); err == nil {
		t.Fatal("DecodeFacts accepted a V2 record")
	} else if reason, ok := InvalidReason(err); !ok || reason != "record_incomplete" {
		t.Fatalf("a V2 record = %v, want INVALID record_incomplete", err)
	}

	// The set decoder: the three records of one run, in order, each holding its own
	// stage.
	wire := v2Stages()
	recs, err := DecodeRecordSet(wire)
	if err != nil {
		t.Fatalf("the three stages of one run must decode: %v", err)
	}
	for i, stage := range RecordStages {
		rec, err := DecodeRecord(wire[i])
		if err != nil {
			t.Fatalf("%s: %v", stage, err)
		}
		if recs[i].Stage != stage || !reflect.DeepEqual(recs[i], rec) {
			t.Fatalf("record %d = %+v, want the %s record", i, recs[i], stage)
		}
	}
	// A set whose during record is the before record is not a run.
	bad := v2Stages()
	bad[1] = bad[0]
	if _, err := DecodeRecordSet(bad); err == nil {
		t.Fatal("a set holding another stage's record decoded")
	} else if reason, ok := InvalidReason(err); !ok || reason != "record_stage" {
		t.Fatalf("another stage's record = %v, want INVALID record_stage", err)
	}
	// The same 64 KiB bound per record as V1.
	big := append([]byte(v2Before), bytes.Repeat([]byte(" "), MaxFactBytes-len(v2Before)+1)...)
	if _, err := DecodeRecordSet([3][]byte{big, big, big}); err == nil {
		t.Fatal("an oversized record decoded")
	} else if reason, ok := InvalidReason(err); !ok || reason != "record_oversized" {
		t.Fatalf("oversized record = %v, want INVALID record_oversized", err)
	}
}

// Fixed instants for the data eligibility cases: one UTC day, and the first instant
// of the next one. No case reads the wall clock.
const (
	recRun     = "bench-20261017T101500Z"
	recBase    = int64(1792232100) // 2026-10-17T10:15:00Z
	recNextDay = int64(1792281600) // 2026-10-18T00:00:00Z
)

// validRecord is one stage's record of an eligible run: four physical same-NUMA
// cores, no foreign workload, 4 GiB with no container swap, no swap activity, a
// read-only empty ext4 NVMe /config, and the stage's own data evidence bound to the
// run and to the runner's /bench-data directory. It starts from the recorded wire
// fixture and overrides only the stage's window, so a field added to either is
// exercised by both.
func validRecord(stage string) Record {
	r := v2Want()
	r.Stage = stage
	switch stage {
	case StageDuring:
		r.Host.StartUnix, r.Host.EndUnix, r.Host.Observations = recBase+1, recBase+9, 9
	case StageAfter:
		r.Host.StartUnix, r.Host.EndUnix = recBase+10, recBase+10
	}
	return r
}

func validRecords() [3]Record {
	return [3]Record{validRecord(StageBefore), validRecord(StageDuring), validRecord(StageAfter)}
}

// TestDataEligibility: the data rules run first, in their own order, and each disjunct
// of a rule answers that rule's own token. Only a run that passes every one of them
// reaches the host rules, which keep their exact meaning. A set that is not V2 is
// refused by the guard, a record of another stage by the host rules' own stage check,
// and a set with no data evidence at all by the first data rule: an in-process caller
// is never judged by the host rules alone.
func TestDataEligibility(t *testing.T) {
	oneCore := func(r *[3]Record) {
		for i := range r {
			r[i].Host.CPUs, r[i].Host.Cores, r[i].Host.ThreadsPerCoreMax, r[i].Host.SiblingsOutsideSet = 1, 1, 1, 0
		}
	}
	for _, tc := range []struct {
		name   string
		mutate func(r *[3]Record)
		reason string
	}{
		{"data valid and host eligible", func(*[3]Record) {}, ""},
		{"data valid one core same UTC day", oneCore, "topology"},
		{"data valid one core end next UTC day", func(r *[3]Record) { oneCore(r); r[2].Host.EndUnix = recNextDay }, "date_rollover"},
		{"data filesystem: xfs", func(r *[3]Record) {
			for i := range r {
				r[i].Data.FS = ConfigFSXFS
			}
		}, ""},
		{"config filesystem: xfs", func(r *[3]Record) {
			for i := range r {
				r[i].Host.ConfigFS = ConfigFSXFS
			}
		}, ""},
		{"data_filesystem false end next UTC day", func(r *[3]Record) { r[1].Data.NVMe = false; r[2].Host.EndUnix = recNextDay }, "data_filesystem"},
		{"data filesystem: other", func(r *[3]Record) { r[0].Data.FS = ConfigFSOther }, "data_filesystem"},
		{"config filesystem: other", func(r *[3]Record) { r[0].Host.ConfigFS = ConfigFSOther }, "filesystem"},
		{"data filesystem: not nvme", func(r *[3]Record) { r[2].Data.NVMe = false }, "data_filesystem"},
		{"data mount: not mounted", func(r *[3]Record) { r[1].Data.Mounted = false }, "data_mount"},
		{"data mount before the host rules", func(r *[3]Record) { r[0].Data.Mounted = false; r[0].Host.ForeignAllowedMax = 1 }, "data_mount"},
		{"data binding: not bound", func(r *[3]Record) { r[0].Data.Bound = false }, "data_binding"},
		{"data binding: not writable", func(r *[3]Record) { r[2].Data.Writable = false }, "data_binding"},
		{"data binding before the host rules", func(r *[3]Record) { r[1].Data.Writable = false; r[1].Host.SwapInEnd++ }, "data_binding"},
		{"data mount before data binding", func(r *[3]Record) { r[0].Data.Mounted = false; r[0].Data.Bound = false }, "data_mount"},
		{"a later record's filesystem before an earlier record's binding", func(r *[3]Record) {
			r[0].Data.Bound = false
			r[1].Data.FS = ConfigFSOther
		}, "data_filesystem"},
		{"no data evidence at all", func(r *[3]Record) {
			for i := range r {
				r[i].Data = Data{}
			}
		}, "data_filesystem"},
		{"a record holds another stage", func(r *[3]Record) { r[0].Stage = StageAfter }, "record_stage"},
		{"a record of another schema", func(r *[3]Record) { r[1].Schema = FactSchema }, "record_malformed"},
		// The host rules keep their own tokens once the data rules pass.
		{"host swap after the data rules", func(r *[3]Record) { r[1].Host.SwapInEnd++ }, "swap"},
		{"host overlap after the data rules", func(r *[3]Record) { r[0].Host.ForeignAllowedMax = 1 }, "overlap"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recs := validRecords()
			tc.mutate(&recs)
			err := CheckDataEligibility(recRun, recs)
			if tc.reason == "" {
				if err != nil {
					t.Fatalf("eligible: %v", err)
				}
				return
			}
			reason, ok := InvalidReason(err)
			if !ok || reason != tc.reason {
				t.Fatalf("verdict = %v, want INVALID %s", err, tc.reason)
			}
		})
	}
	// The run directory name is part of the identity, and the data rules leave it to
	// the host rules they delegate to.
	if reason, ok := InvalidReason(CheckDataEligibility("bench-20261017T101501Z", validRecords())); !ok || reason != "identity_mismatch" {
		t.Fatalf("another run directory = %v, want INVALID identity_mismatch", reason)
	}

	// The control decodes from the wire: the predicate judges what the runner wrote.
	var datas [3][]byte
	for i, stage := range RecordStages {
		data, err := json.Marshal(validRecord(stage))
		if err != nil {
			t.Fatal(err)
		}
		datas[i] = data
	}
	recs, err := DecodeRecordSet(datas)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckDataEligibility(recRun, recs); err != nil {
		t.Fatalf("decoded control: %v", err)
	}

	// A work directory on another filesystem is a recorded value, not a malformed record: the runner
	// writes data_fs "other" for it, the record decodes, and the data rule answers with its own token.
	other := v2Stages()
	for i := range other {
		other[i] = bytes.Replace(other[i], []byte(`"data_fs":"ext4"`), []byte(`"data_fs":"other"`), 1)
	}
	recs, err = DecodeRecordSet(other)
	if err != nil {
		t.Fatalf("the recorded non-ext4 value must decode: %v", err)
	}
	if reason, ok := InvalidReason(CheckDataEligibility(recRun, recs)); !ok || reason != "data_filesystem" {
		t.Fatalf("data_fs other = %v, want INVALID data_filesystem", reason)
	}
}

// TestImageLocate: the image reader names the run of exactly its own directories.
// An absolute argument is a run only when it is that directory, and a relative one
// is resolved against the working directory — never against the image root — so the
// same directory written relative to the root is not a run and nothing outside the
// image is one.
func TestImageLocate(t *testing.T) {
	im := NewImage(t.TempDir(), 0, nil)
	own := im.RunPath(recRun)
	if run, err := im.Locate(own); err != nil || run != recRun {
		t.Fatalf("the image's own run = %q, %v", run, err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(cwd, own)
	if err != nil {
		t.Fatal(err)
	}
	if run, err := im.Locate(rel); err != nil || run != recRun {
		t.Fatalf("the same run relative to the working directory = %q, %v", run, err)
	}
	for _, notARun := range []string{
		filepath.Join("docs", "spikes", "data", recRun), // relative to the image root, not to the working directory
		recRun,
		filepath.Join(t.TempDir(), recRun),
		im.RunPath("x"),
	} {
		run, err := im.Locate(notARun)
		if reason, ok := InvalidReason(err); run != "" || !ok || reason != "run_location" {
			t.Fatalf("%q = %q, %v, want INVALID run_location", notARun, run, err)
		}
	}
}
