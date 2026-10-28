// Package benchprofile holds the evidence one benchmark run records: the V1 and V2
// record schemas and their strict decoders, the reference-profile eligibility
// predicates and the image and plain-file readers the judges share. A second copy of
// a schema, of a predicate or of the image reader would be a second answer to what an
// eligible run is.
package benchprofile

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"time"
)

// Host evidence of one benchmark run: three small files written by the host
// runner (scripts/dev-run.sh --bench) beside the run's samples directory and
// never visible or writable to the benchmarked child. This package owns their
// schema; the runner's writer and the final judge both follow it. Every field is
// a fixed number, enum or boolean: no task listing, command line, environment,
// path, CPU list or host name is ever recorded, so a record cannot leak any.
const (
	FactSchema    = 1
	RecordSchema  = 2
	MaxFactBytes  = 64 << 10
	maxFactsTotal = 3 * MaxFactBytes

	requiredCPUs       = 4
	RequiredMemory     = 4 << 30
	minDuringSamples   = 2
	maxFactNumber      = 1 << 50
	ConfigFSExt4       = "ext4"
	ConfigFSXFS        = "xfs"
	ConfigFSOther      = "other"
	StageBefore        = "before"
	StageDuring        = "during"
	StageAfter         = "after"
	runNameTimeLayout  = "20060102T150405Z"
	runNamePrefix      = "bench-"
	DateLayout         = "20060102"
	imageIDPrefix      = "sha256:"
	HexSHA256Len       = 64
	MinMemoryAvailKiB  = RequiredMemory >> 10
	FactFileNameSuffix = ".json"
	imageSrc           = "/src"
)

// Envelope is the identity both record versions carry: the schema version, the
// stage, and the run directory name and the image and input manifest hash of the
// runner invocation that measured. V1 writes the five fields among its host
// evidence; V2 gives them their own five keys above the host and data blocks.
type Envelope struct {
	Schema int64  `json:"schema"`
	Stage  string `json:"stage"`
	Run    string `json:"run"`
	Image  string `json:"image"`
	Inputs string `json:"inputs"`
}

// Host is one stage's host observation aggregate. before and after are one
// observation each, during is every observation of the child's lifetime.
type Host struct {
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

// Facts is one V1 record: the envelope and the host evidence in one flat object.
// The shared halves are embedded, so the host field definitions exist once.
type Facts struct {
	Envelope
	Host
}

// Data is one stage's data evidence: the storage the run's own writable
// directory sat on, recorded by the host runner and never visible or writable to
// the benchmarked child. Numbers, enums and booleans only: a path, a device name,
// a mount listing or a command line has no place to go. A boolean is recorded true
// only when it held at every observation of its stage.
type Data struct {
	// FS is the filesystem backing the run's work directory: ext4, xfs or other.
	FS string `json:"data_fs"`
	// NVMe is whether that filesystem's device is NVMe.
	NVMe bool `json:"data_nvme"`
	// Writable is whether the directory was owned by the child's uid with owner
	// write and no group or other write, and bound read-write at /bench-data.
	Writable bool `json:"data_writable"`
	// Bound is whether the writable directory was the one the run wrote: the same
	// device and inode under the same run identity.
	Bound bool `json:"data_bound"`
	// Mounted is whether the directory was and stayed the mount created for it.
	Mounted bool `json:"data_mounted"`
}

// Record is one V2 record: the envelope above the host and data evidence of its
// stage.
type Record struct {
	Envelope
	Host Host `json:"host"`
	Data Data `json:"data"`
}

// invalidError is an INVALID verdict: the evidence cannot support any latency
// claim. Reason is a fixed token, never a value.
type invalidError struct{ reason string }

func (e *invalidError) Error() string { return e.reason }

// Invalid is an INVALID verdict with the reason token the report carries.
func Invalid(reason string) error { return &invalidError{reason} }

// InvalidReason returns the reason token of an INVALID error.
func InvalidReason(err error) (string, bool) {
	if ie, ok := errors.AsType[*invalidError](err); ok {
		return ie.reason, true
	}
	return "", false
}

// JSONKeys are the exact JSON keys of a struct type, derived from its tags so
// the key set and the struct cannot drift. An untagged anonymous struct
// contributes its own keys in place, as encoding/json writes them; an anonymous
// struct with a tag of its own is the one key that tag names. The limit: an
// anonymous pointer to a struct, which encoding/json also inlines, and a field
// with no JSON name are not derived here — the whole key set is then nil, so the
// wire is refused rather than answered with a key list that is not its own.
func JSONKeys(t reflect.Type) []string {
	var keys []string
	for f := range t.Fields() {
		if !f.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" {
			if !f.Anonymous || f.Type.Kind() != reflect.Struct {
				return nil
			}
			keys = append(keys, JSONKeys(f.Type)...)
			continue
		}
		keys = append(keys, name)
	}
	return keys
}

// FactKeys are the exact JSON keys of Facts, RecordKeys of Record and HostKeys
// and DataKeys of the record's two blocks.
var (
	FactKeys   = JSONKeys(reflect.TypeFor[Facts]())
	RecordKeys = JSONKeys(reflect.TypeFor[Record]())
	HostKeys   = JSONKeys(reflect.TypeFor[Host]())
	DataKeys   = JSONKeys(reflect.TypeFor[Data]())
)

// RequireKeys checks that data is one JSON object holding exactly keys, none of
// them null: encoding/json decodes a null into the zero value, which would
// otherwise pass as a recorded 0 or false. what prefixes the reason tokens.
func RequireKeys(data []byte, keys []string, what string) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil || m == nil {
		return Invalid(what + "_malformed")
	}
	if len(m) != len(keys) {
		return Invalid(what + "_incomplete")
	}
	for _, k := range keys {
		v, ok := m[k]
		if !ok {
			return Invalid(what + "_incomplete")
		}
		if bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return Invalid(what + "_malformed")
		}
	}
	return nil
}

// strictDecode is the one decoder body of both schemas: data is one JSON object
// holding exactly keys, none null, no field outside them, nothing after it, and
// with the schema's shape. wire checks a schema's own blocks before the decode,
// so a block whose key set is wrong keeps the reason its key set earns; it may be
// nil. A later strictness change here reaches both schemas, so they cannot answer
// the same bytes differently.
func strictDecode[T any](data []byte, keys []string, wire func() error, shape func(T) error) (T, error) {
	var v T
	if err := RequireKeys(data, keys, "record"); err != nil {
		return v, err
	}
	if wire != nil {
		if err := wire(); err != nil {
			return v, err
		}
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return v, Invalid("record_malformed")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return v, Invalid("record_malformed")
	}
	return v, shape(v)
}

// DecodeFacts strictly decodes one record: a single JSON object holding exactly
// the schema's keys, none null, with the schema's types.
func DecodeFacts(data []byte) (Facts, error) {
	return strictDecode(data, FactKeys, nil, Facts.validShape)
}

// IsLowerHex reports whether s is exactly n lower-case hexadecimal digits.
func IsLowerHex(s string, n int) bool {
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

// ValidRunName reports whether s is bench-<UTC YYYYMMDDTHHMMSSZ> of a real instant.
func ValidRunName(s string) bool {
	rest, ok := strings.CutPrefix(s, runNamePrefix)
	if !ok {
		return false
	}
	_, err := time.Parse(runNameTimeLayout, rest)
	return err == nil
}

// validShape checks the envelope's types, ranges and enums for the schema version
// a record of it must carry.
func (e Envelope) validShape(schema int64) error {
	switch {
	case e.Schema != schema:
		return Invalid("record_malformed")
	case e.Stage != StageBefore && e.Stage != StageDuring && e.Stage != StageAfter:
		return Invalid("record_malformed")
	case !ValidRunName(e.Run):
		return Invalid("record_malformed")
	case !strings.HasPrefix(e.Image, imageIDPrefix) || !IsLowerHex(e.Image[len(imageIDPrefix):], HexSHA256Len):
		return Invalid("record_malformed")
	case !IsLowerHex(e.Inputs, HexSHA256Len):
		return Invalid("record_malformed")
	}
	return nil
}

// validShape checks the host evidence's ranges and enums, not the eligibility
// rules.
func (h Host) validShape() error {
	nums := []int64{
		h.StartUnix, h.EndUnix, h.Observations, h.CPUs, h.Cores, h.ThreadsPerCoreMax, h.SiblingsOutsideSet, h.SiblingRunnableMax,
		h.NUMANodes, h.ForeignAllowedMax, h.ForeignRunnableMax, h.Load1MilliMax, h.MemAvailableKiBMin,
		h.SwapInStart, h.SwapInEnd, h.SwapOutStart, h.SwapOutEnd, h.MemLimitBytes, h.MemSwapLimitBytes,
	}
	for _, n := range nums {
		if n < 0 || n > maxFactNumber {
			return Invalid("record_malformed")
		}
	}
	if !benchFS(h.ConfigFS) && h.ConfigFS != ConfigFSOther {
		return Invalid("record_malformed")
	}
	return nil
}

// benchFS reports whether fs is a filesystem the reference profile measures on: ext4 or XFS. Every other
// filesystem is recorded as ConfigFSOther and stays ineligible.
func benchFS(fs string) bool { return fs == ConfigFSExt4 || fs == ConfigFSXFS }

// validShape checks the data evidence's enums, not the eligibility rules.
func (d Data) validShape() error {
	if !benchFS(d.FS) && d.FS != ConfigFSOther {
		return Invalid("record_malformed")
	}
	return nil
}

// validShape checks a V1 record's types' ranges and enums, not the eligibility
// rules.
func (f Facts) validShape() error {
	if err := f.Envelope.validShape(FactSchema); err != nil {
		return err
	}
	return f.Host.validShape()
}

// validShape checks a V2 record's types' ranges and enums, not the eligibility
// rules.
func (r Record) validShape() error {
	if err := r.Envelope.validShape(RecordSchema); err != nil {
		return err
	}
	if err := r.Host.validShape(); err != nil {
		return err
	}
	return r.Data.validShape()
}

// DecodeRecord strictly decodes one V2 record: a single JSON object holding
// exactly the envelope's keys, none null, with a host block and a data block that
// each hold exactly their own keys, none null. A V1 record is not a V2 record: its
// flat key set is refused as incomplete, as a V2 record is by DecodeFacts.
func DecodeRecord(data []byte) (Record, error) {
	return strictDecode(data, RecordKeys, func() error {
		var blocks struct {
			Host json.RawMessage `json:"host"`
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(data, &blocks); err != nil {
			return Invalid("record_malformed")
		}
		if err := RequireKeys(blocks.Host, HostKeys, "record"); err != nil {
			return err
		}
		return RequireKeys(blocks.Data, DataKeys, "record")
	}, Record.validShape)
}

// ReadRegular reads one regular, non-link file of at most max bytes. Failures are
// INVALID with the reason what+"_missing", "_unreadable", "_nonregular" or
// "_oversized".
func ReadRegular(path string, max int64, what string) ([]byte, os.FileInfo, error) {
	fi, err := os.Lstat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil, nil, Invalid(what + "_missing")
	case err != nil:
		return nil, nil, Invalid(what + "_unreadable")
	case !fi.Mode().IsRegular():
		return nil, nil, Invalid(what + "_nonregular")
	case fi.Size() > max:
		return nil, nil, Invalid(what + "_oversized")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, Invalid(what + "_unreadable")
	}
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	_ = f.Close()
	switch {
	case err != nil:
		return nil, nil, Invalid(what + "_unreadable")
	case int64(len(data)) > max:
		return nil, nil, Invalid(what + "_oversized")
	}
	return data, fi, nil
}

// stages are the three records in time order, the one definition both record
// versions and both set decoders derive their order from. FactStages and
// RecordStages name that order for their schema; neither is a second list.
var stages = [3]string{StageBefore, StageDuring, StageAfter}

// FactStages are the three V1 records in time order.
var FactStages = stages

// decodeSet is the one set-decoder body of both record versions: as many records as
// there are stages, each at most 64 KiB, the three together at most 192 KiB, and each
// holding the stage its place in the set names. decode is the schema's own decoder and
// stageOf reads the stage it decoded, so a later strictness change here reaches both
// versions and they cannot answer the same bytes differently.
func decodeSet[T any](datas [3][]byte, decode func([]byte) (T, error), stageOf func(T) string) ([3]T, error) {
	var out [3]T
	total := 0
	for i, data := range datas {
		total += len(data)
		if len(data) > MaxFactBytes || total > maxFactsTotal {
			return out, Invalid("record_oversized")
		}
		rec, err := decode(data)
		if err != nil {
			return out, err
		}
		if stageOf(rec) != stages[i] {
			return out, Invalid("record_stage")
		}
		out[i] = rec
	}
	return out, nil
}

// DecodeFactSet decodes the three V1 records' bytes: each at most 64 KiB, the three
// together at most 192 KiB, each holding its own stage.
func DecodeFactSet(datas [3][]byte) ([3]Facts, error) {
	return decodeSet(datas, DecodeFacts, func(f Facts) string { return f.Stage })
}

// LoadFacts reads the three records of a directory as plain files. A judge that
// must prove the bytes came from its own image reads them itself, not here.
func LoadFacts(runDir string) ([3]Facts, error) {
	var datas [3][]byte
	for i, stage := range stages {
		data, _, err := ReadRegular(filepath.Join(runDir, stage+FactFileNameSuffix), MaxFactBytes, "record")
		if err != nil {
			return [3]Facts{}, err
		}
		datas[i] = data
	}
	return DecodeFactSet(datas)
}

// RecordStages are the three V2 records in time order: the same three stages as
// the V1 records, in the same order.
var RecordStages = stages

// DecodeRecordSet decodes the three V2 records' bytes: each at most 64 KiB, the three
// together at most 192 KiB, each holding its own stage. A record set carries no
// provenance, so a judge that must prove the bytes came from its own image reads them
// through Image and decodes them here, never from a directory a caller picked.
func DecodeRecordSet(datas [3][]byte) ([3]Record, error) {
	return decodeSet(datas, DecodeRecord, func(r Record) string { return r.Stage })
}

// FactsUTCDate is the UTC calendar date of a record instant.
func FactsUTCDate(unix int64) string {
	return time.Unix(unix, 0).UTC().Format(DateLayout)
}

// CheckEligibility applies the reference-profile host rules to the three V1 records
// of run. It accepts only complete, matching, observed-clean evidence: four
// physical cores of one NUMA node, none of them sharing a core with another
// member (SMT siblings outside the set are allowed), no foreign workload on
// them in any stage, memory 4 GiB with no container swap, free memory of at
// least the limit, no swap-in or swap-out anywhere in the run, a local NVMe
// ext4-or-XFS read-only empty /config and one UTC date from the first to the last
// observation. Observation is not reservation proof: the owner reserves the
// cores, and an observation that cannot show it is INVALID.
//
// It judges V1 records; a caller holding V2 records uses CheckDataEligibility.
func CheckEligibility(run string, recs [3]Facts) error {
	var env [3]Envelope
	var host [3]Host
	for i, rec := range recs {
		env[i], host[i] = rec.Envelope, rec.Host
	}
	return checkHost(run, env, host)
}

// checkHost is the one host-rule body: the stage, identity, window, topology,
// overlap, swap, memory, container and /config rules over three stages' envelope and
// host halves, whichever record version read them. It reads no schema, so it answers
// the same for a V1 record and for the host half of a V2 record.
func checkHost(run string, env [3]Envelope, host [3]Host) error {
	before, during, after := host[0], host[1], host[2]
	for i, stage := range stages {
		if env[i].Stage != stage {
			return Invalid("record_stage")
		}
	}
	for i := range host {
		e := env[i]
		if e.Run != run || e.Run != env[0].Run || e.Image != env[0].Image || e.Inputs != env[0].Inputs {
			return Invalid("identity_mismatch")
		}
	}
	switch {
	case before.EndUnix < before.StartUnix || during.EndUnix < during.StartUnix || after.EndUnix < after.StartUnix:
		return Invalid("order")
	case before.EndUnix > during.StartUnix || during.EndUnix > after.StartUnix:
		return Invalid("order")
	case before.Observations < 1 || during.Observations < minDuringSamples || after.Observations < 1:
		return Invalid("incomplete_sampling")
	case FactsUTCDate(before.StartUnix) != FactsUTCDate(after.EndUnix):
		return Invalid("date_rollover")
	}
	for _, r := range host {
		switch {
		case r.CPUs != requiredCPUs || r.Cores < 1 || r.Cores > r.CPUs || r.ThreadsPerCoreMax < 1:
			return Invalid("topology")
		case r.Cores != r.CPUs:
			// Two members of the set share a physical core: a sibling inside the set. Siblings outside the
			// set (threads_per_core_max above 1, siblings_outside_set above 0) are allowed only while idle.
			return Invalid("smt")
		case r.SiblingRunnableMax != 0:
			// A user thread was running on an SMT sibling outside the set: competing work on a reserved core.
			return Invalid("smt")
		case r.NUMANodes != 1:
			return Invalid("numa")
		case r.CPUs != before.CPUs || r.Cores != before.Cores || r.NUMANodes != before.NUMANodes:
			return Invalid("topology")
		case r.ForeignAllowedMax != 0 || r.ForeignRunnableMax != 0:
			return Invalid("overlap")
		case r.SwapInStart != before.SwapInStart || r.SwapInEnd != before.SwapInStart ||
			r.SwapOutStart != before.SwapOutStart || r.SwapOutEnd != before.SwapOutStart:
			return Invalid("swap")
		case r.MemLimitBytes != RequiredMemory || r.MemSwapLimitBytes != RequiredMemory:
			return Invalid("memory")
		case r.MemAvailableKiBMin < MinMemoryAvailKiB:
			return Invalid("memory")
		case !r.CpusetMatches:
			return Invalid("container")
		case !benchFS(r.ConfigFS) || !r.ConfigNVMe || !r.ConfigEmpty || !r.ConfigReadOnly:
			return Invalid("filesystem")
		}
	}
	return nil
}

// CheckDataEligibility applies the reference-profile's data rules to the three
// records of run and then, unchanged, its host rules. The data rules run first, in
// this order: the run's own writable directory was backed by NVMe ext4 or XFS
// (data_filesystem), it was and stayed the mount created for the run (data_mount),
// and it was the directory the run wrote, writable at /bench-data (data_binding).
// Only when every data rule passes does this apply the host rules, over the three
// host halves, so an ineligible host keeps every reason token it always had.
//
// Data first, because a passing /config never stands in for data evidence: an
// empty read-only ext4-or-XFS NVMe /config says nothing about the storage the run itself
// wrote, so a run on another filesystem, mount or directory is ineligible however
// clean its /config was, and its data fact answers with its own token even when
// the host would also have refused the run. A record set that is not V2 is refused
// here on the token its own decoder gives it (record_malformed), so every rule
// below judges a set of V2 records, never V1 host evidence on its own.
func CheckDataEligibility(run string, recs [3]Record) error {
	var env [3]Envelope
	var host [3]Host
	for i, rec := range recs {
		if rec.Schema != RecordSchema {
			return Invalid("record_malformed")
		}
		env[i], host[i] = rec.Envelope, rec.Host
	}
	for _, rec := range recs {
		if !benchFS(rec.Data.FS) || !rec.Data.NVMe {
			return Invalid("data_filesystem")
		}
	}
	for _, rec := range recs {
		if !rec.Data.Mounted {
			return Invalid("data_mount")
		}
	}
	for _, rec := range recs {
		if !rec.Data.Bound || !rec.Data.Writable {
			return Invalid("data_binding")
		}
	}
	return checkHost(run, env, host)
}

// Image is the immutable image a recorded run must come from: one source root
// whose manifest lists every file it holds with its SHA-256, and the owner every
// one of those files must have. A judge that must prove the bytes came from its
// own image reads them through it; a look-alike the benchmarked child made outside
// the image is not.
type Image struct {
	Root     string // the image's /src: the root its manifest paths start from
	UID      uint32 // the owner every file of the image must have
	Manifest []byte // one "<sha256>  <path under /src>" line per file, sha256sum format
}

// NewImage returns the reader for the image rooted at imageRoot: its files are
// owned by uid, and manifest lists every one of them with its SHA-256.
func NewImage(imageRoot string, uid uint32, manifest []byte) *Image {
	return &Image{Root: imageRoot, UID: uid, Manifest: manifest}
}

// RunPath is where the image holds run: its own docs/spikes/data.
func (im *Image) RunPath(run string) string {
	return filepath.Join(im.Root, "docs", "spikes", "data", run)
}

// Locate maps a run directory argument to its run name: it must be exactly
// docs/spikes/data/bench-<UTC> of the image, nothing else. The argument is
// resolved the way a path always is — against the working directory when it is
// relative — so a relative argument names a run only if it already resolves to the
// image's own directory.
func (im *Image) Locate(runDir string) (string, error) {
	abs, err := filepath.Abs(runDir)
	if err != nil {
		return "", Invalid("run_location")
	}
	run := filepath.Base(abs)
	if !ValidRunName(run) || abs != im.RunPath(run) {
		return "", Invalid("run_location")
	}
	return run, nil
}

// immutable reports whether fi is owned by uid and cannot be written by anyone
// else: no write bit at all, except that a directory that is not itself a record
// parent (strict false) may be sticky, which keeps another user from replacing or
// unlinking an entry it does not own.
func (im *Image) immutable(fi os.FileInfo, strict bool) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != im.UID {
		return false
	}
	if fi.Mode().Perm()&0o222 == 0 {
		return true
	}
	return fi.IsDir() && !strict && fi.Mode()&os.ModeSticky != 0
}

// CheckParents requires every directory that could be used to replace the run's
// files (the run directory, its samples directory and every ancestor up to the
// image root) to be a real, owner-owned, unwritable or sticky directory.
func (im *Image) CheckParents(run string) error {
	rp := im.RunPath(run)
	data := filepath.Dir(rp)
	spikes := filepath.Dir(data)
	docs := filepath.Dir(spikes)
	for _, d := range []struct {
		path             string
		strict, optional bool
	}{{rp, true, false}, {filepath.Join(rp, "samples"), true, true}, {data, false, false}, {spikes, false, false}, {docs, false, false}, {im.Root, false, false}} {
		fi, err := os.Lstat(d.path)
		switch {
		case errors.Is(err, os.ErrNotExist) && d.optional:
			continue
		case errors.Is(err, os.ErrNotExist):
			return Invalid("record_missing")
		case err != nil:
			return Invalid("record_unreadable")
		case !fi.IsDir():
			return Invalid("run_location")
		case !im.immutable(fi, d.strict):
			return Invalid("record_mutable")
		}
	}
	return nil
}

// manifestHas reports whether the image manifest lists path exactly once, with the
// SHA-256 of data (sha256sum format: 64 hex digits, two spaces, the path).
func (im *Image) manifestHas(path string, data []byte) bool {
	want := sha256Hex(data)
	n := 0
	for line := range bytes.Lines(im.Manifest) {
		line = bytes.TrimSuffix(line, []byte("\n"))
		if len(line) > 66 && line[64] == ' ' && line[65] == ' ' && string(line[66:]) == path {
			if string(line[:64]) != want {
				return false
			}
			n++
		}
	}
	return n == 1
}

// ReadImported reads one record of the image: a regular file that cannot be
// written by anyone else and whose bytes are the ones the image's manifest lists
// for its place in /src. A look-alike made outside the image is not.
func (im *Image) ReadImported(path string, max int64, what string) ([]byte, error) {
	data, fi, err := ReadRegular(path, max, what)
	if err != nil {
		return nil, err
	}
	if !im.immutable(fi, true) {
		return nil, Invalid("record_mutable")
	}
	rel, err := filepath.Rel(im.Root, path)
	if err != nil || !im.manifestHas(imageSrc+"/"+filepath.ToSlash(rel), data) {
		return nil, Invalid("image_mismatch")
	}
	return data, nil
}

// sha256Hex is the lower-case hexadecimal SHA-256 of data.
func sha256Hex(data []byte) string {
	s := sha256.Sum256(data)
	return hex.EncodeToString(s[:])
}
