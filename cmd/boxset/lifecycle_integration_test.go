//go:build integration && unix

package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ViktorsBaikers/Boxset/internal/child"
	"github.com/ViktorsBaikers/Boxset/internal/install"
	"github.com/ViktorsBaikers/Boxset/internal/secret"
)

const nativeChildFixture = `
	if len(args)==2 && args[0]=="__crash-proof" {
		if child.PrepareManagedOwner()!=nil {return 3}
		if _,err:=startCrashMonitor(args[1]);err!=nil {return 3}
		_,_=os.Stdout.WriteString("CRASH-DRAIN-READY\n")
		var trigger [1]byte
		if _,err:=os.Stdin.Read(trigger[:]);err!=nil {return 3}
		panic(strings.Repeat("apikey=synthetic-native-crash-secret\n",300000))
	}
	if len(args)==1 && args[0]=="__managed-proof" {
		if child.PrepareManagedOwner()!=nil {return 3}
		self,err:=os.Executable();if err!=nil{return 3}
		result,err:=child.Run(context.Background(),child.Spec{Path:self,Args:[]string{"version"},Timeout:5*time.Second,AttemptID:child.AttemptID{1}})
		if err!=nil || result.Exit!=0 || result.Completion.State!=child.TreeQuiescent {return 3}
		_,_=os.Stdout.WriteString("MANAGED-COMMAND-PASS tree_quiescent\n")
		return 0
	}
`

type crashByteCount struct{ total int64 }

func (c *crashByteCount) Write(raw []byte) (int, error) {
	c.total += int64(len(raw))
	return len(raw), nil
}

const diagnosticBarrierFixture = `
func fixtureAck(suffix string,value any) {
 self,err:=os.Executable();if err!=nil {os.Exit(3)}
 raw,err:=json.Marshal(value);if err!=nil {os.Exit(3)}
 name:=self+suffix+".tmp"
 if os.WriteFile(name,raw,0600)!=nil || os.Rename(name,self+suffix)!=nil {os.Exit(3)}
}
func fixturePause(args []string) {
 self,err:=os.Executable();if err!=nil {os.Exit(3)}
 if len(args)!=4 || args[0]!=childEntry || args[3]!="diagnostic" {return}
 if _,err:=os.Stat(self+".pause");err!=nil {return}
 sentinel:=[]byte("bootstrap-sentinel");var pin runtime.Pinner;pin.Pin(&sentinel[0]);defer pin.Unpin()
 dumpable,err:=unix.PrctlRetInt(unix.PR_GET_DUMPABLE,0,0,0,0);if err!=nil {os.Exit(3)}
 fixtureAck(".paused",struct{PID int;Address uintptr;Dumpable int}{os.Getpid(),uintptr(unsafe.Pointer(&sentinel[0])),dumpable})
 if unix.Kill(os.Getpid(),unix.SIGSTOP)!=nil || string(sentinel)!="bootstrap-sentinel" {os.Exit(3)}
}
func fixtureObserver(phase string,fds []int) {
 if phase!="diagnostic-protected" {return}
 fixtureAck(".protected",fds)
 self,_:=os.Executable();if _,err:=os.Stat(self+".refuse_readiness");err==nil {os.Exit(0)}
 if unix.Kill(os.Getpid(),unix.SIGSTOP)!=nil {os.Exit(3)}
}
`

func childFixtureBinary(t *testing.T, policyUnavailable bool, restart ...bool) string {
	t.Helper()
	dir := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	anchor := "func run(args []string, stdout, stderr io.Writer) int {"
	if strings.Count(string(raw), anchor) != 1 {
		t.Fatal("command fixture entry anchor changed")
	}
	source := strings.Replace(string(raw), anchor, anchor+nativeChildFixture, 1)
	intercept := len(restart) > 0 && restart[0]
	if intercept {
		anchor := "proc, err := child.StartCrashMonitor(context.Background(), dir, read)"
		addition := `self,_:=os.Executable(); readFD,writeFD:=0,0
 readRaw,_:=read.SyscallConn();_ = readRaw.Control(func(fd uintptr){readFD=int(fd)})
 writeRaw,_:=write.SyscallConn();_ = writeRaw.Control(func(fd uintptr){writeFD=int(fd)})
 raw:=[]byte(fmt.Sprintf("[%d,%d,%d]",os.Getpid(),readFD,writeFD))
 if os.WriteFile(self+".owner_fds.tmp",raw,0600)!=nil || os.Rename(self+".owner_fds.tmp",self+".owner_fds")!=nil {return nil,errors.New("fixture_ack")}
 `
		if strings.Count(source, anchor) != 1 {
			t.Fatal("crash pipe fixture anchor changed")
		}
		source = strings.Replace(source, anchor, addition+anchor, 1)
		t.Logf("FIXTURE-MAIN-DIFF owner-fd acknowledgement before %q:\n%s", anchor, addition)
	}
	t.Logf("FIXTURE-MAIN original_sha256=%x replacement_sha256=%x insertion_after=%q\n%s", sha256.Sum256(raw), sha256.Sum256([]byte(source)), anchor, nativeChildFixture)
	mainCopy := filepath.Join(dir, "main.go")
	if err := os.WriteFile(mainCopy, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	replacements := map[string]string{filepath.Join(cwd, "main.go"): mainCopy}
	if policyUnavailable || intercept {
		path := filepath.Clean(filepath.Join(cwd, "../../internal/child/guardian_linux.go"))
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		original := append([]byte(nil), raw...)
		if policyUnavailable {
			anchor := regexp.MustCompile(`func readManagedPolicy\(\) \(\[\]byte,\s*error\) \{`)
			if len(anchor.FindAllIndex(raw, -1)) != 1 {
				t.Fatal("managed policy fixture anchor changed")
			}
			raw = anchor.ReplaceAll(raw, []byte("func readManagedPolicy() ([]byte,error) {\n return nil, ErrJobUnavailable\n"))
		}
		if intercept {
			anchor := regexp.MustCompile(`return enter\(args,\s*nil\)`)
			if len(anchor.FindAllIndex(raw, -1)) != 1 {
				t.Fatal("guardian entry fixture anchor changed")
			}
			raw = anchor.ReplaceAll(raw, []byte("fixturePause(args); return enter(args, fixtureObserver)"))
			raw = append(raw, []byte(diagnosticBarrierFixture)...)
			t.Logf("FIXTURE-GUARDIAN-DIFF replace return enter(args,nil) with fixturePause(args); return enter(args,fixtureObserver); append:\n%s", diagnosticBarrierFixture)
		}
		t.Logf("FIXTURE-GUARDIAN original_sha256=%x replacement_sha256=%x policy_unavailable=%v", sha256.Sum256(original), sha256.Sum256(raw), policyUnavailable)
		copy := filepath.Join(dir, "guardian_linux.go")
		if err := os.WriteFile(copy, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		replacements[path] = copy
	}
	overlay, err := json.Marshal(struct{ Replace map[string]string }{replacements})
	if err != nil {
		t.Fatal(err)
	}
	overlayPath := filepath.Join(dir, "overlay.json")
	t.Logf("FIXTURE-OVERLAY %s", overlay)
	if err := os.WriteFile(overlayPath, overlay, 0o600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "boxset")
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-overlay", overlayPath, "-o", output, ".")
	cmd.Env = []string{"PATH=/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin", "CGO_ENABLED=0", "GOENV=off", "GOTOOLCHAIN=local", "GOTELEMETRY=off", "GOPROXY=file:///usr/local/share/boxset/deps/go/cache/download"}
	for _, key := range []string{"GOCACHE", "GOMODCACHE", "GOPATH", "HOME"} {
		if value := os.Getenv(key); value != "" {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("native fixture build: %v %s", err, out)
	}
	built, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("FIXTURE-BINARY sha256=%x", sha256.Sum256(built))
	return output
}

func TestChildGuardianEntry(t *testing.T) {
	for _, args := range [][]string{{"__boxset-child"}, {"__boxset-child", "1", "3", "invalid"}, {"__boxset-child", "2", "3", "managed"}, {"__boxset-child", "1", "3", "managed"}, {"crash-monitor", t.TempDir()}} {
		cmd := exec.Command(binary, args...)
		cmd.Env = []string{"BOXSET_CHILD=1"}
		cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
		if err := cmd.Run(); err == nil {
			t.Fatalf("malformed/direct guardian invocation succeeded: %q", args[0])
		}
	}
	fixture := childFixtureBinary(t, false)
	cmd := exec.Command(fixture, "__managed-proof")
	cmd.Env = []string{}
	if out, err := cmd.CombinedOutput(); err != nil || !bytes.Contains(out, []byte("MANAGED-COMMAND-PASS tree_quiescent")) {
		t.Fatalf("actual guardian registration: %v %s", err, out)
	}
	t.Run("diagnostic_readiness_refusal", func(t *testing.T) {
		fixture := childFixtureBinary(t, false, true)
		if err := os.WriteFile(fixture+".refuse_readiness", []byte("1"), 0o600); err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		cmd := exec.Command(fixture, "__crash-proof", dir)
		cmd.Env = []string{}
		if out, err := cmd.CombinedOutput(); err == nil || bytes.Contains(out, []byte("CRASH-DRAIN-READY")) {
			t.Fatalf("failed diagnostic readiness installed crash output: %v %s", err, out)
		}
		if _, err := os.Stat(filepath.Join(dir, "logs/crash.log")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("unready diagnostic wrote a crash record")
		}
	})
}

func TestChildOwnerPreparation(t *testing.T) {
	original, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("UNMODIFIED-COMMAND sha256=%x control=serve_readiness_dumpability_shutdown", sha256.Sum256(original))
	s := start(t, t.TempDir())
	s.waitReady(t)
	if _, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/1", s.cmd.Process.Pid)); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("actual serve owner is dumpable: %v", err)
	}
	terminate(t, s)
	fixture := childFixtureBinary(t, true)
	cmd := exec.Command(fixture, "__managed-proof")
	cmd.Env = []string{}
	if out, err := cmd.CombinedOutput(); err == nil || bytes.Contains(out, []byte("MANAGED-COMMAND-PASS")) {
		t.Fatalf("unavailable managed policy executed target: %v %s", err, out)
	}
	dir := t.TempDir()
	server := &proc{port: freePort(t), done: make(chan struct{})}
	server.cmd = exec.Command(fixture, "serve")
	server.cmd.Env = []string{"BOXSET_DATA_DIR=" + dir, "BOXSET_PORT=" + server.port}
	server.cmd.Stderr = &server.stderr
	if err := server.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { server.err = server.cmd.Wait(); close(server.done) }()
	t.Cleanup(func() {
		select {
		case <-server.done:
		default:
			_ = server.cmd.Process.Kill()
			<-server.done
		}
	})
	server.waitReady(t)
	terminate(t, server)
	t.Run("restart_with_unknown_attempt", testDiagnosticRestartUnknown)
}

func restartAck(t *testing.T, path string) []byte {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if raw, err := os.ReadFile(path); err == nil && len(raw) > 0 {
			return raw
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatalf("restart acknowledgement missing: %s", filepath.Base(path))
		}
	}
}

func testDiagnosticRestartUnknown(t *testing.T) {
	fixture := childFixtureBinary(t, false, true)
	dir := t.TempDir()
	path := filepath.Join(dir, "old-attempt")
	helper := filepath.Join(dir, "child.test")
	build := exec.CommandContext(t.Context(), "go", "test", "-c", "-o", helper, "../../internal/child")
	build.Env = []string{"PATH=/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin", "CGO_ENABLED=0", "GOENV=off", "GOTOOLCHAIN=local", "GOTELEMETRY=off", "GOPROXY=file:///usr/local/share/boxset/deps/go/cache/download"}
	for _, key := range []string{"GOCACHE", "GOMODCACHE", "GOPATH", "HOME"} {
		if value := os.Getenv(key); value != "" {
			build.Env = append(build.Env, key+"="+value)
		}
	}
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("old target fixture build: %v %s", err, out)
	}
	old := exec.Command(helper, "-test.run=^TestChildAuthorityFixture$", "--", "unknown_attack_owner", path)
	old.Env = []string{child.Marker}
	old.Stdout, old.Stderr = io.Discard, io.Discard
	inputRead, input, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	defer inputRead.Close()
	old.Stdin = inputRead
	if err := old.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		if !waited {
			_ = old.Process.Kill()
			_ = old.Wait()
		}
	}()
	var pids []int
	if err := json.Unmarshal(restartAck(t, path+".target_ready"), &pids); err != nil || len(pids) != 2 {
		t.Fatalf("old target acknowledgement=%v %v", pids, err)
	}
	for _, pid := range pids {
		process, err := os.FindProcess(pid)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = process.Kill(); _ = process.Release() })
		if err := process.Signal(syscall.Signal(0)); err != nil {
			t.Fatalf("old acknowledged target not live: %v", err)
		}
	}
	if err := os.WriteFile(path+".kill_guardian", []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	var unknown child.Completion
	if err := json.Unmarshal(restartAck(t, path+".old_unknown"), &unknown); err != nil || unknown != (child.Completion{AttemptID: child.AttemptID{7}, State: child.CompletionUnknown}) {
		t.Fatalf("old fence token=%+v %v", unknown, err)
	}
	if err := old.Wait(); err != nil {
		t.Fatal(err)
	}
	waited = true
	if err := os.WriteFile(fixture+".pause", []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	logsDir := filepath.Join(dir, "new-owner")
	if err := os.MkdirAll(filepath.Join(logsDir, "logs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(logsDir, "logs/crash.log"), []byte("prior complete crash"), 0o600); err != nil {
		t.Fatal(err)
	}
	owner := exec.Command(fixture, "__crash-proof", logsDir)
	owner.Env = []string{}
	produced := &crashByteCount{}
	owner.Stderr = produced
	trigger, err := owner.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer trigger.Close()
	output, err := owner.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	ownerWaited := false
	defer func() {
		if !ownerWaited {
			_ = owner.Process.Kill()
			_ = owner.Wait()
		}
	}()
	var instructions struct {
		Paused struct {
			PID      int
			Address  uintptr
			Dumpable int
		}
		Owner, OwnerLive, OwnerResult int
		GuardianRights                []int
		AttemptID                     child.AttemptID
	}
	instructions.AttemptID = child.AttemptID{7}
	if err := json.Unmarshal(restartAck(t, fixture+".paused"), &instructions.Paused); err != nil {
		t.Fatal(err)
	}
	diagnostic, err := os.FindProcess(instructions.Paused.PID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = diagnostic.Kill(); _ = diagnostic.Release() })
	encoder := json.NewEncoder(input)
	if err := encoder.Encode(instructions); err != nil {
		t.Fatal(err)
	}
	_ = restartAck(t, path+".attacked")
	if err := diagnostic.Signal(syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(restartAck(t, fixture+".protected"), &instructions.GuardianRights); err != nil || len(instructions.GuardianRights) != 2 {
		t.Fatalf("diagnostic rights=%v %v", instructions.GuardianRights, err)
	}
	var aliases []int
	if err := json.Unmarshal(restartAck(t, fixture+".owner_fds"), &aliases); err != nil || len(aliases) != 3 {
		t.Fatal("owner crash pipe acknowledgement invalid")
	}
	instructions.Owner, instructions.OwnerLive, instructions.OwnerResult = aliases[0], aliases[1], aliases[2]
	if err := encoder.Encode(instructions); err != nil {
		t.Fatal(err)
	}
	_ = restartAck(t, path+".protected_attacked")
	if err := diagnostic.Signal(syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	if file, ok := output.(*os.File); ok {
		_ = file.SetReadDeadline(time.Now().Add(5 * time.Second))
	}
	if line, err := bufio.NewReader(output).ReadString('\n'); err != nil || line != "CRASH-DRAIN-READY\n" {
		t.Fatalf("restarted diagnostic readiness=%q %v", line, err)
	}
	if unknown.State != child.CompletionUnknown {
		t.Fatal("diagnostic readiness released old fence")
	}
	for _, pid := range pids {
		process, err := os.FindProcess(pid)
		if err != nil {
			t.Fatal(err)
		}
		err = process.Signal(syscall.Signal(0))
		_ = process.Release()
		if err != nil {
			t.Fatal("old unknown attempt was not live across restart")
		}
	}
	if before, err := os.ReadFile(filepath.Join(logsDir, "logs/crash.log")); err != nil || string(before) != "prior complete crash" {
		t.Fatal("restart published before crash EOF")
	}
	if _, err := trigger.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	_ = trigger.Close()
	err = owner.Wait()
	ownerWaited = true
	if err == nil || produced.total <= 8<<20 {
		t.Fatalf("restart crash was not exercised: %v bytes=%d", err, produced.total)
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if record, err := os.ReadFile(filepath.Join(logsDir, "logs/crash.log")); err == nil && string(record) != "prior complete crash" {
			if len(record) == 0 || len(record) > 1<<20 || bytes.Contains(record, []byte("synthetic-native-crash-secret")) {
				t.Fatalf("restart crash cap/redaction bytes=%d", len(record))
			}
			prior, err := os.ReadFile(filepath.Join(logsDir, "logs/crash.log.1"))
			if err != nil || string(prior) != "prior complete crash" {
				t.Fatal("restart lost prior crash")
			}
			t.Logf("INSTRUMENTED-COMMAND-RESTART-PASS old_attempt=%x old_completion=%s old_target_live=true diagnostic_socket_only=true protected_aliases_denied=true input_bytes=%d retained_bytes=%d", unknown.AttemptID, unknown.State, produced.total, len(record))
			return
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatal("restarted independent diagnostic failed EOF publication")
		}
	}
}

func TestCrashMonitorAfterOwnerDeath(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		t.Run(strconv.FormatBool(unavailable), func(t *testing.T) {
			fixture := childFixtureBinary(t, unavailable)
			dir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dir, "logs"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "logs/crash.log"), []byte("prior complete crash"), 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(fixture, "__crash-proof", dir)
			cmd.Env = []string{}
			produced := &crashByteCount{}
			cmd.Stderr = produced
			input, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			output, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			waited := false
			defer func() {
				if !waited {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				}
			}()
			if file, ok := output.(*os.File); ok {
				if err := file.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
					t.Fatal(err)
				}
			}
			line, err := bufio.NewReader(output).ReadString('\n')
			if err != nil || line != "CRASH-DRAIN-READY\n" {
				t.Fatalf("protected diagnostic readiness=%q %v", line, err)
			}
			before, err := os.ReadFile(filepath.Join(dir, "logs/crash.log"))
			if err != nil || string(before) != "prior complete crash" {
				t.Fatalf("crash record changed before EOF: %q %v", before, err)
			}
			if _, err := input.Write([]byte{1}); err != nil {
				t.Fatal(err)
			}
			_ = input.Close()
			err = cmd.Wait()
			waited = true
			if err == nil {
				t.Fatal("crashing owner exited successfully")
			}
			if produced.total <= 8<<20 {
				t.Fatalf("runtime produced only %d bytes; the large crash was not exercised", produced.total)
			}
			deadline := time.NewTimer(15 * time.Second)
			defer deadline.Stop()
			ticker := time.NewTicker(20 * time.Millisecond)
			defer ticker.Stop()
			for {
				record, err := os.ReadFile(filepath.Join(dir, "logs/crash.log"))
				if err == nil && string(record) != "prior complete crash" {
					if len(record) == 0 || len(record) > 1<<20 || bytes.Contains(record, []byte("synthetic-native-crash-secret")) {
						t.Fatalf("crash output cap/redaction bytes=%d", len(record))
					}
					previous, err := os.ReadFile(filepath.Join(dir, "logs/crash.log.1"))
					if err != nil || string(previous) != "prior complete crash" {
						t.Fatal("previous crash record was not preserved")
					}
					t.Logf("CRASH-OWNER-EOF input_bytes=%d retained_bytes=%d managed_policy_unavailable=%v", produced.total, len(record), unavailable)
					return
				}
				select {
				case <-ticker.C:
				case <-deadline.C:
					t.Fatal("diagnostic did not survive owner death and publish at EOF")
				}
			}
		})
	}
}

// cli runs one stopped command of the built binary against dir.
func cli(t *testing.T, dir, name string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(binary, append([]string{name}, args...)...)
	cmd.Env = []string{"BOXSET_DATA_DIR=" + dir, "PATH=/usr/bin:/bin"}
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	err := cmd.Run()
	return output.String(), exitCodeOf(t, err)
}

// healthcheck reports the state `boxset healthcheck` answers for dir.
func healthcheckCLI(t *testing.T, dir string) (string, int) {
	t.Helper()
	return cli(t, dir, "healthcheck")
}

// serveChildren returns the pids whose parent is pid, from the kernel's own list. The
// list is per thread, so every task of the process is read and the hits united.
func serveChildren(t *testing.T, pid int) []int {
	t.Helper()
	tasks, err := os.ReadDir(filepath.Join("/proc", strconv.Itoa(pid), "task"))
	if err != nil {
		t.Fatal(err)
	}
	var pids []int
	for _, task := range tasks {
		raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "task", task.Name(), "children"))
		if err != nil {
			t.Fatal(err)
		}
		for _, field := range strings.Fields(string(raw)) {
			child, err := strconv.Atoi(field)
			if err != nil {
				t.Fatalf("%q in the child list %q", field, raw)
			}
			pids = append(pids, child)
		}
	}
	return pids
}

// terminated reports why pid is no longer running: nil when the drain killed it, an
// error when it is still running or ended by itself. The wait status is read from the
// kernel's own record, because a child the init process has not reaped stays in the
// table as a zombie and a signal sent to it succeeds.
func terminated(t *testing.T, pid int) error {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	// The command name may hold spaces and parentheses, so the fields start after the
	// last ')': index 0 is the state and index 49 the wait status (signal number in
	// the low byte), as /proc/<pid>/stat documents.
	stat := string(raw)
	fields := strings.Fields(stat[strings.LastIndex(stat, ")")+1:])
	if len(fields) < 50 {
		t.Fatalf("stat %q holds %d fields", stat, len(fields))
	}
	if fields[0] != "Z" {
		return fmt.Errorf("still %s", fields[0])
	}
	if code, err := strconv.Atoi(fields[49]); err != nil || code != int(syscall.SIGKILL) {
		return fmt.Errorf("wait status %s, want the SIGKILL the drain sends", fields[49])
	}
	return nil
}

// TestDrainReleasesLock requires a stop to end cleanly inside the 30 s drain, to
// record the order the contract depends on — the credential worker joined, then
// the store closed, then the lock released — and to leave the data directory
// reacquirable, so another server reaches ready on it.
func TestDrainReleasesLock(t *testing.T) {
	dir := t.TempDir()
	s := start(t, dir)
	s.waitReady(t)
	if out, code := healthcheckCLI(t, dir); code != 0 || strings.TrimSpace(out) != "ready" {
		t.Fatalf("healthcheck while ready = %q exit=%d", out, code)
	}
	if _, err := os.Stat(filepath.Join(dir, "logs", "boxset.log")); err != nil {
		t.Fatalf("the running server wrote no rotating log: %v", err)
	}
	// The crash monitor is this binary as a child of serve, started before the
	// listener: the drain must end it, not hand it to the init process.
	children := serveChildren(t, s.cmd.Process.Pid)
	if len(children) == 0 {
		t.Fatal("the running server has no crash-monitor child")
	}
	if err := s.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if code := s.exited(t); code != 0 {
		t.Fatalf("stop exit=%d: %s", code, s.stderr.String())
	}
	// The drain's own bound is the contract: a child it signalled reaches the
	// kernel's terminal state shortly after the process exits, but not always by
	// the time the exit returns, so the diagnosis is sampled until the bound.
	for _, pid := range children {
		deadline := time.Now().Add(drainTimeout)
		for {
			err := terminated(t, pid)
			if err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the crash monitor (pid %d) outlived the drain: %v", pid, err)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	// The lock is released only once every mutation capability has ended.
	lock, err := install.Acquire(dir)
	if errors.Is(err, install.ErrLockHeld) {
		t.Fatalf("the lifetime lock outlived the stop: %s", s.stderr.String())
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	record := s.stderr.String()
	last := -1
	for _, step := range []string{"signal received", "metadata worker joined", "store closed", "lock released"} {
		at := strings.Index(record, step)
		if at < 0 {
			t.Fatalf("%q is missing from the stop record: %s", step, record)
		}
		if at < last {
			t.Fatalf("%q was recorded before the step the contract puts before it: %s", step, record)
		}
		last = at
	}
	if out, code := healthcheckCLI(t, dir); code != 1 || strings.TrimSpace(out) != "draining" {
		t.Fatalf("healthcheck after the stop = %q exit=%d, want draining and non-zero", out, code)
	}
	// A second server takes the same data directory and reaches ready again.
	next := start(t, dir)
	next.waitReady(t)
	terminate(t, next)
}

// TestHealthcheckStates requires `boxset healthcheck` to report the lifecycle states
// of a real installation — ready while it serves, draining once it stopped, and
// recovering when its key file no longer matches — exit 0 only when it is ready, and
// refuse a record no server wrote instead of guessing one. `starting` is the one
// state the test records itself: serve writes it before the listener exists
// (cmd/boxset/main.go:418), so the window is too narrow to sample here without a race.
func TestHealthcheckStates(t *testing.T) {
	dir := t.TempDir()
	s := start(t, dir)
	s.waitReady(t)
	if out, code := healthcheckCLI(t, dir); code != 0 || strings.TrimSpace(out) != "ready" {
		t.Fatalf("healthcheck while ready = %q exit=%d, want ready and exit 0", out, code)
	}
	terminate(t, s)
	if out, code := healthcheckCLI(t, dir); code != 1 || strings.TrimSpace(out) != "draining" {
		t.Fatalf("healthcheck after a clean stop = %q exit=%d, want draining and exit 1", out, code)
	}

	// A key file whose bytes no longer fingerprint to its own name is a mismatch:
	// the installation must come back recovering, not ready.
	keys, err := os.ReadDir(secret.Dir(dir))
	if err != nil || len(keys) != 1 {
		t.Fatalf("key files after a first start = %v, %v; want exactly one", keys, err)
	}
	if err := install.WriteSecretFile(filepath.Join(secret.Dir(dir), keys[0].Name()), bytes.Repeat([]byte{1}, secret.KeySize)); err != nil {
		t.Fatal(err)
	}
	broken := start(t, dir)
	deadline := time.Now().Add(30 * time.Second)
	for {
		out, code := healthcheckCLI(t, dir)
		if code == 1 && strings.TrimSpace(out) == "recovering" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("healthcheck over a key-broken installation = %q exit=%d, want recovering and exit 1", out, code)
		}
		time.Sleep(50 * time.Millisecond)
	}
	terminate(t, broken)

	// The one state no poll samples reliably, and the records no server wrote.
	for _, tc := range []struct {
		state string
		exit  int
	}{
		{"starting", 1},
	} {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "run"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "run", "state"), []byte(tc.state), 0o600); err != nil {
			t.Fatal(err)
		}
		out, code := healthcheckCLI(t, dir)
		if code != tc.exit || strings.TrimSpace(out) != tc.state {
			t.Fatalf("%s: healthcheck = %q exit=%d, want the state and exit %d", tc.state, out, code, tc.exit)
		}
	}
	out, code := healthcheckCLI(t, t.TempDir())
	if code != 3 || !strings.Contains(out, "healthcheck_unavailable") {
		t.Fatalf("a directory no server started in = %q exit=%d, want exit 3 healthcheck_unavailable", out, code)
	}
	dir = t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "run"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "run", "state"), []byte("readyy"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, code := healthcheckCLI(t, dir); code != 3 || !strings.Contains(out, "healthcheck_unavailable") {
		t.Fatalf("an unknown state word = %q exit=%d, want a refusal", out, code)
	}
}

// TestChildCannotServe requires the marker environment to be a refusal: a child
// process can never become a second server, a listener or a lock holder.
func TestChildCannotServe(t *testing.T) {
	dir := t.TempDir()
	s := startIn(t, "", dir, "BOXSET_CHILD=1")
	if code := s.exited(t); code != 2 {
		t.Fatalf("serve under the child marker exit=%d: %s", code, s.stderr.String())
	}
	if !strings.Contains(s.stderr.String(), "child_cannot_serve") {
		t.Fatalf("serve under the child marker did not name child_cannot_serve: %s", s.stderr.String())
	}
	if state := s.ping(); state != "" {
		t.Fatalf("a child answered /ping with %q", state)
	}
	if _, err := os.Stat(filepath.Join(dir, "boxset.lock")); err == nil {
		t.Fatal("a child created the lifetime lock file")
	}
}

// TestCrashMonitorOverServe requires a crashed start to leave exactly one bounded
// crash record, published only after the process ended: no crash byte is on disk
// while the server runs, and the record the monitor writes is capped.
func TestCrashMonitorOverServe(t *testing.T) {
	dir := t.TempDir()
	s := start(t, dir)
	s.waitReady(t)
	record := filepath.Join(dir, "logs", "crash.log")
	if _, err := os.Stat(record); err == nil {
		t.Fatal("a crash record exists while the server is running")
	}
	if err := s.cmd.Process.Signal(syscall.SIGABRT); err != nil {
		t.Fatal(err)
	}
	if code := s.exited(t); code == 0 {
		t.Fatal("a crashed server reported a successful exit")
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		info, err := os.Stat(record)
		if err == nil {
			if info.Size() == 0 {
				t.Fatal("the crash record is empty")
			}
			if info.Size() > 1<<20 {
				t.Fatalf("the crash record holds %d bytes, want at most %d", info.Size(), 1<<20)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no crash record after a crashed start: %s", s.stderr.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}
