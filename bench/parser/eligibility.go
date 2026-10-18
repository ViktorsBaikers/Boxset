package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"
)

// Host evidence of one benchmark run: three small files written by the host
// runner (scripts/dev-run.sh --bench) beside the run's samples directory and
// never visible or writable to the benchmarked child. This file owns their
// schema; the runner's writer and the final judge both follow it. Every field is
// a fixed number, enum or boolean: no task listing, command line, environment,
// path, CPU list or host name is ever recorded, so a record cannot leak any.
const (
	factsSchema   = 1
	maxFactBytes  = 64 << 10
	maxFactsTotal = 3 * maxFactBytes

	requiredCPUs        = 4
	requiredMemory      = 4 << 30
	minDuringSamples    = 2
	maxFactNumber       = 1 << 50
	configFSExt4        = "ext4"
	configFSOther       = "other"
	stageBefore         = "before"
	stageDuring         = "during"
	stageAfter          = "after"
	runNameTimeLayout   = "20060102T150405Z"
	runNamePrefix       = "bench-"
	factsDateLayout     = "20060102"
	imageIDPrefix       = "sha256:"
	hexSHA256Len        = 64
	minMemoryAvailKiB   = requiredMemory >> 10
	factsFileNameSuffix = ".json"
)

// Facts is one stage's host observation aggregate. before and after are one
// observation each, during is every observation of the child's lifetime.
type Facts struct {
	Schema int64  `json:"schema"`
	Stage  string `json:"stage"`
	// Identity: the run directory name and the image and input manifest hash
	// of the runner invocation that measured.
	Run    string `json:"run"`
	Image  string `json:"image"`
	Inputs string `json:"inputs"`
	// Window of the stage in UNIX seconds and the number of observations in it.
	StartUnix    int64 `json:"start_unix"`
	EndUnix      int64 `json:"end_unix"`
	Observations int64 `json:"observations"`
	// Topology of the reserved set: CPUs counts its logical CPUs, Cores its
	// distinct physical cores, ThreadsPerCoreMax the largest SMT sibling count
	// of any member, SiblingsOutsideSet the SMT siblings of members that are
	// not members, NUMANodes the distinct NUMA nodes.
	CPUs               int64 `json:"cpus"`
	Cores              int64 `json:"cores"`
	ThreadsPerCoreMax  int64 `json:"threads_per_core_max"`
	SiblingsOutsideSet int64 `json:"siblings_outside_set"`
	NUMANodes          int64 `json:"numa_nodes"`
	// SiblingRunnableMax is the most user threads outside the benchmark container that were running on an SMT
	// sibling of a member that is not itself a member, in one observation (no CPU number is recorded).
	SiblingRunnableMax int64 `json:"sibling_runnable_max"`
	// Overlap: the most user threads outside the benchmark container that may
	// run on (ForeignAllowedMax) or were runnable on (ForeignRunnableMax) a
	// member CPU in one observation.
	ForeignAllowedMax  int64 `json:"foreign_allowed_max"`
	ForeignRunnableMax int64 `json:"foreign_runnable_max"`
	// Load and free memory over the stage.
	Load1MilliMax      int64 `json:"load1_milli_max"`
	MemAvailableKiBMin int64 `json:"mem_available_kib_min"`
	// Cumulative host swap-in and swap-out page counters at the first and the
	// last observation of the stage.
	SwapInStart  int64 `json:"swap_in_start"`
	SwapInEnd    int64 `json:"swap_in_end"`
	SwapOutStart int64 `json:"swap_out_start"`
	SwapOutEnd   int64 `json:"swap_out_end"`
	// Container as Docker created it.
	MemLimitBytes     int64 `json:"mem_limit_bytes"`
	MemSwapLimitBytes int64 `json:"memswap_limit_bytes"`
	CpusetMatches     bool  `json:"cpuset_matches"`
	// The read-only /config mount's backing filesystem.
	ConfigFS       string `json:"config_fs"`
	ConfigNVMe     bool   `json:"config_nvme"`
	ConfigEmpty    bool   `json:"config_empty"`
	ConfigReadOnly bool   `json:"config_readonly"`
}

// invalidError is an INVALID verdict: the evidence cannot support any latency
// claim. Reason is a fixed token, never a value.
type invalidError struct{ reason string }

func (e *invalidError) Error() string { return e.reason }

func invalid(reason string) error { return &invalidError{reason} }

// invalidReason returns the reason token of an INVALID error.
func invalidReason(err error) (string, bool) {
	if ie, ok := errors.AsType[*invalidError](err); ok {
		return ie.reason, true
	}
	return "", false
}

// jsonKeys are the exact JSON keys of a struct type, derived from its tags so
// the key set and the struct cannot drift.
func jsonKeys(t reflect.Type) []string {
	var keys []string
	for f := range t.Fields() {
		if !f.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		keys = append(keys, name)
	}
	return keys
}

// factKeys are the exact JSON keys of Facts.
var factKeys = jsonKeys(reflect.TypeFor[Facts]())

// requireKeys checks that data is one JSON object holding exactly keys, none of
// them null: encoding/json decodes a null into the zero value, which would
// otherwise pass as a recorded 0 or false. what prefixes the reason tokens.
func requireKeys(data []byte, keys []string, what string) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil || m == nil {
		return invalid(what + "_malformed")
	}
	if len(m) != len(keys) {
		return invalid(what + "_incomplete")
	}
	for _, k := range keys {
		v, ok := m[k]
		if !ok {
			return invalid(what + "_incomplete")
		}
		if bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return invalid(what + "_malformed")
		}
	}
	return nil
}

// decodeFacts strictly decodes one record: a single JSON object holding exactly
// the schema's keys, none null, with the schema's types.
func decodeFacts(data []byte) (Facts, error) {
	var f Facts
	if err := requireKeys(data, factKeys, "record"); err != nil {
		return f, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return Facts{}, invalid("record_malformed")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Facts{}, invalid("record_malformed")
	}
	if err := f.validShape(); err != nil {
		return Facts{}, err
	}
	return f, nil
}

func isLowerHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := range len(s) {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// validRunName reports whether s is bench-<UTC YYYYMMDDTHHMMSSZ> of a real instant.
func validRunName(s string) bool {
	rest, ok := strings.CutPrefix(s, runNamePrefix)
	if !ok {
		return false
	}
	_, err := time.Parse(runNameTimeLayout, rest)
	return err == nil
}

// validShape checks types' ranges and enums, not the eligibility rules.
func (f Facts) validShape() error {
	nums := []int64{
		f.StartUnix, f.EndUnix, f.Observations, f.CPUs, f.Cores, f.ThreadsPerCoreMax, f.SiblingsOutsideSet, f.SiblingRunnableMax,
		f.NUMANodes, f.ForeignAllowedMax, f.ForeignRunnableMax, f.Load1MilliMax, f.MemAvailableKiBMin,
		f.SwapInStart, f.SwapInEnd, f.SwapOutStart, f.SwapOutEnd, f.MemLimitBytes, f.MemSwapLimitBytes,
	}
	for _, n := range nums {
		if n < 0 || n > maxFactNumber {
			return invalid("record_malformed")
		}
	}
	switch {
	case f.Schema != factsSchema:
		return invalid("record_malformed")
	case f.Stage != stageBefore && f.Stage != stageDuring && f.Stage != stageAfter:
		return invalid("record_malformed")
	case !validRunName(f.Run):
		return invalid("record_malformed")
	case !strings.HasPrefix(f.Image, imageIDPrefix) || !isLowerHex(f.Image[len(imageIDPrefix):], hexSHA256Len):
		return invalid("record_malformed")
	case !isLowerHex(f.Inputs, hexSHA256Len):
		return invalid("record_malformed")
	case f.ConfigFS != configFSExt4 && f.ConfigFS != configFSOther:
		return invalid("record_malformed")
	}
	return nil
}

// readRegular reads one regular, non-link file of at most max bytes. Failures are
// INVALID with the reason what+"_missing", "_unreadable", "_nonregular" or
// "_oversized".
func readRegular(path string, max int64, what string) ([]byte, os.FileInfo, error) {
	fi, err := os.Lstat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil, nil, invalid(what + "_missing")
	case err != nil:
		return nil, nil, invalid(what + "_unreadable")
	case !fi.Mode().IsRegular():
		return nil, nil, invalid(what + "_nonregular")
	case fi.Size() > max:
		return nil, nil, invalid(what + "_oversized")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, invalid(what + "_unreadable")
	}
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	_ = f.Close()
	switch {
	case err != nil:
		return nil, nil, invalid(what + "_unreadable")
	case int64(len(data)) > max:
		return nil, nil, invalid(what + "_oversized")
	}
	return data, fi, nil
}

// factStages are the three records in time order.
var factStages = [3]string{stageBefore, stageDuring, stageAfter}

// decodeFactSet decodes the three records' bytes: each at most 64 KiB, the three
// together at most 192 KiB, each holding its own stage.
func decodeFactSet(datas [3][]byte) ([3]Facts, error) {
	var out [3]Facts
	total := 0
	for i, data := range datas {
		total += len(data)
		if len(data) > maxFactBytes || total > maxFactsTotal {
			return out, invalid("record_oversized")
		}
		rec, err := decodeFacts(data)
		if err != nil {
			return out, err
		}
		if rec.Stage != factStages[i] {
			return out, invalid("record_stage")
		}
		out[i] = rec
	}
	return out, nil
}

// loadFacts reads the three records of a directory as plain files; the judge
// reads them through its image (see loadRun).
func loadFacts(runDir string) ([3]Facts, error) {
	var datas [3][]byte
	for i, stage := range factStages {
		data, _, err := readRegular(filepath.Join(runDir, stage+factsFileNameSuffix), maxFactBytes, "record")
		if err != nil {
			return [3]Facts{}, err
		}
		datas[i] = data
	}
	return decodeFactSet(datas)
}

// factsUTCDate is the UTC calendar date of a record instant.
func factsUTCDate(unix int64) string {
	return time.Unix(unix, 0).UTC().Format(factsDateLayout)
}

// checkEligibility applies the reference-profile rules to the three records of
// run. It accepts only complete, matching, observed-clean evidence: four
// physical cores of one NUMA node, none of them sharing a core with another
// member (SMT siblings outside the set are allowed), no foreign workload on
// them in any stage, memory 4 GiB with no container swap, free memory of at
// least the limit, no swap-in or swap-out anywhere in the run, a local NVMe
// ext4 read-only empty /config and one UTC date from the first to the last
// observation. Observation is not reservation proof: the owner reserves the
// cores, and an observation that cannot show it is INVALID.
func checkEligibility(run string, recs [3]Facts) error {
	before, during, after := recs[0], recs[1], recs[2]
	for i, stage := range [3]string{stageBefore, stageDuring, stageAfter} {
		if recs[i].Stage != stage {
			return invalid("record_stage")
		}
	}
	for _, r := range recs {
		if r.Run != run || r.Run != before.Run || r.Image != before.Image || r.Inputs != before.Inputs {
			return invalid("identity_mismatch")
		}
	}
	switch {
	case before.EndUnix < before.StartUnix || during.EndUnix < during.StartUnix || after.EndUnix < after.StartUnix:
		return invalid("order")
	case before.EndUnix > during.StartUnix || during.EndUnix > after.StartUnix:
		return invalid("order")
	case before.Observations < 1 || during.Observations < minDuringSamples || after.Observations < 1:
		return invalid("incomplete_sampling")
	case factsUTCDate(before.StartUnix) != factsUTCDate(after.EndUnix):
		return invalid("date_rollover")
	}
	for _, r := range recs {
		switch {
		case r.CPUs != requiredCPUs || r.Cores < 1 || r.Cores > r.CPUs || r.ThreadsPerCoreMax < 1:
			return invalid("topology")
		case r.Cores != r.CPUs:
			// Two members of the set share a physical core: a sibling inside the set. Siblings outside the
			// set (threads_per_core_max above 1, siblings_outside_set above 0) are allowed only while idle.
			return invalid("smt")
		case r.SiblingRunnableMax != 0:
			// A user thread was running on an SMT sibling outside the set: competing work on a reserved core.
			return invalid("smt")
		case r.NUMANodes != 1:
			return invalid("numa")
		case r.CPUs != before.CPUs || r.Cores != before.Cores || r.NUMANodes != before.NUMANodes:
			return invalid("topology")
		case r.ForeignAllowedMax != 0 || r.ForeignRunnableMax != 0:
			return invalid("overlap")
		case r.SwapInStart != before.SwapInStart || r.SwapInEnd != before.SwapInStart ||
			r.SwapOutStart != before.SwapOutStart || r.SwapOutEnd != before.SwapOutStart:
			return invalid("swap")
		case r.MemLimitBytes != requiredMemory || r.MemSwapLimitBytes != requiredMemory:
			return invalid("memory")
		case r.MemAvailableKiBMin < minMemoryAvailKiB:
			return invalid("memory")
		case !r.CpusetMatches:
			return invalid("container")
		case r.ConfigFS != configFSExt4 || !r.ConfigNVMe || !r.ConfigEmpty || !r.ConfigReadOnly:
			return invalid("filesystem")
		}
	}
	return nil
}
