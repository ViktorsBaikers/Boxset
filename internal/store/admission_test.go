package store

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The shared contract with scripts/s5-toolchain.sh --admission (DEC-011,
// DEC-012): docs/spikes/s5-version-admission.md holds one go-dependency row per
// go.mod require and per module the main module compiles. This test reads that
// file, go.mod, go.sum and `go list -deps -test ./...` and applies the same
// row rule; go.sum lines outside the compiled set need no row.

type module struct{ path, version string }

func (m module) String() string { return m.path + "@" + m.version }

type require struct {
	module
	indirect bool
}

type goRow struct {
	id, pin, source string
	module
}

type admissionInput struct {
	requires []require
	compiled []module
	sums     map[string]string // "path version" and "path version/go.mod" -> h1: hash
	rows     []goRow
}

var (
	exactRelease = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)
	pseudoSuffix = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+-[0-9]{14}-([0-9a-f]{12})$`)
	commitHex    = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// violations lists every way the rows fail the rule, each naming its entry.
func (in admissionInput) violations() []string {
	var out []string
	rowFor := func(m module) (goRow, bool) {
		for _, r := range in.rows {
			if r.module == m {
				return r, true
			}
		}
		return goRow{}, false
	}
	for _, q := range in.requires {
		if _, ok := rowFor(q.module); !ok {
			out = append(out, fmt.Sprintf("go.mod require %s has no go-dependency row", q.module))
		}
	}
	for _, m := range in.compiled {
		if _, ok := rowFor(m); !ok {
			out = append(out, fmt.Sprintf("compiled module %s has no go-dependency row", m))
		}
	}
	for _, r := range in.rows {
		who := fmt.Sprintf("go-dependency row %s (%s)", r.module, r.id)
		q := slices.IndexFunc(in.requires, func(q require) bool { return q.module == r.module })
		if q < 0 {
			out = append(out, who+": go.mod does not require that module and version")
		}
		if got, ok := in.sums[r.path+" "+r.version]; !ok || got != r.pin {
			out = append(out, fmt.Sprintf("%s: pin %s is not the go.sum h1: line %q", who, r.pin, got))
		}
		src := strings.Fields(r.source)
		if len(src) < 4 || src[0] != "go-proxy" || src[1] != "https://proxy.golang.org" ||
			!strings.HasPrefix(src[2], "origin=https://") || !strings.HasPrefix(src[3], "commit=") {
			out = append(out, who+": source is not go-proxy https://proxy.golang.org origin=<https URL> commit=<40 hex>")
			continue
		}
		commit := strings.TrimPrefix(src[3], "commit=")
		if !commitHex.MatchString(commit) {
			out = append(out, who+": source commit is not 40 lowercase hex")
		}
		switch pseudo := pseudoSuffix.FindStringSubmatch(r.version); {
		case exactRelease.MatchString(r.version):
			if len(src) != 5 || src[4] != "ref=refs/tags/"+r.version {
				out = append(out, who+": an exact release needs source ref=refs/tags/"+r.version)
			}
		case pseudo == nil:
			out = append(out, who+": version is neither an exact vMAJOR.MINOR.PATCH release nor a pseudo-version")
		case q >= 0 && !in.requires[q].indirect:
			out = append(out, who+": a direct require must be an exact vMAJOR.MINOR.PATCH release, not a pseudo-version")
		case len(src) != 4:
			out = append(out, who+": a pseudo-version source carries a ref, want none")
		case !strings.HasPrefix(commit, pseudo[1]):
			out = append(out, fmt.Sprintf("%s: the 12 hex %s are not the prefix of the source commit %s", who, pseudo[1], commit))
		}
	}
	return out
}

func (in admissionInput) clone() admissionInput {
	out := in
	out.requires = slices.Clone(in.requires)
	out.compiled = slices.Clone(in.compiled)
	out.rows = slices.Clone(in.rows)
	out.sums = make(map[string]string, len(in.sums))
	for k, v := range in.sums {
		out.sums[k] = v
	}
	return out
}

func parseGoMod(t *testing.T, src string) []require {
	t.Helper()
	var out []require
	inBlock := false
	for line := range strings.SplitSeq(src, "\n") {
		indirect := false
		if i := strings.Index(line, "//"); i >= 0 {
			comment := strings.Fields(line[i+2:])
			indirect = len(comment) > 0 && (comment[0] == "indirect" || strings.HasPrefix(comment[0], "indirect;"))
			line = line[:i]
		}
		f := strings.Fields(line)
		switch {
		case len(f) == 0:
		case inBlock && len(f) == 1 && f[0] == ")":
			inBlock = false
		case inBlock && len(f) == 2:
			out = append(out, require{module{f[0], f[1]}, indirect})
		case len(f) == 2 && f[0] == "require" && f[1] == "(":
			inBlock = true
		case len(f) == 3 && f[0] == "require":
			out = append(out, require{module{f[1], f[2]}, indirect})
		case inBlock || f[0] == "require":
			t.Fatalf("go.mod line %q is not a require the admission rule understands", line)
		}
	}
	return out
}

func parseGoSum(t *testing.T, src string) map[string]string {
	t.Helper()
	sums := map[string]string{}
	for line := range strings.SplitSeq(src, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if len(f) != 3 || !strings.HasPrefix(f[2], "h1:") {
			t.Fatalf("go.sum line %q is not 'path version h1:hash'", line)
		}
		sums[f[0]+" "+f[1]] = f[2]
	}
	return sums
}

// parseReport returns the go-dependency rows and the committed compile targets.
func parseReport(t *testing.T, src string) (rows []goRow, targets []string) {
	t.Helper()
	for line := range strings.SplitSeq(src, "\n") {
		cells := strings.Split(strings.TrimSuffix(strings.TrimPrefix(line, "| "), " |"), " | ")
		switch {
		case strings.HasPrefix(line, "| CMT-TARGETS | "):
			targets = strings.Fields(cells[2])
		case strings.HasPrefix(line, "| ADM-") && len(cells) == 12 && cells[1] == "go":
			rows = append(rows, goRow{id: cells[0], module: module{cells[3], cells[4]}, pin: cells[5], source: cells[6]})
		}
	}
	return rows, targets
}

// compiledModules is the union of `go list -deps -test ./...` over the host
// default and every committed target with CGO_ENABLED=0.
func compiledModules(t *testing.T, root string, targets []string) []module {
	t.Helper()
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("the go command is required to list compiled modules: %v", err)
	}
	var out []module
	for _, target := range append([]string{""}, targets...) {
		cmd := exec.CommandContext(t.Context(), goBin, "list", "-deps", "-test", "-f", "{{with .Module}}{{if not .Main}}{{.Path}} {{.Version}}{{end}}{{end}}", "./...")
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if goos, goarch, ok := strings.Cut(target, "/"); ok {
			cmd.Env = append(cmd.Env, "GOOS="+goos, "GOARCH="+goarch)
		}
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("go list -deps -test ./... for %q: %v\n%s", target, err, stderr.String())
		}
		for line := range strings.SplitSeq(stdout.String(), "\n") {
			if f := strings.Fields(line); len(f) == 2 {
				if m := (module{f[0], f[1]}); !slices.Contains(out, m) {
					out = append(out, m)
				}
			}
		}
	}
	return out
}

func readRepoFile(t *testing.T, root string, parts ...string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(append([]string{root}, parts...)...))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestAdmissionRowsCoverModuleFiles(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	rows, targets := parseReport(t, readRepoFile(t, root, "docs", "spikes", "s5-version-admission.md"))
	if len(targets) == 0 {
		t.Fatal("the report has no CMT-TARGETS commitment row")
	}
	in := admissionInput{
		requires: parseGoMod(t, readRepoFile(t, root, "go.mod")),
		compiled: compiledModules(t, root, targets),
		sums:     parseGoSum(t, readRepoFile(t, root, "go.sum")),
		rows:     rows,
	}
	t.Logf("go.mod requires=%d compiled modules=%d go.sum lines=%d go-dependency rows=%d targets=%d", len(in.requires), len(in.compiled), len(in.sums), len(in.rows), len(targets))
	if len(in.requires) == 0 || len(in.compiled) == 0 || len(in.sums) == 0 || len(in.rows) == 0 {
		t.Fatal("nothing was checked: a count is zero")
	}
	for _, v := range in.violations() {
		t.Errorf("%s", v)
	}

	direct := slices.IndexFunc(in.requires, func(q require) bool { return !q.indirect })
	row := func(in admissionInput, m module) int {
		return slices.IndexFunc(in.rows, func(r goRow) bool { return r.module == m })
	}
	pseudo := slices.IndexFunc(in.rows, func(r goRow) bool { return pseudoSuffix.MatchString(r.version) })
	if direct < 0 || pseudo < 0 {
		t.Fatalf("the controls need a direct require (index %d) and an indirect pseudo-version row (index %d)", direct, pseudo)
	}
	directMod, pseudoRow := in.requires[direct].module, in.rows[pseudo]
	controls := []struct {
		name, entry, rule string // the violation must name the entry and the broken rule
		mutate            func(*admissionInput)
	}{
		{"one row dropped", directMod.String(), "has no go-dependency row", func(a *admissionInput) {
			a.rows = slices.Delete(a.rows, row(*a, directMod), row(*a, directMod)+1)
		}},
		{"one unlisted require added", "example.invalid/unlisted@v1.0.0", "has no go-dependency row", func(a *admissionInput) {
			a.requires = append(a.requires, require{module: module{"example.invalid/unlisted", "v1.0.0"}})
		}},
		{"one pin flipped", directMod.String(), "is not the go.sum h1: line", func(a *admissionInput) {
			a.rows[row(*a, directMod)].pin = "h1:" + strings.Repeat("A", 43) + "="
		}},
		{"one direct pseudo-version", directMod.path, "a direct require must be an exact", func(a *admissionInput) {
			pv := "v1.0.1-20260101000000-" + strings.Repeat("a", 12)
			i := row(*a, directMod)
			a.sums[directMod.path+" "+pv] = a.rows[i].pin
			for j := range a.compiled {
				if a.compiled[j] == directMod {
					a.compiled[j].version = pv
				}
			}
			a.requires[direct].version = pv
			a.rows[i].version = pv
			a.rows[i].source = "go-proxy https://proxy.golang.org origin=https://example.invalid/x commit=" + strings.Repeat("a", 40)
		}},
		{"one mismatched commit", pseudoRow.path, "are not the prefix of the source commit", func(a *admissionInput) {
			i := row(*a, pseudoRow.module)
			src := strings.Fields(a.rows[i].source)
			commit := strings.TrimPrefix(src[3], "commit=")
			flipped := "0"
			if commit[0] == '0' {
				flipped = "1"
			}
			src[3] = "commit=" + flipped + commit[1:]
			a.rows[i].source = strings.Join(src, " ")
		}},
		{"one compiled module without a row", "example.invalid/compiled@v1.0.0", "compiled module", func(a *admissionInput) {
			a.compiled = append(a.compiled, module{"example.invalid/compiled", "v1.0.0"})
		}},
	}
	for _, c := range controls {
		t.Run(c.name, func(t *testing.T) {
			mutated := in.clone()
			c.mutate(&mutated)
			var hit string
			for _, v := range mutated.violations() {
				if strings.Contains(v, c.entry) && strings.Contains(v, c.rule) {
					hit = v
					break
				}
			}
			if hit == "" {
				t.Fatalf("no violation names %s and the rule %q; got %q", c.entry, c.rule, mutated.violations())
			}
			t.Logf("negative control %q reported: %s", c.name, hit)
		})
	}
}

// A single-line require is read like a block entry: the test feeds parseGoMod a
// serialized go.mod with one direct and one indirect single-line require and one
// block, then drops the single-line direct row and expects that module named.
func TestParseGoModSingleLine(t *testing.T) {
	src := strings.Join([]string{
		"module example.invalid/m",
		"",
		"go 1.27",
		"",
		"require example.invalid/single-direct v1.2.3",
		"",
		"require example.invalid/single-indirect v0.1.0 // indirect",
		"",
		"require (",
		"\texample.invalid/block-direct v1.0.0",
		"\texample.invalid/block-indirect v1.0.1 // indirect",
		")",
		"",
	}, "\n")
	want := []require{
		{module{"example.invalid/single-direct", "v1.2.3"}, false},
		{module{"example.invalid/single-indirect", "v0.1.0"}, true},
		{module{"example.invalid/block-direct", "v1.0.0"}, false},
		{module{"example.invalid/block-indirect", "v1.0.1"}, true},
	}
	got := parseGoMod(t, src)
	if !slices.Equal(got, want) {
		t.Fatalf("parseGoMod = %+v, want %+v", got, want)
	}

	in := admissionInput{requires: got}
	for _, q := range got[1:] {
		in.rows = append(in.rows, goRow{module: q.module})
	}
	missing := "go.mod require example.invalid/single-direct@v1.2.3 has no go-dependency row"
	var rowless []string
	for _, v := range in.violations() {
		if strings.Contains(v, "has no go-dependency row") {
			rowless = append(rowless, v)
		}
	}
	if !slices.Equal(rowless, []string{missing}) {
		t.Errorf("missing-row violations = %q, want exactly %q", rowless, missing)
	}
}

// A single-line pseudo-version goes through the same rule as a block entry: the
// direct one must be an exact release, the indirect one with a matching commit
// prefix and no ref passes, and a single-line indirect require with no row is the
// only missing-row violation.
func TestParseGoModSingleLinePseudoVersions(t *testing.T) {
	const (
		direct   = "example.invalid/pseudo-direct"
		indirect = "example.invalid/pseudo-indirect"
		norow    = "example.invalid/pseudo-norow"
		dv       = "v1.2.4-20240101000000-0123456789ab"
		iv       = "v0.0.0-20240102000000-abcdef012345"
	)
	src := strings.Join([]string{
		"module example.invalid/m",
		"",
		"go 1.27",
		"",
		"require " + direct + " " + dv,
		"",
		"require " + indirect + " " + iv + " // indirect",
		"",
		"require " + norow + " v0.3.0 // indirect",
		"",
	}, "\n")
	reqs := parseGoMod(t, src)
	want := []require{
		{module{direct, dv}, false},
		{module{indirect, iv}, true},
		{module{norow, "v0.3.0"}, true},
	}
	if !slices.Equal(reqs, want) {
		t.Fatalf("parseGoMod = %+v, want %+v", reqs, want)
	}

	in := admissionInput{requires: reqs, sums: map[string]string{}}
	for _, c := range []struct{ m module; commit string }{
		{reqs[0].module, "0123456789ab" + strings.Repeat("0", 28)},
		{reqs[1].module, "abcdef012345" + strings.Repeat("0", 28)},
	} {
		in.sums[c.m.path+" "+c.m.version] = "h1:" + c.m.path
		in.rows = append(in.rows, goRow{
			id:     c.m.path,
			pin:    "h1:" + c.m.path,
			source: "go-proxy https://proxy.golang.org origin=https://example.invalid/r commit=" + c.commit,
			module: c.m,
		})
	}
	got := in.violations()
	if len(got) != 2 {
		t.Fatalf("violations = %q, want one for the direct pseudo-version and one missing row", got)
	}
	var sawDirect, sawMissing bool
	for _, v := range got {
		switch {
		case strings.Contains(v, direct+"@"+dv) && strings.Contains(v, "must be an exact vMAJOR.MINOR.PATCH release"):
			sawDirect = true
		case v == "go.mod require "+norow+"@v0.3.0 has no go-dependency row":
			sawMissing = true
		}
	}
	if !sawDirect || !sawMissing {
		t.Errorf("violations = %q, want the direct pseudo-version %s@%s named with the exact-release rule and the missing row for %s@v0.3.0", got, direct, dv, norow)
	}
}
