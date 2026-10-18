package main

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

const (
	testRun    = "bench-20261017T101500Z"
	testImage  = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testInputs = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

var testBase = time.Date(2026, 10, 17, 10, 15, 0, 0, time.UTC).Unix()

// validFacts is an independent serialized control: four physical same-NUMA cores
// without SMT siblings, no foreign workload, 4 GiB with no container swap, no swap
// activity, a read-only empty ext4 NVMe /config and one UTC date.
func validFacts(stage string) Facts {
	f := Facts{
		Schema: factsSchema, Stage: stage, Run: testRun, Image: testImage, Inputs: testInputs,
		StartUnix: testBase, EndUnix: testBase, Observations: 1,
		CPUs: 4, Cores: 4, ThreadsPerCoreMax: 2, SiblingsOutsideSet: 4, NUMANodes: 1,
		Load1MilliMax: 20, MemAvailableKiBMin: 16 << 20,
		SwapInStart: 7, SwapInEnd: 7, SwapOutStart: 9, SwapOutEnd: 9,
		MemLimitBytes: requiredMemory, MemSwapLimitBytes: requiredMemory, CpusetMatches: true,
		ConfigFS: configFSExt4, ConfigNVMe: true, ConfigEmpty: true, ConfigReadOnly: true,
	}
	switch stage {
	case stageDuring:
		f.StartUnix, f.EndUnix, f.Observations = testBase+1, testBase+9, 9
	case stageAfter:
		f.StartUnix, f.EndUnix = testBase+10, testBase+10
	}
	return f
}

func validRecs() [3]Facts {
	return [3]Facts{validFacts(stageBefore), validFacts(stageDuring), validFacts(stageAfter)}
}

// writeRecs serializes records into dir the way the judge reads them.
func writeRecs(t *testing.T, dir string, recs [3]Facts) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i, stage := range []string{stageBefore, stageDuring, stageAfter} {
		data, err := json.Marshal(recs[i])
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, stage+".json"), data)
	}
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func wantInvalid(t *testing.T, err error, reason string) {
	t.Helper()
	got, ok := invalidReason(err)
	if !ok || got != reason {
		t.Fatalf("verdict = %v, want INVALID %s", err, reason)
	}
}

func TestEligibilityValidControlIsValid(t *testing.T) {
	dir := filepath.Join(t.TempDir(), testRun)
	writeRecs(t, dir, validRecs())
	recs, err := loadFacts(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkEligibility(testRun, recs); err != nil {
		t.Fatalf("valid control: %v", err)
	}
	if !reflect.DeepEqual(recs, validRecs()) {
		t.Fatalf("round trip changed the records: %+v", recs)
	}
}

func TestEligibilityRejectsEveryBreach(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(r *[3]Facts)
		reason string
	}{
		{"three cores", func(r *[3]Facts) { r[1].CPUs, r[1].Cores = 3, 3 }, "topology"},
		{"five cores", func(r *[3]Facts) { r[0].CPUs, r[0].Cores = 5, 5 }, "topology"},
		{"logical cpus are not cores", func(r *[3]Facts) { r[2].Cores = 2 }, "smt"},
		{"two members share a core", func(r *[3]Facts) { r[1].Cores = 3 }, "smt"},
		{"cores unknown", func(r *[3]Facts) { r[0].Cores = 0 }, "topology"},
		{"threads per core unknown", func(r *[3]Facts) { r[1].ThreadsPerCoreMax = 0 }, "topology"},
		{"more cores than cpus", func(r *[3]Facts) { r[1].Cores = 5 }, "topology"},
		{"cross numa", func(r *[3]Facts) { r[1].NUMANodes = 2 }, "numa"},
		{"unknown numa", func(r *[3]Facts) { r[0].NUMANodes = 0 }, "numa"},
		{"foreign thread allowed before", func(r *[3]Facts) { r[0].ForeignAllowedMax = 1 }, "overlap"},
		{"foreign thread allowed during", func(r *[3]Facts) { r[1].ForeignAllowedMax = 3 }, "overlap"},
		{"foreign thread runnable after", func(r *[3]Facts) { r[2].ForeignRunnableMax = 1 }, "overlap"},
		{"busy external sibling before", func(r *[3]Facts) { r[0].SiblingRunnableMax = 1 }, "smt"},
		{"busy external sibling during", func(r *[3]Facts) { r[1].SiblingRunnableMax = 2 }, "smt"},
		{"busy external sibling after", func(r *[3]Facts) { r[2].SiblingRunnableMax = 1 }, "smt"},
		{"swap in during a stage", func(r *[3]Facts) { r[1].SwapInEnd++ }, "swap"},
		{"swap out during a stage", func(r *[3]Facts) { r[0].SwapOutEnd++ }, "swap"},
		{"swap between before and during", func(r *[3]Facts) {
			r[1].SwapInStart++
			r[1].SwapInEnd++
		}, "swap"},
		{"swap after", func(r *[3]Facts) {
			r[2].SwapOutStart += 5
			r[2].SwapOutEnd += 5
		}, "swap"},
		{"memory not 4g", func(r *[3]Facts) { r[0].MemLimitBytes = 8 << 30 }, "memory"},
		{"container swap allowed", func(r *[3]Facts) { r[1].MemSwapLimitBytes = 8 << 30 }, "memory"},
		{"container swap unlimited", func(r *[3]Facts) { r[1].MemSwapLimitBytes = 0 }, "memory"},
		{"free memory below the limit", func(r *[3]Facts) { r[1].MemAvailableKiBMin = minMemoryAvailKiB - 1 }, "memory"},
		{"cpuset not applied", func(r *[3]Facts) { r[0].CpusetMatches = false }, "container"},
		{"config not ext4", func(r *[3]Facts) { r[0].ConfigFS = configFSOther }, "filesystem"},
		{"config not nvme", func(r *[3]Facts) { r[1].ConfigNVMe = false }, "filesystem"},
		{"config not empty", func(r *[3]Facts) { r[2].ConfigEmpty = false }, "filesystem"},
		{"config not empty during only", func(r *[3]Facts) { r[1].ConfigEmpty = false }, "filesystem"},
		{"config not empty before only", func(r *[3]Facts) { r[0].ConfigEmpty = false }, "filesystem"},
		{"config writable", func(r *[3]Facts) { r[0].ConfigReadOnly = false }, "filesystem"},
		{"run identity", func(r *[3]Facts) { r[1].Run = "bench-20261017T101501Z" }, "identity_mismatch"},
		{"image identity", func(r *[3]Facts) { r[2].Image = "sha256:" + strings.Repeat("c", 64) }, "identity_mismatch"},
		{"input identity", func(r *[3]Facts) { r[0].Inputs = strings.Repeat("d", 64) }, "identity_mismatch"},
		{"after-state record swapped", func(r *[3]Facts) { r[2].Stage = stageBefore }, "record_stage"},
		{"after before during", func(r *[3]Facts) { r[2].StartUnix, r[2].EndUnix = testBase, testBase }, "order"},
		{"window ends before it starts", func(r *[3]Facts) { r[1].EndUnix = r[1].StartUnix - 1 }, "order"},
		{"one during observation", func(r *[3]Facts) { r[1].Observations = 1 }, "incomplete_sampling"},
		{"no before observation", func(r *[3]Facts) { r[0].Observations = 0 }, "incomplete_sampling"},
		{"date rollover", func(r *[3]Facts) {
			day := time.Date(2026, 10, 17, 23, 59, 59, 0, time.UTC).Unix()
			r[0].StartUnix, r[0].EndUnix = day, day
			r[1].StartUnix, r[1].EndUnix = day, day+1
			r[2].StartUnix, r[2].EndUnix = day+1, day+2
		}, "date_rollover"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recs := validRecs()
			tc.mutate(&recs)
			wantInvalid(t, checkEligibility(testRun, recs), tc.reason)
		})
	}
	// The directory name is part of the identity.
	wantInvalid(t, checkEligibility("bench-20261017T101501Z", validRecs()), "identity_mismatch")
}

func TestEligibilityRecordBoundaries(t *testing.T) {
	good, err := json.Marshal(validFacts(stageBefore))
	if err != nil {
		t.Fatal(err)
	}
	pad := func(n int) []byte {
		return append(append([]byte{}, good...), bytes.Repeat([]byte(" "), n-len(good))...)
	}
	obj := func(mutate func(m map[string]any)) []byte {
		m := map[string]any{}
		if err := json.Unmarshal(good, &m); err != nil {
			t.Fatal(err)
		}
		mutate(m)
		out, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	for _, tc := range []struct {
		name   string
		data   []byte
		reason string // empty: valid
	}{
		{"exactly 64 KiB", pad(maxFactBytes), ""},
		{"one byte over 64 KiB", pad(maxFactBytes + 1), "record_oversized"},
		{"empty", nil, "record_malformed"},
		{"not an object", []byte(`[1,2,3]`), "record_malformed"},
		{"trailing second object", append(append([]byte{}, good...), good...), "record_malformed"},
		{"truncated", good[:len(good)/2], "record_malformed"},
		{"missing key", obj(func(m map[string]any) { delete(m, "swap_in_end") }), "record_incomplete"},
		{"hostname key", obj(func(m map[string]any) { m["host"] = "example" }), "record_incomplete"},
		{"command line key replaces a key", obj(func(m map[string]any) {
			delete(m, "cores")
			m["cmdline"] = "x"
		}), "record_incomplete"},
		{"number as string", obj(func(m map[string]any) { m["cpus"] = "4" }), "record_malformed"},
		{"fraction", obj(func(m map[string]any) { m["cpus"] = 4.5 }), "record_malformed"},
		{"negative", obj(func(m map[string]any) { m["swap_in_end"] = -1 }), "record_malformed"},
		{"huge", obj(func(m map[string]any) { m["observations"] = 1e18 }), "record_malformed"},
		{"null number", obj(func(m map[string]any) { m["cpus"] = nil }), "record_malformed"},
		{"null boolean", obj(func(m map[string]any) { m["cpuset_matches"] = nil }), "record_malformed"},
		{"null string", obj(func(m map[string]any) { m["image"] = nil }), "record_malformed"},
		{"unknown schema", obj(func(m map[string]any) { m["schema"] = 2 }), "record_malformed"},
		{"unknown stage", obj(func(m map[string]any) { m["stage"] = "later" }), "record_malformed"},
		{"path as run", obj(func(m map[string]any) { m["run"] = "../bench-20261017T101500Z" }), "record_malformed"},
		{"impossible run date", obj(func(m map[string]any) { m["run"] = "bench-20261317T101500Z" }), "record_malformed"},
		{"short image", obj(func(m map[string]any) { m["image"] = "sha256:abc" }), "record_malformed"},
		{"upper-case inputs", obj(func(m map[string]any) { m["inputs"] = strings.Repeat("B", 64) }), "record_malformed"},
		{"free-form filesystem", obj(func(m map[string]any) { m["config_fs"] = "/dev/nvme0n1p2" }), "record_malformed"},
		{"deep nesting", []byte(strings.Repeat("[", 500) + strings.Repeat("]", 500)), "record_malformed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), testRun)
			writeRecs(t, dir, validRecs())
			writeFile(t, filepath.Join(dir, "before.json"), tc.data)
			_, err := loadFacts(dir)
			if tc.reason == "" {
				if err != nil {
					t.Fatalf("loadFacts = %v, want valid", err)
				}
				return
			}
			wantInvalid(t, err, tc.reason)
		})
	}
}

func TestEligibilityRecordFileKinds(t *testing.T) {
	fresh := func() string {
		dir := filepath.Join(t.TempDir(), testRun)
		writeRecs(t, dir, validRecs())
		return dir
	}
	dir := fresh()
	if err := os.Remove(filepath.Join(dir, "after.json")); err != nil {
		t.Fatal(err)
	}
	_, err := loadFacts(dir)
	wantInvalid(t, err, "record_missing")

	dir = fresh()
	target := filepath.Join(t.TempDir(), "target.json")
	data, _ := json.Marshal(validFacts(stageDuring))
	writeFile(t, target, data)
	if err := os.Remove(filepath.Join(dir, "during.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "during.json")); err != nil {
		t.Fatal(err)
	}
	_, err = loadFacts(dir)
	wantInvalid(t, err, "record_nonregular")

	dir = fresh()
	if err := os.Remove(filepath.Join(dir, "before.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "before.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err = loadFacts(dir)
	wantInvalid(t, err, "record_nonregular")

	// A record that holds another stage is not the stage's record.
	dir = fresh()
	data, _ = json.Marshal(validFacts(stageAfter))
	writeFile(t, filepath.Join(dir, "during.json"), data)
	_, err = loadFacts(dir)
	wantInvalid(t, err, "record_stage")
}

// Records carry numbers, enums and booleans only: every string field is one of the
// five fixed-shape identities, so no path, CPU list, command line or host name has
// a place to go.
func TestEligibilityRecordsCarryNoHostText(t *testing.T) {
	allowed := map[string]bool{"stage": true, "run": true, "image": true, "inputs": true, "config_fs": true}
	for field := range reflect.TypeFor[Facts]().Fields() {
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		switch field.Type.Kind() {
		case reflect.Int64, reflect.Bool:
		case reflect.String:
			if !allowed[name] {
				t.Errorf("string field %s is not a fixed identity or enum", name)
			}
		default:
			t.Errorf("field %s has kind %s", name, field.Type.Kind())
		}
	}
	data, err := json.Marshal(validFacts(stageBefore))
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > 2048 || strings.ContainsAny(string(data), "\n\\") {
		t.Fatalf("record is not compact fixed text: %d bytes", len(data))
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(data, &keys); err != nil || len(keys) != len(factKeys) {
		t.Fatalf("keys = %d, want %d (%v)", len(keys), len(factKeys), err)
	}
}

// The final judge is the next runner invocation's process: a record that is not in
// its image, or is not eligible, is INVALID, exit 3, and never prints the pass line.
func TestEligibilityJudgeExitsInvalid(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(t *testing.T, env *judgeEnv)
		reason string
	}{
		{"run absent from the image", func(t *testing.T, env *judgeEnv) {}, "record_missing"},
		{"overlap", func(t *testing.T, env *judgeEnv) {
			recs := validRecs()
			recs[1].ForeignAllowedMax = 2
			publishFiles(t, env, testRun, recFiles(t, recs))
		}, "overlap"},
		{"config not empty during only (before and after empty)", func(t *testing.T, env *judgeEnv) {
			recs := validRecs()
			recs[1].ConfigEmpty = false
			publishFiles(t, env, testRun, recFiles(t, recs))
		}, "filesystem"},
		{"valid records without samples", func(t *testing.T, env *judgeEnv) {
			publishFiles(t, env, testRun, recFiles(t, validRecs()))
		}, "samples_missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := testEnv(t)
			tc.setup(t, env)
			wantJudged(t, env, testRun, tc.reason)
		})
	}
}

// A required number or boolean that is JSON null is INVALID, not a recorded zero or
// false: through the final judge, exit 3, never the pass line. Every key of the
// schema, in every stage's record, is tried with null and with absence.
func TestEligibilityNullAndAbsentRequiredScalarsAreInvalid(t *testing.T) {
	for _, key := range factKeys {
		for _, stage := range factStages {
			for _, mode := range []string{"null", "absent"} {
				t.Run(stage+" "+key+" "+mode, func(t *testing.T) {
					env := testEnv(t)
					files := recFiles(t, validRecs())
					var m map[string]any
					if err := json.Unmarshal(files[stage+".json"], &m); err != nil {
						t.Fatal(err)
					}
					reason := "record_malformed"
					if mode == "null" {
						m[key] = nil
					} else {
						delete(m, key)
						reason = "record_incomplete"
					}
					data, err := json.Marshal(m)
					if err != nil {
						t.Fatal(err)
					}
					files[stage+".json"] = data
					publishFiles(t, env, testRun, files)
					wantJudged(t, env, testRun, reason)
					if _, err := decodeFacts(data); err == nil {
						t.Fatal("decodeFacts accepted it")
					}
				})
			}
		}
	}
}
