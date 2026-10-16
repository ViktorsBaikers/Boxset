package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if os.Getenv("REDACT_TEST_MAIN") == "1" {
		main()
		return
	}
	os.Exit(m.Run())
}

// redact drives the real entry point with literal arguments, an explicit
// environment and raw input bytes.
func redact(args []string, env []string, input []byte) (status int, out []byte) {
	var stdout bytes.Buffer
	status = run(args, env, bytes.NewReader(input), &stdout)
	return status, stdout.Bytes()
}

func cap64k(extra ...string) []string { return append([]string{"-cap", "65536"}, extra...) }

func TestSelectedValuesAreMaskedWhateverTheirLengthOrShape(t *testing.T) {
	big := strings.Repeat("k", 65536)
	cases := []struct {
		name  string
		value string
		input string
		want  string
	}{
		{"one byte", "z", "a z b", "a [REDACTED] b"},
		{"three bytes", "zq7", "a zq7 b", "a [REDACTED] b"},
		{"four bytes", "zq7X", "a zq7X b", "a [REDACTED] b"},
		{"multiline", "line1\nline2", "a line1\nline2 b", "a [REDACTED] b"},
		{"65536 bytes", big, "<" + big + ">", "<[REDACTED]>"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The cap is the larger of raw and emitted size so the maximum-length
			// value is judged by masking, not by the 64 KiB raw cap it cannot fit under.
			limit := max(len(tc.input), len(tc.want))
			status, out := redact([]string{"-cap", fmt.Sprint(limit)}, []string{"BOXSET_TEST_SECRET=" + tc.value}, []byte(tc.input))
			if status != 0 || string(out) != tc.want {
				t.Fatalf("status %d output %q, want 0 and %q", status, out, tc.want)
			}
		})
	}
}

func TestSelectionByNameAndLimits(t *testing.T) {
	for _, name := range []string{"x_secret_y", "MyToken", "DB_PASSWORD", "passwd", "AWS_CREDENTIALS", "stripe_api_key", "APIKEY", "SSH_PRIVATE_KEY"} {
		status, out := redact(cap64k(), []string{name + "=hunter22"}, []byte("a hunter22 b"))
		if status != 0 || string(out) != "a [REDACTED] b" {
			t.Fatalf("name %s: status %d output %q", name, status, out)
		}
	}
	status, out := redact(cap64k(), []string{"PATH=hunter22", "EMPTY_TOKEN="}, []byte("a hunter22 b"))
	if status != 0 || string(out) != "a hunter22 b" {
		t.Fatalf("unselected names: status %d output %q", status, out)
	}

	env := func(n int) []string {
		var e []string
		for i := range n {
			e = append(e, fmt.Sprintf("S%02d_TOKEN=value-%02d", i, i))
		}
		return e
	}
	if status, _ := redact([]string{"-check"}, env(64), nil); status != 0 {
		t.Fatalf("64 values: status %d, want 0", status)
	}
	if status, out := redact(cap64k(), env(65), []byte("x")); status != 2 || len(out) != 0 {
		t.Fatalf("65th value: status %d with %d output bytes, want 2 and none", status, len(out))
	}
	value := func(n int) string { return strings.Repeat("v", n) }
	if status, _ := redact([]string{"-check"}, []string{"A_TOKEN=" + value(65536)}, nil); status != 0 {
		t.Fatalf("65536-byte value: status %d, want 0", status)
	}
	if status, _ := redact([]string{"-check"}, []string{"A_TOKEN=" + value(65537)}, nil); status != 2 {
		t.Fatalf("65537-byte value: status %d, want 2", status)
	}
	var sixteen, seventeen []string
	for i := range 17 {
		kv := fmt.Sprintf("T%02d_TOKEN=%s", i, strings.Repeat(string(rune('a'+i)), 65536))
		if i < 16 {
			sixteen = append(sixteen, kv)
		}
		seventeen = append(seventeen, kv)
	}
	if status, _ := redact([]string{"-check"}, sixteen, nil); status != 0 {
		t.Fatalf("exactly 1 MiB total: status %d, want 0", status)
	}
	if status, _ := redact([]string{"-check"}, seventeen, nil); status != 2 {
		t.Fatalf("1 MiB + 65536 total: status %d, want 2", status)
	}
}

func TestUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"-cap", "10", "-tail", "10"},
		{"-check", "-cap", "10"},
		{"-raw", "5"},
		{"-tail", "10", "-raw", "5"},
		{"-cap", "x"},
		{"-bogus"},
		{"-cap", "10", "extra"},
		{"-clock", "-check"},
		{"-cap", "10", "-clock"},
		{"-clock", "-raw", "5"},
		{"-clock", "x"},
		{"-clock", "-clock"},
		{"-clock=false"},
	} {
		if status, _ := redact(args, nil, []byte("x")); status != 2 {
			t.Fatalf("args %q: status %d, want 2", args, status)
		}
	}
}

func TestOverlapsAndMarkerLookalikes(t *testing.T) {
	cases := []struct {
		name  string
		env   []string
		input string
		want  string
	}{
		{"marker lookalikes", []string{"A_TOKEN=REDACTED", "B_TOKEN=[REDACTED]", "C_TOKEN=ED]"}, "1[REDACTED]2REDACTED3ED]4", "1[REDACTED]2[REDACTED]3[REDACTED]4"},
		{"overlapping values form one run", []string{"A_TOKEN=abc", "B_TOKEN=bcd"}, "xabcdy", "x[REDACTED]y"},
		{"contained value", []string{"A_TOKEN=abcdef", "B_TOKEN=cd"}, "1abcdef2", "1[REDACTED]2"},
		{"repeated and duplicate values", []string{"A_TOKEN=dup", "B_TOKEN=dup"}, "dupdup dup", "[REDACTED] [REDACTED]"},
		{"adjacent occurrences are one run", []string{"A_TOKEN=ab"}, "ababab-ab", "[REDACTED]-[REDACTED]"},
		{"partial prefixes stay visible", []string{"A_TOKEN=abcd"}, "abc abcX abcd", "abc abcX [REDACTED]"},
		{"unterminated at EOF", []string{"A_TOKEN=tail-secret"}, "pre tail-secret", "pre [REDACTED]"},
		{"output is never rescanned", []string{"A_TOKEN=RED"}, "RED", "[REDACTED]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, out := redact(cap64k(), tc.env, []byte(tc.input))
			if status != 0 || string(out) != tc.want {
				t.Fatalf("status %d output %q, want 0 and %q", status, out, tc.want)
			}
		})
	}
}

type splitReader struct {
	parts [][]byte
}

func (r *splitReader) Read(p []byte) (int, error) {
	if len(r.parts) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.parts[0])
	r.parts[0] = r.parts[0][n:]
	if len(r.parts[0]) == 0 {
		r.parts = r.parts[1:]
	}
	return n, nil
}

func TestSecretSplitAtEveryReadBoundary(t *testing.T) {
	input := []byte("pre zq-secret-7X post zq-secret-7X")
	env := []string{"A_TOKEN=zq-secret-7X"}
	for i := 0; i <= len(input); i++ {
		var out bytes.Buffer
		r := &splitReader{parts: [][]byte{input[:i], input[i:]}}
		if status := run(cap64k(), env, r, &out); status != 0 || out.String() != "pre [REDACTED] post [REDACTED]" {
			t.Fatalf("split at %d: status %d output %q", i, status, out.String())
		}
	}
	var out bytes.Buffer
	var parts [][]byte
	for _, c := range input {
		parts = append(parts, []byte{c})
	}
	if status := run(cap64k(), env, &splitReader{parts: parts}, &out); status != 0 || out.String() != "pre [REDACTED] post [REDACTED]" {
		t.Fatalf("one byte per read: status %d output %q", status, out.String())
	}
}

func TestURLsAreRedactedWithoutAnyEnvironment(t *testing.T) {
	cases := []struct{ name, input, want string }{
		{"userinfo query fragment", "https://u:p@h/x?token=t&k=v#f=g", "https://[REDACTED]@h/x?token=[REDACTED]&k=[REDACTED]#f=[REDACTED]"},
		{"other scheme", "ftp://user:pw@host/file", "ftp://[REDACTED]@host/file"},
		{"unterminated at EOF", "see https://u:p@h", "see https://[REDACTED]@h"},
		{"ended by quote", `"https://a:b@h/p?x=1"`, `"https://[REDACTED]@h/p?x=[REDACTED]"`},
		{"ended by newline then plain text", "https://a:b@h?x=1\nnext=1", "https://[REDACTED]@h?x=[REDACTED]\nnext=1"},
		{"no userinfo", "http://example.test/a/b", "http://example.test/a/b"},
		{"empty value stays empty", "http://h/p?a=&b=2", "http://h/p?a=&b=[REDACTED]"},
		{"fragment only", "http://h#a=1&b=2", "http://h#a=[REDACTED]&b=[REDACTED]"},
		{"no letter in scheme", "1://u:p@h", "1://u:p@h"},
		{"scheme starting after a separator", "x(git+ssh://u:p@h/r)", "x(git+ssh://[REDACTED]@h/r)"},
		{"last at sign wins", "http://a@b@c/", "http://[REDACTED]@c/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, out := redact(cap64k(), nil, []byte(tc.input))
			if status != 0 || string(out) != tc.want {
				t.Fatalf("status %d output %q, want 0 and %q", status, out, tc.want)
			}
		})
	}
}

func TestURLChunkedByteByByte(t *testing.T) {
	input := "go https://u:p@h/x?token=t&k=v#f=g end"
	var parts [][]byte
	for i := range len(input) {
		parts = append(parts, []byte{input[i]})
	}
	var out bytes.Buffer
	if status := run(cap64k(), nil, &splitReader{parts: parts}, &out); status != 0 ||
		out.String() != "go https://[REDACTED]@h/x?token=[REDACTED]&k=[REDACTED]#f=[REDACTED] end" {
		t.Fatalf("status %d output %q", status, out.String())
	}
}

func TestUndelimitedAuthorityIsMaskedAtTheLimit(t *testing.T) {
	long := strings.Repeat("a", 1025)
	status, out := redact(cap64k(), nil, []byte("https://"+long+"\n"))
	if status != 0 || string(out) != "https://[REDACTED]\n" {
		t.Fatalf("1025-byte authority: status %d output %q", status, out)
	}
	status, out = redact(cap64k(), nil, []byte("https://"+long+"/p?k=v"))
	if status != 0 || string(out) != "https://[REDACTED]/p?k=[REDACTED]" {
		t.Fatalf("1025-byte authority with path: status %d output %q", status, out)
	}
	ok := strings.Repeat("a", 1024)
	status, out = redact(cap64k(), nil, []byte("https://"+ok+"\n"))
	if status != 0 || string(out) != "https://"+ok+"\n" {
		t.Fatalf("1024-byte authority was altered: status %d", status)
	}
}

func TestNULAndNonUTF8BytesAreCountedAndPassed(t *testing.T) {
	input := []byte("a\x00b\xffc\xc3\x28d")
	status, out := redact(cap64k("-raw", "8"), nil, input)
	if status != 0 || !bytes.Equal(out, input) {
		t.Fatalf("status %d output %q, want 0 and the input", status, out)
	}
	status, out = redact(cap64k(), []string{"A_TOKEN=b\xffc"}, input)
	if status != 0 || string(out) != "a\x00[REDACTED]\xc3\x28d" {
		t.Fatalf("covered binary value: status %d output %q", status, out)
	}
}

func TestRawAndEmittedCaps(t *testing.T) {
	status, out := redact(cap64k(), nil, bytes.Repeat([]byte("a"), 65536))
	if status != 0 || len(out) != 65536 {
		t.Fatalf("65536 raw bytes: status %d with %d output bytes, want 0 and 65536", status, len(out))
	}
	status, out = redact(cap64k(), nil, bytes.Repeat([]byte("a"), 65537))
	if status != 3 || len(out) != 65536 {
		t.Fatalf("65537 raw bytes: status %d with %d output bytes, want 3 and 65536", status, len(out))
	}
	status, out = redact(cap64k(), nil, bytes.Repeat([]byte{0}, 65537))
	if status != 3 || len(out) > 65536 {
		t.Fatalf("65537 NUL bytes: status %d with %d output bytes, want 3 and at most 65536", status, len(out))
	}
	status, out = redact(cap64k(), nil, bytes.Repeat([]byte{0}, 65536))
	if status != 0 || len(out) != 65536 {
		t.Fatalf("65536 NUL bytes: status %d with %d output bytes", status, len(out))
	}
	// One-byte secret: 65,536 raw bytes expand to 360,448 emitted bytes.
	expand := bytes.Repeat([]byte("a-"), 32768)
	status, out = redact(cap64k(), []string{"A_TOKEN=a"}, expand)
	if status != 3 || len(out) > 65536 {
		t.Fatalf("expansion: status %d with %d output bytes, want 3 and at most 65536", status, len(out))
	}
	if !bytes.HasPrefix(out, []byte("[REDACTED]-[REDACTED]-")) {
		t.Fatalf("expansion output began %q", out[:30])
	}
	// Emitted length exactly at the cap passes: 100 pairs of an 10-byte marker plus "-".
	exact := bytes.Repeat([]byte("a-"), 100)
	status, out = redact([]string{"-cap", "1100"}, []string{"A_TOKEN=a"}, exact)
	if status != 0 || len(out) != 1100 {
		t.Fatalf("emitted exactly at the cap: status %d with %d bytes, want 0 and 1100", status, len(out))
	}
	status, out = redact([]string{"-cap", "1099"}, []string{"A_TOKEN=a"}, exact)
	if status != 3 || len(out) != 1099 {
		t.Fatalf("emitted cap+1: status %d with %d bytes, want 3 and 1099", status, len(out))
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("no space left on device") }

func TestWriteErrorExitsFour(t *testing.T) {
	for _, args := range [][]string{cap64k(), {"-tail", "10"}} {
		if status := run(args, nil, strings.NewReader("hello"), failWriter{}); status != 4 {
			t.Fatalf("args %q: status %d, want 4", args, status)
		}
	}
}

func TestRawCountIsCheckedBeforeRedaction(t *testing.T) {
	cases := []struct {
		name  string
		env   []string
		input string
		want  string
	}{
		{"selected value expands from 7 to 10 bytes", []string{"A_TOKEN=ADM-003"}, "ADM-003", "[REDACTED]"},
		{"18-byte userinfo shrinks", nil, "https://aaaaaaaaa:bbbbbbbb@h", "https://[REDACTED]@h"},
		{"plain", nil, "plain text", "plain text"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := fmt.Sprint(len(tc.input))
			status, out := redact(cap64k("-raw", s), tc.env, []byte(tc.input))
			if status != 0 || string(out) != tc.want {
				t.Fatalf("status %d output %q, want 0 and %q", status, out, tc.want)
			}
			for _, wrong := range []int{len(tc.input) - 1, len(tc.input) + 1} {
				if status, _ := redact(cap64k("-raw", fmt.Sprint(wrong)), tc.env, []byte(tc.input)); status != 5 {
					t.Fatalf("-raw %d for %d raw bytes: status %d, want 5", wrong, len(tc.input), status)
				}
			}
		})
	}
	if got := len("ADM-003"); got != 7 {
		t.Fatalf("fixture value is %d bytes, want 7", got)
	}
	if got := len("aaaaaaaaa:bbbbbbbb"); got != 18 {
		t.Fatalf("fixture userinfo is %d bytes, want 18", got)
	}
}

func TestTailKeepsTheLastRedactedBytes(t *testing.T) {
	input := strings.Repeat("x", 90) + "0123456789"
	status, out := redact([]string{"-tail", "10"}, nil, []byte(input))
	if status != 0 || string(out) != "[dev-run: 90 earlier bytes omitted]\n0123456789" {
		t.Fatalf("status %d output %q", status, out)
	}
	status, out = redact([]string{"-tail", "100"}, nil, []byte(input))
	if status != 0 || string(out) != input {
		t.Fatalf("untruncated tail: status %d output %q", status, out)
	}
	status, out = redact([]string{"-tail", "8"}, []string{"A_TOKEN=secret"}, []byte("aaaaaaaaaa secret zz"))
	if status != 0 || string(out) != "[dev-run: 16 earlier bytes omitted]\nCTED] zz" {
		t.Fatalf("redacted tail: status %d output %q", status, out)
	}
	// Constant memory: a large stream is accepted without a raw cap.
	big := io.LimitReader(zeroReader{}, 5<<20)
	var got bytes.Buffer
	if status := run([]string{"-tail", "16"}, nil, big, &got); status != 0 || got.Len() != len("[dev-run: 5242864 earlier bytes omitted]\n")+16 {
		t.Fatalf("large tail: status %d with %d bytes", status, got.Len())
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func TestBinaryEntryPoint(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-cap", "100", "-raw", "4")
	cmd.Env = []string{"REDACT_TEST_MAIN=1", "BOXSET_TEST_SECRET=zz"}
	cmd.Stdin = strings.NewReader("azzb")
	out, err := cmd.Output()
	if err != nil || string(out) != "a[REDACTED]b" {
		t.Fatalf("binary entry: output %q error %v", out, err)
	}
	cmd = exec.Command(os.Args[0], "-cap", "100", "-raw", "5")
	cmd.Env = []string{"REDACT_TEST_MAIN=1"}
	cmd.Stdin = strings.NewReader("azzb")
	var ee *exec.ExitError
	if err := cmd.Run(); !errors.As(err, &ee) || ee.ExitCode() != 5 {
		t.Fatalf("binary entry raw mismatch: %v, want exit 5", err)
	}
}

// A selected value that overlaps URL syntax must not hide the URL: both
// decisions are made on the original bytes and united before replacement.
func TestSelectedValuesInsideURLsDoNotHideThem(t *testing.T) {
	const url = "https://u:p@h/x?k=v"
	cases := []struct{ name, value, input, want string }{
		{"scheme", "https", url, "[REDACTED]://[REDACTED]@h/x?k=[REDACTED]"},
		{"colon", ":", url, "https[REDACTED]//[REDACTED]@h/x?k=[REDACTED]"},
		{"scheme separator", "://", url, "https[REDACTED]@h/x?k=[REDACTED]"},
		{"at sign", "@", url, "https://[REDACTED]h/x?k=[REDACTED]"},
		{"question mark", "?", url, "https://[REDACTED]@h/x[REDACTED]k=[REDACTED]"},
		{"equals", "=", url, "https://[REDACTED]@h/x?k[REDACTED]"},
		{"value straddling userinfo end", "p@h", url, "https://[REDACTED]/x?k=[REDACTED]"},
		{"scheme with fragment", "https", "see https://a:b@h#f=g.", "see [REDACTED]://[REDACTED]@h#f=[REDACTED]"},
		{"scheme with 1025-byte authority", "https", "https://" + strings.Repeat("a", 1025) + "\n", "[REDACTED]://[REDACTED]\n"},
		{"selected value inside a masked authority", "aaaa", "https://" + strings.Repeat("a", 1025) + "\n", "https://[REDACTED]\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := []string{"A_TOKEN=" + tc.value}
			status, out := redact(cap64k(), env, []byte(tc.input))
			if status != 0 || string(out) != tc.want {
				t.Fatalf("status %d output %q, want 0 and %q", status, out, tc.want)
			}
			// Every read boundary and one byte per read give the same bytes.
			if len(tc.input) < 200 {
				for i := 0; i <= len(tc.input); i++ {
					var split bytes.Buffer
					r := &splitReader{parts: [][]byte{[]byte(tc.input[:i]), []byte(tc.input[i:])}}
					if status := run(cap64k(), env, r, &split); status != 0 || split.String() != tc.want {
						t.Fatalf("split at %d: status %d output %q", i, status, split.String())
					}
				}
			}
			var parts [][]byte
			for i := range len(tc.input) {
				parts = append(parts, []byte{tc.input[i]})
			}
			var one bytes.Buffer
			if status := run(cap64k(), env, &splitReader{parts: parts}, &one); status != 0 || one.String() != tc.want {
				t.Fatalf("one byte per read: status %d output %q", status, one.String())
			}
		})
	}
}

func TestSelectedSchemeURLUnderCapAndTail(t *testing.T) {
	const url = "https://u:p@h/x?k=v"
	const want = "[REDACTED]://[REDACTED]@h/x?k=[REDACTED]" // 40 bytes
	env := []string{"A_TOKEN=https"}
	if status, out := redact([]string{"-cap", "40"}, env, []byte(url)); status != 0 || string(out) != want {
		t.Fatalf("emitted exactly at the cap: status %d output %q", status, out)
	}
	if status, out := redact([]string{"-cap", "39"}, env, []byte(url)); status != 3 || string(out) != want[:39] {
		t.Fatalf("emitted cap+1: status %d output %q", status, out)
	}
	if status, out := redact([]string{"-tail", "12"}, env, []byte(url)); status != 0 || string(out) != "[dev-run: 28 earlier bytes omitted]\nk=[REDACTED]" {
		t.Fatalf("tail: status %d output %q", status, out)
	}
	if _, out := redact([]string{"-tail", "100"}, env, []byte(url)); bytes.Contains(out, []byte("u:p")) {
		t.Fatalf("userinfo survived: %q", out)
	}
}

// errReader yields its data, then a read error.
type errReader struct{ data []byte }

func (r *errReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, errors.New("input/output error")
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

func TestReadErrorExitsOneWithSafeOutput(t *testing.T) {
	for _, args := range [][]string{cap64k(), {"-tail", "10"}} {
		var out bytes.Buffer
		if status := run(args, nil, &errReader{}, &out); status != 1 || out.Len() != 0 {
			t.Fatalf("args %q immediate read error: status %d with %d bytes, want 1 and none", args, status, out.Len())
		}
		out.Reset()
		if status := run(args, nil, &errReader{data: []byte("hello")}, &out); status != 1 || out.String() != "hello" {
			t.Fatalf("args %q bytes then read error: status %d output %q, want 1 and hello", args, status, out.String())
		}
		// A partly read secret is held back, never emitted raw.
		out.Reset()
		env := []string{"A_TOKEN=TOKENVALUE"}
		if status := run(args, env, &errReader{data: []byte("xTOKEN")}, &out); status != 1 || strings.Contains(out.String(), "TOKEN") {
			t.Fatalf("args %q secret prefix then read error: status %d output %q", args, status, out.String())
		}
	}
}

// The error path emits exactly the bytes already decided: a secret prefix still
// inside the holdback is never written, and tail mode keeps its bound and
// omitted-count line when the decided output is longer than the tail.
func TestReadErrorEmitsOnlyDecidedBytesBeyondTheTail(t *testing.T) {
	env := []string{"A_TOKEN=SECRET12"}
	input := []byte(strings.Repeat("a", 50) + "xSEC")
	// 54 bytes read, the longest value is 8: the last 7 stay held back, so 47 bytes are decided.
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"cap", cap64k(), strings.Repeat("a", 47)},
		{"tail", []string{"-tail", "10"}, "[dev-run: 37 earlier bytes omitted]\n" + strings.Repeat("a", 10)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			if status := run(tc.args, env, &errReader{data: input}, &out); status != 1 || out.String() != tc.want {
				t.Fatalf("status %d output %q, want 1 and %q", status, out.String(), tc.want)
			}
		})
	}
}

// failIfRead fails the test the moment anything reads it.
type failIfRead struct{ t *testing.T }

func (r failIfRead) Read([]byte) (int, error) {
	r.t.Error("the clock mode must not read standard input")
	return 0, io.EOF
}

var clockText = regexp.MustCompile(`^(0|[1-9][0-9]{0,8})\.[0-9]{2}\n$`)

func centis(t *testing.T, text string) int {
	t.Helper()
	whole, frac, _ := strings.Cut(strings.TrimSuffix(text, "\n"), ".")
	s, e1 := strconv.Atoi(whole)
	f, e2 := strconv.Atoi(frac)
	if e1 != nil || e2 != nil {
		t.Fatalf("unparseable clock text %q", text)
	}
	return s*100 + f
}

func TestClockModeOutput(t *testing.T) {
	var env []string
	for i := range 65 { // a selection that would exit 2 is never evaluated
		env = append(env, fmt.Sprintf("BOXSET_TEST_SECRET_%d=v%d", i, i))
	}
	first := func() string {
		var out bytes.Buffer
		if status := run([]string{"-clock"}, env, failIfRead{t}, &out); status != 0 || !clockText.MatchString(out.String()) {
			t.Fatalf("status %d output %q, want 0 and S.CC plus LF", status, out.String())
		}
		return out.String()
	}
	a := first()
	b := first()
	if centis(t, b) < centis(t, a) {
		t.Fatalf("clock went backward: %q then %q", a, b)
	}
	time.Sleep(30 * time.Millisecond)
	if c := first(); centis(t, c)-centis(t, b) < 2 {
		t.Fatalf("clock advanced %d centiseconds over 30 ms, want at least 2 (%q then %q)", centis(t, c)-centis(t, b), b, c)
	}
}

func TestClockFormatsAndBounds(t *testing.T) {
	if got, ok := formatClock(5, 999999999); !ok || got != "5.99\n" {
		t.Fatalf("formatClock(5, 999999999) = %q, %v, want the floored 5.99", got, ok)
	}
	if got, ok := formatClock(0, 0); !ok || got != "0.00\n" {
		t.Fatalf("formatClock(0, 0) = %q, %v", got, ok)
	}
	if got, ok := formatClock(999999999, 0); !ok || got != "999999999.00\n" {
		t.Fatalf("formatClock(999999999, 0) = %q, %v", got, ok)
	}
	for _, in := range [][2]int64{{1000000000, 0}, {-1, 0}, {1, -1}, {1, 1000000000}} {
		if got, ok := formatClock(in[0], in[1]); ok || got != "" {
			t.Fatalf("formatClock(%d, %d) = %q, %v, want it refused", in[0], in[1], got, ok)
		}
	}
}

type shortWriter struct{}

func (shortWriter) Write([]byte) (int, error) { return 0, errors.New("closed") }

func TestClockFailuresFailClosed(t *testing.T) {
	for name, read := range map[string]func() (int64, int64, error){
		"source error":  func() (int64, int64, error) { return 5, 1, errors.New("no clock") },
		"sec too big":   func() (int64, int64, error) { return 1000000000, 0, nil },
		"sec negative":  func() (int64, int64, error) { return -1, 0, nil },
		"nsec negative": func() (int64, int64, error) { return 1, -1, nil },
		"nsec too big":  func() (int64, int64, error) { return 1, 1000000000, nil },
	} {
		var out bytes.Buffer
		if status := clockMode(&out, read); status != 6 || out.Len() != 0 {
			t.Fatalf("%s: status %d with %d bytes, want 6 and none", name, status, out.Len())
		}
	}
	if status := clockMode(shortWriter{}, func() (int64, int64, error) { return 1, 0, nil }); status != 4 {
		t.Fatalf("failing writer: status %d, want 4", status)
	}
}

func TestClockBinaryEntryPoint(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-clock")
	cmd.Env = []string{"REDACT_TEST_MAIN=1"}
	out, err := cmd.Output()
	if err != nil || !clockText.Match(out) {
		t.Fatalf("binary clock: output %q error %v", out, err)
	}
	cmd = exec.Command(os.Args[0], "-clock", "-check")
	cmd.Env = []string{"REDACT_TEST_MAIN=1"}
	out, err = cmd.Output()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 2 || len(out) != 0 {
		t.Fatalf("binary clock with a second flag: output %q error %v, want exit 2 and no output", out, err)
	}
}

// One immutable selection serves every stream: two streams and a tail through
// the same automaton are redacted alike.
func TestOneSelectionServesEveryStream(t *testing.T) {
	values, ok := selectValues([]string{"BOXSET_TEST_SECRET=hunter22"})
	if !ok {
		t.Fatal("selection refused")
	}
	ac := newAutomaton(values)
	for _, o := range []streamOpts{{capN: 100, raw: -1, tail: -1}, {capN: -1, raw: -1, tail: 100}, {capN: 100, raw: 12, tail: -1}} {
		var out bytes.Buffer
		if status := redactStream(ac, o, strings.NewReader("a hunter22 b"), &out); status != 0 || out.String() != "a [REDACTED] b" {
			t.Fatalf("%+v: status %d output %q", o, status, out.String())
		}
	}
}
