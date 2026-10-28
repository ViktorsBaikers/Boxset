//go:build linux

package child

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

type pausedGuardian struct {
	PID      int
	Address  uintptr
	Dumpable int
}

func writeFixtureFile(path string, raw []byte, mode os.FileMode) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".ack-")
	if err != nil {
		return err
	}
	name := file.Name()
	defer func() { _ = os.Remove(name) }()
	if err := file.Chmod(mode); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(raw); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func TestMain(m *testing.M) {
	self, err := os.Executable()
	if err != nil {
		os.Exit(3)
	}
	fixture := filepath.Base(self) == "fixture-child.test"
	if fixture && len(os.Args) > 1 && os.Args[1] == childEntry {
		if _, err := os.Stat(self + ".pause"); err == nil {
			sentinel := []byte("bootstrap-sentinel")
			var pin runtime.Pinner
			pin.Pin(&sentinel[0])
			dumpable, e := unix.PrctlRetInt(unix.PR_GET_DUMPABLE, 0, 0, 0, 0)
			if e != nil {
				os.Exit(3)
			}
			raw, _ := json.Marshal(pausedGuardian{PID: os.Getpid(), Address: uintptr(unsafe.Pointer(&sentinel[0])), Dumpable: dumpable})
			if writeFixtureFile(self+".paused", raw, 0o600) != nil {
				os.Exit(3)
			}
			if unix.Kill(os.Getpid(), unix.SIGSTOP) != nil {
				os.Exit(3)
			}
			if string(sentinel) != "bootstrap-sentinel" {
				os.Exit(3)
			}
			pin.Unpin()
		}
	}
	var observer func(string, []int)
	if fixture {
		observer = func(phase string, fds []int) {
			if phase == "bootstrap" {
				if value, err := unix.PrctlRetInt(unix.PR_GET_NO_NEW_PRIVS, 0, 0, 0, 0); err != nil || value != 1 {
					os.Exit(3)
				}
				if err := verifyOwner(); err != nil {
					os.Exit(3)
				}
				if _, err := os.Stat(self + ".startup_hold"); err == nil {
					time.Sleep(time.Minute)
				}
			}
			if phase == "before-release" {
				if _, err := os.Stat(self + ".release_fault"); err == nil {
					var limit unix.Rlimit
					if unix.Getrlimit(unix.RLIMIT_NOFILE, &limit) != nil {
						os.Exit(3)
					}
					limit.Cur = 64
					if unix.Setrlimit(unix.RLIMIT_NOFILE, &limit) != nil {
						os.Exit(3)
					}
					for {
						_, err := unix.Open(os.DevNull, unix.O_RDONLY|unix.O_CLOEXEC, 0)
						if err == unix.EMFILE {
							break
						}
						if err != nil {
							os.Exit(3)
						}
					}
				}
			}
			if phase == "protected" {
				if raw, err := os.ReadFile(self + ".terminal_payload"); err == nil {
					if len(fds) != 2 || len(raw) > 2048 {
						os.Exit(3)
					}
					if len(raw) > 0 {
						if n, err := unix.Write(fds[1], raw); err != nil || n != len(raw) {
							os.Exit(3)
						}
					}
					if _, err := os.Stat(self + ".retain_writer"); err == nil {
						time.Sleep(time.Minute)
					}
					os.Exit(0)
				}
			}
			if phase == "cleanup" {
				if path, err := os.ReadFile(self + ".generation"); err == nil {
					if writeFixtureFile(string(path)+".cleanup", []byte("1"), 0o600) != nil {
						os.Exit(3)
					}
					ctx, cancel := context.WithTimeout(context.Background(), waitDelay)
					defer cancel()
					if _, err := waitFixtureBytes(ctx, string(path)+".generation_ack"); err != nil {
						os.Exit(3)
					}
					if _, err := waitFixtureBytes(ctx, string(path)+".generation_pinned"); err != nil {
						os.Exit(3)
					}
				}
			}
			raw, _ := json.Marshal(fds)
			if writeFixtureFile(self+"."+phase, raw, 0o600) != nil {
				os.Exit(3)
			}
		}
	}
	if handled, code := enter(os.Args[1:], observer); handled {
		os.Exit(code)
	}
	if helperMode() != "" {
		os.Exit(m.Run())
	}
	if os.Args[len(os.Args)-1] == "prepared-test-owner" {
		if PrepareManagedOwner() != nil {
			os.Exit(3)
		}
		os.Exit(m.Run())
	}
	cmd := exec.Command(self, append(os.Args[1:], "prepared-test-owner")...)
	cmd.Env = []string{Marker}
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			os.Exit(exit.ExitCode())
		}
		os.Exit(3)
	}
	os.Exit(0)
}

type attackInstructions struct {
	Paused         pausedGuardian
	Owner          int
	OwnerLive      int
	OwnerResult    int
	GuardianRights []int
	AttemptID      AttemptID
}

func helperCommand(mode, path string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=^TestChildAuthorityFixture$", "--", mode, path)
	cmd.Env = []string{Marker}
	return cmd
}

func TestChildAuthorityFixture(t *testing.T) {
	mode := helperMode()
	if mode != "control_pipe" && mode != "attack_target" && mode != "authority_owner" && mode != "unknown_attack_owner" && mode != "unknown_attack_target" && mode != "descriptor_limit" && mode != "bootstrap_sender" {
		return
	}
	path := os.Args[len(os.Args)-1]
	switch mode {
	case "bootstrap_sender":
		ctx, cancel := context.WithTimeout(t.Context(), waitDelay)
		defer cancel()
		if err := sendBootstrap(ctx, 3, childFrame{Version: 1, Role: "managed", Phase: "bootstrap-ready"}, nil); err != nil {
			t.Fatal(err)
		}
	case "descriptor_limit":
		pair, err := privatePair()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = unix.Close(pair[0]); _ = unix.Close(pair[1]) }()
		var files []*os.File
		var rights []int
		for range 3 {
			read, write, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			files = append(files, read, write)
			fd, err := fileDescriptor(read)
			if err != nil {
				t.Fatal(err)
			}
			rights = append(rights, fd)
		}
		defer func() {
			for _, file := range files {
				_ = file.Close()
			}
		}()
		if err := sendBootstrap(t.Context(), pair[1], childFrame{Version: 1, Role: "managed", Phase: "handoff", StartupDeadlineUnixNano: time.Now().Add(waitDelay).UnixNano()}, rights); err != nil {
			t.Fatal(err)
		}
		var original unix.Rlimit
		if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &original); err != nil {
			t.Fatal(err)
		}
		limited := original
		limited.Cur = 64
		if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &limited); err != nil {
			t.Fatal(err)
		}
		var held []int
		defer func() {
			for _, fd := range held {
				_ = unix.Close(fd)
			}
			_ = unix.Setrlimit(unix.RLIMIT_NOFILE, &original)
		}()
		for {
			fd, err := unix.Open(os.DevNull, unix.O_RDONLY|unix.O_CLOEXEC, 0)
			if err == unix.EMFILE {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			held = append(held, fd)
		}
		_, received, err := receiveBootstrap(t.Context(), pair[0], os.Getpid(), "managed", "handoff", 3)
		if err == nil || len(received) != 0 {
			t.Fatal("descriptor exhaustion accepted partial rights")
		}
		for _, fd := range held {
			_ = unix.Close(fd)
		}
		held = nil
		if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &original); err != nil {
			t.Fatal(err)
		}
		// A second valid transfer proves the exhausted receive left no queued authority.
		if err := sendBootstrap(t.Context(), pair[1], childFrame{Version: 1, Role: "managed", Phase: "handoff", StartupDeadlineUnixNano: time.Now().Add(waitDelay).UnixNano()}, rights); err != nil {
			t.Fatal(err)
		}
		_, received, err = receiveBootstrap(t.Context(), pair[0], os.Getpid(), "managed", "handoff", 3)
		if err != nil || len(received) != 3 {
			t.Fatalf("post-exhaustion transfer=%d %v", len(received), err)
		}
		for _, fd := range received {
			_ = unix.Close(fd)
		}
	case "control_pipe":
		memory := []byte("control-sentinel")
		var pin runtime.Pinner
		pin.Pin(&memory[0])
		defer pin.Unpin()
		raw, _ := json.Marshal(pausedGuardian{PID: os.Getpid(), Address: uintptr(unsafe.Pointer(&memory[0])), Dumpable: 1})
		if err := writeFixtureFile(path+".control", raw, 0o600); err != nil {
			t.Fatal(err)
		}
		input := os.NewFile(3, "control pipe")
		reader := bufio.NewReader(input)
		var expected childFrame
		if err := json.Unmarshal(fixtureFile(t, path+".candidate"), &expected); err != nil {
			t.Fatal(err)
		}
		frame, err := readFrame(reader, "managed", "terminal", expected.AttemptID)
		if err != nil || frame != expected || frame.Completion != TreeQuiescent {
			t.Fatalf("positive pipe injection=%+v %v", frame, err)
		}
		if err := writeFixtureFile(path+".control_memory", memory, 0o600); err != nil {
			t.Fatal(err)
		}
	case "attack_target", "unknown_attack_target":
		if mode == "unknown_attack_target" {
			null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			os.Stdout = null
		}
		beacon := startHeartbeat(path + ".heartbeat")
		if beacon == nil {
			t.Fatal("attack descendant did not start")
		}
		_ = fixtureFile(t, path+".heartbeat")
		if err := writeFixtureFile(path+".descendant", []byte(strconv.Itoa(beacon.Process.Pid)), 0o600); err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal([]int{os.Getpid(), beacon.Process.Pid})
		if err := writeFixtureFile(path+".target_ready", raw, 0o600); err != nil {
			t.Fatal(err)
		}
		decoder := json.NewDecoder(os.Stdin)
		var instructions attackInstructions
		if err := decoder.Decode(&instructions); err != nil {
			t.Fatal(err)
		}
		probeBootstrap(t, path, instructions)
		if err := writeFixtureFile(path+".attacked", []byte("1"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := decoder.Decode(&instructions); err != nil {
			t.Fatal(err)
		}
		probeProtectedEndpoints(t, instructions)
		if err := writeFixtureFile(path+".protected_attacked", []byte("1"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, os.Stdin)
	case "authority_owner":
		if PrepareManagedOwner() != nil {
			t.Fatal("owner protection refused")
		}
		runAuthorityOwner(t, path)
	case "unknown_attack_owner":
		if PrepareManagedOwner() != nil {
			t.Fatal("old owner protection refused")
		}
		p, err := Start(t.Context(), Spec{Path: os.Args[0], Args: []string{"-test.run=^TestChildAuthorityFixture$", "--", "unknown_attack_target", path}, Timeout: time.Minute, AttemptID: AttemptID{7}}, os.Stdin)
		if err != nil {
			t.Fatal(err)
		}
		_ = fixtureFile(t, path+".target_ready")
		_ = fixtureFile(t, path+".kill_guardian")
		if err := p.cmd.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		result, err := p.Wait()
		if !errors.Is(err, ErrCompletionUnknown) || result.Completion != (Completion{AttemptID: AttemptID{7}, State: CompletionUnknown}) {
			t.Fatalf("old completion=%+v %v", result.Completion, err)
		}
		raw, _ := json.Marshal(result.Completion)
		if err := writeFixtureFile(path+".old_unknown", raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func vmTransfer(pid int, address uintptr, buffer []byte, write bool) (int, error) {
	local := []unix.Iovec{{Base: &buffer[0]}}
	local[0].SetLen(len(buffer))
	remote := []unix.RemoteIovec{{Base: address, Len: len(buffer)}}
	if write {
		return unix.ProcessVMWritev(pid, local, remote, 0)
	}
	return unix.ProcessVMReadv(pid, local, remote, 0)
}

const (
	controlPtrace = iota
	controlDescriptor
	controlReadMemory
	controlWriteMemory
	controlPipe
	controlCount
)

type bootstrapControlObservation struct {
	Name      string
	PID       int
	FD        int
	Device    uint64
	Inode     uint64
	Access    int
	Errno     unix.Errno
	Validated bool
}

type bootstrapControlReport struct {
	PID           int
	Profile       string
	PipeDevice    uint64
	PipeInode     uint64
	Controls      [controlCount]bootstrapControlObservation
	ForeignDenied bool
}

func acceptBootstrapControls(report bootstrapControlReport) error {
	if report.PID <= 0 || report.Profile == "" || report.PipeDevice == 0 || report.PipeInode == 0 || !report.ForeignDenied {
		return errors.New("control_context_invalid")
	}
	for index, name := range []string{"ptrace_attach", "pidfd_getfd", "process_vm_readv", "process_vm_writev", "pipe_injection"} {
		observed := report.Controls[index]
		if observed.Name != name {
			return fmt.Errorf("control_missing:%s", name)
		}
		if observed.PID != report.PID {
			return fmt.Errorf("control_pid:%s", name)
		}
		fd := -1
		if index == controlDescriptor || index == controlPipe {
			fd = 3
		}
		if observed.FD != fd {
			return fmt.Errorf("control_fd:%s", name)
		}
		access := unix.O_RDONLY
		if index == controlPipe {
			access = unix.O_WRONLY
		}
		if fd == 3 && observed.Validated && observed.Access != access {
			return fmt.Errorf("control_pipe_direction:%s", name)
		}
		if fd == 3 && observed.Validated && (observed.Device != report.PipeDevice || observed.Inode != report.PipeInode) {
			return fmt.Errorf("control_pipe_identity:%s", name)
		}
		if observed.Errno == 0 && !observed.Validated {
			return fmt.Errorf("control_unvalidated:%s", name)
		}
	}
	for _, observed := range report.Controls {
		if observed.Errno != 0 {
			return fmt.Errorf("control_failed:%s:%d", observed.Name, observed.Errno)
		}
	}
	return nil
}

func assertBootstrapControlOracle(t *testing.T, report bootstrapControlReport, transcript string) {
	t.Helper()
	baseline := acceptBootstrapControls(report)
	t.Run("complete_native_report", func(t *testing.T) {
		unavailable := false
		for _, observed := range report.Controls {
			unavailable = unavailable || observed.Errno != 0
		}
		marker := strings.Contains(transcript, "CHILD-BOOT-CONTROLS-PASS")
		if unavailable {
			if baseline == nil || marker {
				t.Fatal("CHILD-BOOT-ORACLE: unavailable native control received full-control credit")
			}
			t.Logf("native controls unavailable: %v; no full-control credit", baseline)
		} else if baseline != nil || !marker {
			t.Fatalf("CHILD-BOOT-ORACLE: complete actual controls rejected: %v marker=%v", baseline, marker)
		} else {
			t.Log("CHILD-BOOT-ORACLE-VALID controls=5 actual_native=true")
		}
	})
	type controlTrial struct {
		name string
		want string
		edit func(*bootstrapControlReport)
	}
	trials := []controlTrial{
		{"always_denied", fmt.Sprintf("control_failed:ptrace_attach:%d", unix.EPERM), func(r *bootstrapControlReport) {
			for index := range r.Controls {
				r.Controls[index].Errno, r.Controls[index].Validated = unix.EPERM, false
			}
		}},
		{"always_unavailable", fmt.Sprintf("control_failed:ptrace_attach:%d", unix.ENOSYS), func(r *bootstrapControlReport) {
			for index := range r.Controls {
				r.Controls[index].Errno, r.Controls[index].Validated = unix.ENOSYS, false
			}
		}},
		{"wrong_fd", "control_fd:pidfd_getfd", func(r *bootstrapControlReport) { r.Controls[controlDescriptor].FD = 4 }},
		{"wrong_pid", "control_pid:ptrace_attach", func(r *bootstrapControlReport) { r.Controls[controlPtrace].PID++ }},
		{"wrong_pipe", "control_pipe_identity:pipe_injection", func(r *bootstrapControlReport) { r.Controls[controlPipe].Inode++ }},
		{"wrong_direction", "control_pipe_direction:pipe_injection", func(r *bootstrapControlReport) { r.Controls[controlPipe].Access = unix.O_RDONLY }},
		{"foreign_denial_omitted", "control_context_invalid", func(r *bootstrapControlReport) { r.ForeignDenied = false }},
	}
	for index, observed := range report.Controls {
		trials = append(trials, controlTrial{"skipped_" + observed.Name, "control_unvalidated:" + observed.Name, func(r *bootstrapControlReport) {
			r.Controls[index].Errno, r.Controls[index].Validated = 0, false
		}})
	}
	for _, trial := range trials {
		t.Run(trial.name, func(t *testing.T) {
			bad := report
			trial.edit(&bad)
			err := acceptBootstrapControls(bad)
			if err == nil || err.Error() != trial.want {
				t.Fatalf("CHILD-BOOT-ORACLE: planted %s rejection=%v want=%s", trial.name, err, trial.want)
			}
			if baseline != nil && err.Error() == baseline.Error() {
				t.Fatalf("CHILD-BOOT-ORACLE: planted %s masked by preexisting unavailable control: %v", trial.name, baseline)
			}
			t.Logf("CHILD-BOOT-ORACLE-REJECT planted=%s reason=%s", trial.name, err)
		})
	}
}

func probeBootstrap(t *testing.T, path string, instructions attackInstructions) {
	t.Helper()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = read.Close(); _ = write.Close() }()
	control := helperCommand("control_pipe", path)
	control.ExtraFiles = []*os.File{read}
	control.Stdout, control.Stderr = io.Discard, io.Discard
	frame := childFrame{Version: 1, Role: "managed", Phase: "terminal", AttemptID: instructions.AttemptID, Completion: TreeQuiescent, CleanupDeadlineUnixNano: time.Now().Add(waitDelay).UnixNano()}
	injection, _ := json.Marshal(frame)
	injection = append(injection, '\n')
	if err := writeFixtureFile(path+".candidate", injection, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := control.Start(); err != nil {
		t.Fatal(err)
	}
	controlWaited := false
	defer func() {
		if !controlWaited {
			_ = control.Process.Kill()
			_ = control.Wait()
		}
	}()
	var allowed pausedGuardian
	if err := json.Unmarshal(fixtureFile(t, path+".control"), &allowed); err != nil || allowed.PID != control.Process.Pid {
		t.Fatalf("control identity=%+v %v", allowed, err)
	}
	readFD, err := fileDescriptor(read)
	if err != nil {
		t.Fatal(err)
	}
	var pipeIdentity unix.Stat_t
	if err := unix.Fstat(readFD, &pipeIdentity); err != nil {
		t.Fatal(err)
	}
	report := bootstrapControlReport{PID: control.Process.Pid, PipeDevice: pipeIdentity.Dev, PipeInode: pipeIdentity.Ino}
	for index, name := range []string{"ptrace_attach", "pidfd_getfd", "process_vm_readv", "process_vm_writev", "pipe_injection"} {
		fd := -1
		if index == controlDescriptor || index == controlPipe {
			fd = 3
		}
		report.Controls[index] = bootstrapControlObservation{Name: name, PID: allowed.PID, FD: fd}
	}
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Fatal(err)
	}
	profile := ""
	for _, line := range strings.Split(string(status), "\n") {
		if strings.HasPrefix(line, "Seccomp:") {
			fields := strings.Fields(line)
			if len(fields) == 2 {
				profile = "seccomp-" + fields[1]
			}
		}
	}
	if profile == "" {
		t.Fatal("control profile is unobservable")
	}
	report.Profile = profile
	unavailable := func(index int, err error) {
		if err != unix.EPERM && err != unix.EACCES && err != unix.ENOSYS {
			t.Fatalf("valid control %s failed: %v", report.Controls[index].Name, err)
		}
		if !errors.As(err, &report.Controls[index].Errno) {
			t.Fatal(err)
		}
		_, _ = fmt.Fprintf(os.Stdout, "CONTROL-UNAVAILABLE syscall=%s errno=%v profile=%s\n", report.Controls[index].Name, err, profile)
	}
	if err := unix.PtraceAttach(allowed.PID); err != nil {
		unavailable(controlPtrace, err)
	} else {
		var status unix.WaitStatus
		if pid, err := unix.Wait4(allowed.PID, &status, unix.WUNTRACED, nil); err != nil || pid != allowed.PID || !status.Stopped() {
			t.Fatalf("ptrace control stop=%v %v", status, err)
		}
		if err := unix.PtraceDetach(allowed.PID); err != nil {
			t.Fatal(err)
		}
		report.Controls[controlPtrace].Validated = true
	}
	controlFD, err := unix.PidfdOpen(allowed.PID, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(controlFD) }()
	if duplicate, err := unix.PidfdGetfd(controlFD, 3, 0); err != nil {
		unavailable(controlDescriptor, err)
	} else {
		var stat unix.Stat_t
		access, err := unix.FcntlInt(uintptr(duplicate), unix.F_GETFL, 0)
		if unix.Fstat(duplicate, &stat) != nil || stat.Mode&unix.S_IFMT != unix.S_IFIFO || stat.Dev != pipeIdentity.Dev || stat.Ino != pipeIdentity.Ino || err != nil || access&unix.O_ACCMODE != unix.O_RDONLY {
			t.Error("pidfd_getfd control duplicated the wrong descriptor")
		} else {
			report.Controls[controlDescriptor].Device, report.Controls[controlDescriptor].Inode = stat.Dev, stat.Ino
			report.Controls[controlDescriptor].Access = access & unix.O_ACCMODE
			report.Controls[controlDescriptor].Validated = true
		}
		_ = unix.Close(duplicate)
	}
	memory := make([]byte, len("control-sentinel"))
	if n, err := vmTransfer(allowed.PID, allowed.Address, memory, false); err != nil {
		unavailable(controlReadMemory, err)
	} else if n != len(memory) || string(memory) != "control-sentinel" {
		t.Fatalf("read control returned %d %q", n, memory)
	} else {
		report.Controls[controlReadMemory].Validated = true
	}
	changed := []byte("changed-sentinel")
	wrote := false
	if n, err := vmTransfer(allowed.PID, allowed.Address, changed, true); err != nil {
		unavailable(controlWriteMemory, err)
	} else if n != len(changed) {
		t.Fatalf("write control returned %d", n)
	} else {
		wrote = true
	}
	pipePath := fmt.Sprintf("/proc/%d/fd/3", allowed.PID)
	pipe, err := os.OpenFile(pipePath, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("dumpable pipe control cannot reopen: %v", err)
	}
	pipeFD, err := fileDescriptor(pipe)
	if err != nil {
		t.Fatal(err)
	}
	var reopened unix.Stat_t
	access, err := unix.FcntlInt(uintptr(pipeFD), unix.F_GETFL, 0)
	if err != nil || access&unix.O_ACCMODE != unix.O_WRONLY {
		t.Fatalf("dumpable pipe control reopened the wrong direction: %v", err)
	}
	if err := unix.Fstat(pipeFD, &reopened); err != nil || reopened.Dev != pipeIdentity.Dev || reopened.Ino != pipeIdentity.Ino {
		t.Fatalf("dumpable pipe control reopened the wrong descriptor: %v", err)
	}
	if n, err := pipe.Write(injection); err != nil || n != len(injection) {
		t.Fatalf("dumpable control injection=%d %v", n, err)
	}
	observed := fixtureFile(t, path+".control_memory")
	if wrote && string(observed) != "changed-sentinel" || !wrote && string(observed) != "control-sentinel" {
		t.Fatalf("control memory after probe=%q wrote=%v", observed, wrote)
	}
	report.Controls[controlWriteMemory].Validated = wrote
	err = control.Wait()
	controlWaited = true
	if err != nil {
		t.Fatal(err)
	}
	_ = write.Close()
	poll := []unix.PollFd{{Fd: int32(readFD), Events: unix.POLLIN}}
	if _, err := unix.Poll(poll, 0); err != nil || poll[0].Revents&unix.POLLHUP != 0 {
		t.Fatalf("retained writer control did not retain EOF: %v", err)
	}
	_ = pipe.Close()
	if _, err := unix.Poll(poll, 1000); err != nil || poll[0].Revents&unix.POLLHUP == 0 {
		t.Fatalf("closing retained writer did not release EOF: %v", err)
	}
	report.Controls[controlPipe].Device, report.Controls[controlPipe].Inode = reopened.Dev, reopened.Ino
	report.Controls[controlPipe].Access = access & unix.O_ACCMODE
	report.Controls[controlPipe].Validated = true
	foreign := instructions.Paused
	if foreign.Dumpable != 1 {
		t.Fatal("foreign guardian was not stopped in the dumpable exec window")
	}
	socketPath := fmt.Sprintf("/proc/%d/fd/3", foreign.PID)
	link, err := os.Readlink(socketPath)
	if err != nil || !strings.HasPrefix(link, "socket:[") {
		t.Fatalf("foreign bootstrap resource missing: %q %v", link, err)
	}
	entries, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", foreign.PID))
	if err != nil || len(entries) > 64 {
		t.Fatalf("bootstrap descriptor inventory=%d %v", len(entries), err)
	}
	for _, entry := range entries {
		fd, _ := strconv.Atoi(entry.Name())
		if fd <= 3 {
			continue
		}
		link, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/%d", foreign.PID, fd))
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(link, "pipe:[") {
			t.Fatalf("authoritative pipe crossed exec at fd%d", fd)
		}
	}
	for _, flags := range []int{unix.O_RDONLY, unix.O_WRONLY, unix.O_RDWR} {
		fd, err := unix.Open(socketPath, flags|unix.O_CLOEXEC, 0)
		if err == nil {
			_ = unix.Close(fd)
			t.Fatal("dumpable bootstrap socket reopened with usable authority")
		}
		if err != unix.ENXIO {
			t.Fatalf("bootstrap open failed at the wrong boundary: %v", err)
		}
	}
	opath, err := unix.Open(socketPath, unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unix.Write(opath, injection); err != unix.EBADF {
		t.Errorf("O_PATH became a writer: %v", err)
	}
	_ = unix.Close(opath)
	if err := unix.PtraceAttach(foreign.PID); err == nil {
		_ = unix.PtraceDetach(foreign.PID)
		t.Fatal("managed sibling attached to the dumpable guardian")
	} else if err != unix.EPERM && err != unix.EACCES {
		t.Fatalf("foreign ptrace unavailable, not denied: %v", err)
	}
	foreignFD, err := unix.PidfdOpen(foreign.PID, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(foreignFD) }()
	if fd, err := unix.PidfdGetfd(foreignFD, 3, 0); err == nil {
		_ = unix.Close(fd)
		t.Fatal("managed sibling duplicated bootstrap socket")
	} else if err != unix.EPERM && err != unix.EACCES {
		t.Fatalf("foreign duplication unavailable: %v", err)
	}
	foreignMemory := make([]byte, len("bootstrap-sentinel"))
	for _, write := range []bool{false, true} {
		if _, err := vmTransfer(foreign.PID, foreign.Address, foreignMemory, write); err == nil {
			t.Fatal("managed sibling accessed paused guardian memory")
		} else if err != unix.EPERM && err != unix.EACCES {
			t.Fatalf("foreign memory unavailable: %v", err)
		}
	}
	report.ForeignDenied = !t.Failed()
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFixtureFile(path+".controls", raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func probeProtectedEndpoints(t *testing.T, instructions attackInstructions) {
	t.Helper()
	endpoints := []struct{ pid, fd int }{{instructions.Owner, instructions.OwnerLive}, {instructions.Owner, instructions.OwnerResult}}
	for _, fd := range instructions.GuardianRights {
		endpoints = append(endpoints, struct{ pid, fd int }{instructions.Paused.PID, fd})
	}
	for _, endpoint := range endpoints {
		path := fmt.Sprintf("/proc/%d/fd/%d", endpoint.pid, endpoint.fd)
		if _, err := os.Readlink(path); !errors.Is(err, os.ErrPermission) {
			t.Fatalf("protected endpoint readlink=%v", err)
		}
		for _, flags := range []int{os.O_RDONLY, os.O_WRONLY} {
			file, err := os.OpenFile(path, flags, 0)
			if err == nil {
				_ = file.Close()
				t.Fatal("protected authority reopened")
			}
			if !errors.Is(err, os.ErrPermission) {
				t.Fatalf("protected authority absent instead of denied: %v", err)
			}
		}
	}
}

func runAuthorityOwner(t *testing.T, path string) {
	t.Helper()
	inputRead, inputWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = inputRead.Close(); _ = inputWrite.Close() }()
	aSpec := Spec{Path: os.Args[0], Args: []string{"-test.run=^TestChildAuthorityFixture$", "--", "attack_target", path}, Timeout: 30 * time.Second, AttemptID: AttemptID{7}}
	a, err := Start(t.Context(), aSpec, inputRead)
	if err != nil {
		t.Fatal(err)
	}
	_ = fixtureFile(t, path+".target_ready")
	go func() {
		result, err := a.Wait()
		if err != nil || result.Exit != 0 {
			_, _ = os.Stdout.WriteString(result.Output)
		}
	}()
	self, err := os.Open(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	copyPath := filepath.Join(filepath.Dir(path), "fixture-child.test")
	copy, err := os.OpenFile(copyPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(copy, self); err != nil {
		t.Fatal(err)
	}
	_ = copy.Close()
	_ = self.Close()
	if err := writeFixtureFile(copyPath+".pause", []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	var instructions attackInstructions
	var b *Proc
	bDone := make(chan error, 1)
	ownerEndpoints := make(chan [3]int, 1)
	ctx := context.WithValue(t.Context(), launchFixtureKey{}, launchFixture{executable: copyPath, endpoints: func(pid, live, result int) { ownerEndpoints <- [3]int{os.Getpid(), live, result} }})
	bSpec := Spec{Path: os.Args[0], Args: []string{"-test.run=^TestManagedDescendantFixture$", "--", "held_exit_tree", path + ".b"}, Timeout: 30 * time.Second, AttemptID: AttemptID{8}}
	instructions.AttemptID = bSpec.AttemptID
	go func() { var err error; b, err = Start(ctx, bSpec, nil); bDone <- err }()
	if err := json.Unmarshal(fixtureFile(t, copyPath+".paused"), &instructions.Paused); err != nil {
		t.Fatal(err)
	}
	guardianFD, err := unix.PidfdOpen(instructions.Paused.PID, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(guardianFD) }()
	encoder := json.NewEncoder(inputWrite)
	if err := encoder.Encode(instructions); err != nil {
		t.Fatal(err)
	}
	_ = fixtureFile(t, path+".attacked")
	if err := unix.PidfdSendSignal(guardianFD, unix.SIGCONT, nil, 0); err != nil {
		t.Fatal(err)
	}
	if err := <-bDone; err != nil {
		t.Fatal(err)
	}
	endpoints := <-ownerEndpoints
	instructions.Owner, instructions.OwnerLive, instructions.OwnerResult = endpoints[0], endpoints[1], endpoints[2]
	if err := json.Unmarshal(fixtureFile(t, copyPath+".protected"), &instructions.GuardianRights); err != nil || len(instructions.GuardianRights) != 2 {
		t.Fatalf("guardian endpoint acknowledgement=%v %v", instructions.GuardianRights, err)
	}
	if err := encoder.Encode(instructions); err != nil {
		t.Fatal(err)
	}
	_ = fixtureFile(t, path+".protected_attacked")
	_ = fixtureFile(t, path+".b")
	_, _ = os.Stdout.WriteString(a.out.String())
	var report bootstrapControlReport
	if err := json.Unmarshal(fixtureFile(t, path+".controls"), &report); err != nil {
		t.Fatal(err)
	}
	for _, observed := range report.Controls {
		if observed.Errno != 0 {
			_, _ = fmt.Fprintf(os.Stdout, "CONTROL-UNAVAILABLE syscall=%s errno=%v profile=%s\n", observed.Name, observed.Errno, report.Profile)
		}
	}
	if acceptBootstrapControls(report) == nil {
		_, _ = os.Stdout.WriteString("CHILD-BOOT-CONTROLS-PASS controls=ptrace,pidfd_getfd,process_vm_readv,process_vm_writev,pipe_injection foreign_denied=true\n")
	}
	pids := []int{a.cmd.Process.Pid, b.cmd.Process.Pid}
	raw, _ := json.Marshal(pids)
	if err := writeFixtureFile(path+".ready", raw, 0o600); err != nil {
		t.Fatal(err)
	}
	_, _ = a.Wait()
}

func TestChildBootstrapAuthority(t *testing.T) {
	path := filepath.Join(t.TempDir(), "authority")
	owner := helperCommand("authority_owner", path)
	out := &capture{limit: Cap}
	owner.Stdout, owner.Stderr = out, out
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	stop := func() { once.Do(func() { _ = owner.Process.Kill(); _ = owner.Wait() }) }
	t.Cleanup(func() { stop(); t.Log(out.String()) })
	var guardians []int
	if err := json.Unmarshal(fixtureFile(t, path+".ready"), &guardians); err != nil || len(guardians) != 2 {
		t.Fatalf("owner readiness=%v %v output=%s", guardians, err, out.String())
	}
	var report bootstrapControlReport
	if err := json.Unmarshal(fixtureFile(t, path+".controls"), &report); err != nil {
		t.Fatal(err)
	}
	var handles []int
	for _, pid := range guardians {
		fd, err := unix.PidfdOpen(pid, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = unix.Close(fd) }()
		handles = append(handles, fd)
	}
	handles = append(handles, fixtureHandle(t, path+".descendant"), fixtureHandle(t, path+".b.pid"))
	stop()
	for _, fd := range handles {
		assertHandleStopped(t, fd)
	}
	t.Run("control_oracle", func(t *testing.T) { assertBootstrapControlOracle(t, report, out.String()) })
}

func TestChildEndpointAuthority(t *testing.T) { TestChildBootstrapAuthority(t) }

func TestChildStartupCancellationBeforeRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unreleased-target")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	checks := nativeOwnerChecks()
	reads := 0
	checks.policy = func() ([]byte, error) {
		reads++
		if reads == 2 {
			cancel()
		}
		return readManagedPolicy()
	}
	ctx = context.WithValue(ctx, launchFixtureKey{}, launchFixture{checks: &checks})
	p, err := Start(ctx, Spec{Path: os.Args[0], Args: []string{"-test.run=^TestManagedDescendantFixture$", "--", "marker", path}, Timeout: 30 * time.Second, AttemptID: AttemptID{22}}, nil)
	if p != nil {
		p.Stop()
		t.Fatal("canceled handoff returned a released attempt")
	}
	if reads != 2 || !errors.Is(err, ErrJobUnavailable) || errors.Is(err, ErrCompletionUnknown) {
		t.Fatalf("pre-release cancellation lost safe refusal: policy reads=%d err=%v", reads, err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("target executed before canceled handoff")
	}
}

func TestChildStartupCancellationAfterRelease(t *testing.T) {
	for _, evidence := range []string{"cleanup", "deadline", "guardian_lost"} {
		t.Run(evidence, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "startup-tree")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if evidence == "deadline" {
				var expire context.CancelFunc
				ctx, expire = context.WithTimeout(ctx, time.Second)
				defer expire()
			}
			var leader, descendant int
			fixture := launchFixture{acceptReady: func() {
				leader = fixtureHandle(t, path+".leader")
				descendant = fixtureHandle(t, path+".pid")
				_ = fixtureFile(t, path)
				t.Cleanup(func() {
					_ = unix.PidfdSendSignal(leader, unix.SIGKILL, nil, 0)
					_ = unix.PidfdSendSignal(descendant, unix.SIGKILL, nil, 0)
				})
				if evidence == "guardian_lost" {
					raw := fixtureFile(t, path+".guardian")
					pid, err := strconv.Atoi(string(raw))
					if err != nil {
						t.Fatal(err)
					}
					fd, err := unix.PidfdOpen(pid, 0)
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = unix.Close(fd) }()
					if err := unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0); err != nil {
						t.Fatal(err)
					}
				}
				if evidence == "deadline" {
					<-ctx.Done()
				} else {
					cancel()
				}
			}}
			ctx = context.WithValue(ctx, launchFixtureKey{}, fixture)
			started := time.Now()
			p, err := Start(ctx, Spec{Path: os.Args[0], Args: []string{"-test.run=^TestManagedDescendantFixture$", "--", "held_exit_tree", path}, Timeout: 30 * time.Second, AttemptID: AttemptID{21}}, nil)
			if p == nil || err != nil {
				t.Fatalf("CHILD-STARTUP-RELEASE: effects-possible cancellation lost its correlated process: %v", err)
			}
			result, err := p.Wait()
			if !errors.Is(err, ErrCompletionUnknown) || errors.Is(err, ErrJobUnavailable) || result.Completion.State != CompletionUnknown || result.Completion.AttemptID != (AttemptID{21}) {
				t.Fatalf("unaccepted readiness manufactured completion/refusal: %+v %v", result, err)
			}
			if time.Since(started) > 2*waitDelay {
				t.Fatal("startup cancellation exceeded startup plus cleanup bounds")
			}
			if evidence != "guardian_lost" {
				assertHandleReaped(t, leader)
				assertHandleReaped(t, descendant)
			}
		})
	}
}

func TestChildBootstrapProtocol(t *testing.T) {
	for _, creator := range []bool{false, true} {
		t.Run(fmt.Sprintf("socket_creator_substitution_%v", creator), func(t *testing.T) {
			pair, err := privatePair()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = unix.Close(pair[0]) }()
			remote := os.NewFile(uintptr(pair[1]), "sender bootstrap")
			defer func() { _ = remote.Close() }()
			cmd := helperCommand("bootstrap_sender", filepath.Join(t.TempDir(), "sender"))
			cmd.ExtraFiles = []*os.File{remote}
			cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), waitDelay)
			defer cancel()
			pid := cmd.Process.Pid
			if creator {
				pid = os.Getpid()
			}
			_, rights, err := receiveBootstrap(ctx, pair[0], pid, "managed", "bootstrap-ready", 0)
			if creator && err == nil || !creator && err != nil || len(rights) != 0 {
				t.Errorf("creator=%v readiness=%v", creator, err)
			}
			if err := cmd.Wait(); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, fault := range []string{"startup_hold", "release_fault"} {
		t.Run(fault, func(t *testing.T) {
			dir := t.TempDir()
			copyPath := filepath.Join(dir, "fixture-child.test")
			source, err := os.Open(os.Args[0])
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = source.Close() }()
			copy, err := os.OpenFile(copyPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.Copy(copy, source); err != nil {
				t.Fatal(err)
			}
			_ = copy.Close()
			if err := writeFixtureFile(copyPath+"."+fault, []byte("1"), 0o600); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "forbidden-marker")
			ctx := context.WithValue(t.Context(), launchFixtureKey{}, launchFixture{executable: copyPath})
			p, err := Start(ctx, Spec{Path: os.Args[0], Args: []string{"-test.run=^TestManagedDescendantFixture$", "--", "marker", path}, Timeout: 200 * time.Millisecond, AttemptID: AttemptID{18}}, nil)
			if fault == "startup_hold" {
				if p != nil {
					p.Stop()
					t.Fatal("interrupted startup returned readiness")
				}
				if err == nil {
					t.Fatal("interrupted startup reported success")
				}
			} else {
				if p == nil || err != nil {
					t.Fatalf("possible configuration delivery lost its attempt: %v", err)
				}
				result, err := p.Wait()
				if !errors.Is(err, ErrCompletionUnknown) || errors.Is(err, ErrJobUnavailable) || result.Completion.State != CompletionUnknown || result.Completion.AttemptID != (AttemptID{18}) {
					t.Fatalf("release refusal lacked correlated unknown evidence: %+v %v", result, err)
				}
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("target executed after startup/release refusal")
			}
		})
	}
	t.Run("descriptor_limit", func(t *testing.T) {
		cmd := helperCommand("descriptor_limit", filepath.Join(t.TempDir(), "limit"))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("descriptor-limit refusal: %v %s", err, out)
		}
	})
	pair, err := privatePair()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(pair[0]); _ = unix.Close(pair[1]) }()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := sendBootstrap(ctx, pair[1], childFrame{Version: 1, Role: "managed", Phase: "bootstrap-ready"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := receiveBootstrap(ctx, pair[0], os.Getpid(), "managed", "bootstrap-ready", 0); err != nil {
		t.Fatal(err)
	}
	if !unnamed(pair[0]) || !unnamed(pair[1]) {
		t.Fatal("bootstrap endpoint was autobound")
	}
	for _, kind := range []string{"empty", "oversize", "wrong_version", "wrong_role", "wrong_order", "foreign_attempt", "unknown_field", "truncated_json", "wrong_pid", "missing_credentials", "short_credentials", "short_rights", "rights_on_readiness", "extra_rights", "wrong_right_count"} {
		t.Run(kind, func(t *testing.T) {
			pair, err := privatePair()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = unix.Close(pair[0]); _ = unix.Close(pair[1]) }()
			frame := childFrame{Version: 1, Role: "managed", Phase: "bootstrap-ready"}
			if kind == "wrong_version" {
				frame.Version = 2
			}
			if kind == "wrong_role" {
				frame.Role = "diagnostic"
			}
			if kind == "wrong_order" {
				frame.Phase = "handoff"
			}
			if kind == "foreign_attempt" {
				frame.AttemptID = AttemptID{1}
			}
			if kind == "short_rights" {
				frame.Phase = "handoff"
				frame.StartupDeadlineUnixNano = time.Now().Add(waitDelay).UnixNano()
			}
			raw, _ := json.Marshal(frame)
			switch kind {
			case "empty":
				raw = nil
			case "oversize":
				raw = bytes.Repeat([]byte("x"), 1025)
			case "unknown_field":
				raw = append(raw[:len(raw)-1], []byte(",\"unknown\":true}")...)
			case "truncated_json":
				raw = raw[:len(raw)-1]
			}
			oob := unix.UnixCredentials(&unix.Ucred{Pid: int32(os.Getpid()), Uid: uint32(os.Geteuid()), Gid: uint32(os.Getegid())})
			if kind == "missing_credentials" {
				if err := unix.SetsockoptInt(pair[0], unix.SOL_SOCKET, unix.SO_PASSCRED, 0); err != nil {
					t.Fatal(err)
				}
				oob = nil
			}
			if kind == "short_credentials" {
				oob = oob[:len(oob)-8]
			}
			var files []*os.File
			if strings.Contains(kind, "rights") || kind == "wrong_right_count" {
				read, write, err := os.Pipe()
				if err != nil {
					t.Fatal(err)
				}
				files = []*os.File{read, write}
				defer func() {
					for _, file := range files {
						_ = file.Close()
					}
				}()
				fd, err := fileDescriptor(read)
				if err != nil {
					t.Fatal(err)
				}
				rights := []int{fd}
				if kind == "extra_rights" {
					rights = []int{fd, fd, fd, fd, fd, fd, fd, fd}
				}
				rawRights := unix.UnixRights(rights...)
				if kind == "short_rights" {
					header := (*unix.Cmsghdr)(unsafe.Pointer(&rawRights[0]))
					header.SetLen(unix.CmsgLen(3))
					malformed := append(append([]byte(nil), oob...), rawRights...)
					if _, _, _, valid := parseBootstrapControl(malformed); valid {
						t.Fatal("non-multiple ancillary rights accepted by parser")
					}
				}
				oob = append(oob, rawRights...)
			}
			before, err := os.ReadDir("/proc/self/fd")
			if err != nil {
				t.Fatal(err)
			}
			_, err = unix.SendmsgN(pair[1], raw, oob, nil, unix.MSG_NOSIGNAL)
			if err != nil {
				if kind != "short_credentials" && kind != "short_rights" || err != unix.EINVAL {
					t.Fatal(err)
				}
				return
			}
			pid := os.Getpid()
			if kind == "wrong_pid" {
				pid++
			}
			phase, count := "bootstrap-ready", 0
			if kind == "short_rights" {
				phase, count = "handoff", 3
			}
			_, rights, err := receiveBootstrap(t.Context(), pair[0], pid, "managed", phase, count)
			if err == nil {
				for _, fd := range rights {
					_ = unix.Close(fd)
				}
				t.Fatal("malformed bootstrap record was accepted")
			}
			after, err := os.ReadDir("/proc/self/fd")
			if err != nil {
				t.Fatal(err)
			}
			if len(after) != len(before) {
				t.Fatalf("installed rights leaked: before=%d after=%d", len(before), len(after))
			}
		})
	}
	for _, kind := range []string{"eof", "canceled", "expired", "expired_handoff", "extra_empty_record", "extra_record", "clean_handoff_eof"} {
		t.Run(kind, func(t *testing.T) {
			pair, err := privatePair()
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				_ = unix.Close(pair[0])
				if pair[1] >= 0 {
					_ = unix.Close(pair[1])
				}
			}()
			ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
			defer cancel()
			switch kind {
			case "eof":
				_ = unix.Close(pair[1])
				pair[1] = -1
			case "canceled":
				cancel()
			case "expired":
				ctx, cancel = context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
				defer cancel()
			case "expired_handoff":
				if err := sendBootstrap(ctx, pair[1], childFrame{Version: 1, Role: "managed", Phase: "handoff", StartupDeadlineUnixNano: time.Now().Add(-time.Second).UnixNano()}, nil); err != nil {
					t.Fatal(err)
				}
			case "extra_empty_record", "extra_record":
				raw := []byte{}
				if kind == "extra_record" {
					raw = []byte("x")
				}
				if _, err := unix.SendmsgN(pair[1], raw, unix.UnixCredentials(&unix.Ucred{Pid: int32(os.Getpid()), Uid: uint32(os.Geteuid()), Gid: uint32(os.Getegid())}), nil, unix.MSG_NOSIGNAL); err != nil {
					t.Fatal(err)
				}
				if bootstrapExhausted(pair[0], false) == nil {
					t.Fatal("extra record accepted")
				}
				return
			case "clean_handoff_eof":
				_ = unix.Close(pair[1])
				pair[1] = -1
				if err := bootstrapExhausted(pair[0], true); err != nil {
					t.Fatal(err)
				}
				return
			}
			phase := "bootstrap-ready"
			if kind == "expired_handoff" {
				phase = "handoff"
			}
			if _, rights, err := receiveBootstrap(ctx, pair[0], os.Getpid(), "managed", phase, 0); err == nil || len(rights) != 0 {
				t.Fatal("interrupted bootstrap accepted")
			}
		})
	}
	for _, kind := range []string{"duplicate_identity", "wrong_direction", "wrong_type"} {
		t.Run(kind, func(t *testing.T) {
			var files []*os.File
			defer func() {
				for _, file := range files {
					_ = file.Close()
				}
			}()
			var fds []int
			for range 3 {
				read, write, err := os.Pipe()
				if err != nil {
					t.Fatal(err)
				}
				files = append(files, read, write)
				var file *os.File
				if len(fds) == 2 {
					file = write
				} else {
					file = read
				}
				fd, err := fileDescriptor(file)
				if err != nil {
					t.Fatal(err)
				}
				fds = append(fds, fd)
			}
			if validateRights(fds, true) != nil {
				t.Fatal("valid descriptor control failed")
			}
			switch kind {
			case "duplicate_identity":
				fds[1] = fds[0]
			case "wrong_direction":
				fd, err := fileDescriptor(files[1])
				if err != nil {
					t.Fatal(err)
				}
				fds[0] = fd
			case "wrong_type":
				file, err := os.Open(os.DevNull)
				if err != nil {
					t.Fatal(err)
				}
				files = append(files, file)
				fd, err := fileDescriptor(file)
				if err != nil {
					t.Fatal(err)
				}
				fds[0] = fd
			}
			if validateRights(fds, true) == nil {
				t.Fatal("invalid handoff descriptors accepted")
			}
		})
	}
}

func TestChildOwnerPreparation(t *testing.T) {
	value, err := unix.PrctlRetInt(unix.PR_GET_DUMPABLE, 0, 0, 0, 0)
	if err != nil || value != 0 {
		t.Fatalf("owner dumpability=%d error=%v", value, err)
	}
	raw, err := os.ReadFile("/proc/sys/kernel/yama/ptrace_scope")
	if err != nil {
		t.Fatalf("managed policy read=%v", err)
	}
	t.Logf("managed policy=%q", raw)
	if err := launchRestrictions(false); err != nil {
		t.Fatalf("owner launch restrictions=%v", err)
	}
	if err := managedPolicy(); err != nil {
		t.Fatalf("managed policy admission=%v", err)
	}
	path := filepath.Join(t.TempDir(), "native-state")
	result, err := Run(t.Context(), Spec{Path: os.Args[0], Args: []string{"-test.run=^TestManagedDescendantFixture$", "--", "security_state", path}, Timeout: 5 * time.Second, AttemptID: AttemptID{19}})
	if err != nil || result.Exit != 0 || result.Completion.State != TreeQuiescent {
		t.Fatalf("target security state=%+v %v", result, err)
	}
	if string(fixtureFile(t, path)) != "nnp=1 forbidden_caps=0" {
		t.Fatal("target inherited forbidden security state")
	}
	if value, err := unix.PrctlRetInt(unix.PR_GET_DUMPABLE, 0, 0, 0, 0); err != nil || value != 0 {
		t.Fatal("completed run restored owner dumpability")
	}
}

func TestChildOwnerPreparationFaults(t *testing.T) {
	for _, fault := range []string{"control", "dumpability_readback", "effective_capability", "permitted_capability", "inheritable_capability", "capability_read_failure", "ptracer_refused", "privileges_refused", "privileges_readback", "policy_missing", "policy_zero", "policy_malformed", "handoff_recheck", "handoff_cancellation"} {
		t.Run(fault, func(t *testing.T) {
			checks := nativeOwnerChecks()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if strings.Contains(fault, "capability") {
				checks.capget = func(_ *unix.CapUserHeader, data *unix.CapUserData) error {
					if fault == "capability_read_failure" {
						return unix.EPERM
					}
					switch fault {
					case "effective_capability":
						data.Effective = 1 << unix.CAP_SYS_PTRACE
					case "permitted_capability":
						data.Permitted = 1 << unix.CAP_SYS_PTRACE
					case "inheritable_capability":
						data.Inheritable = 1 << unix.CAP_SYS_PTRACE
					}
					return nil
				}
			}
			if fault == "dumpability_readback" || fault == "privileges_readback" {
				native := checks.readInt
				checks.readInt = func(option int, a, b, c, d uintptr) (int, error) {
					if fault == "dumpability_readback" && option == unix.PR_GET_DUMPABLE {
						return 1, nil
					}
					if fault == "privileges_readback" && option == unix.PR_GET_NO_NEW_PRIVS {
						return 0, nil
					}
					return native(option, a, b, c, d)
				}
			}
			if fault == "ptracer_refused" || fault == "privileges_refused" {
				native := checks.prctl
				checks.prctl = func(option int, a, b, c, d uintptr) error {
					if fault == "ptracer_refused" && option == unix.PR_SET_PTRACER || fault == "privileges_refused" && option == unix.PR_SET_NO_NEW_PRIVS {
						return unix.EPERM
					}
					return native(option, a, b, c, d)
				}
			}
			calls := 0
			switch fault {
			case "policy_missing":
				checks.policy = func() ([]byte, error) { return nil, os.ErrNotExist }
			case "policy_zero":
				checks.policy = func() ([]byte, error) { return []byte("0\n"), nil }
			case "policy_malformed":
				checks.policy = func() ([]byte, error) { return []byte("1 trailing"), nil }
			case "handoff_recheck", "handoff_cancellation":
				checks.policy = func() ([]byte, error) {
					calls++
					if calls == 2 {
						if fault == "handoff_cancellation" {
							cancel()
						} else {
							return []byte("0\n"), nil
						}
					}
					return []byte("1\n"), nil
				}
			}
			path := filepath.Join(t.TempDir(), "target-marker")
			ctx = context.WithValue(ctx, launchFixtureKey{}, launchFixture{checks: &checks})
			p, err := Start(ctx, Spec{Path: os.Args[0], Args: []string{"-test.run=^TestManagedDescendantFixture$", "--", "marker", path}, Timeout: 10 * time.Second, AttemptID: AttemptID{15}}, nil)
			if fault == "control" {
				if err != nil {
					t.Fatal(err)
				}
				res, err := p.Wait()
				if err != nil || res.Completion.State != TreeQuiescent {
					t.Fatalf("native preparation control=%+v %v", res, err)
				}
				if string(fixtureFile(t, path)) != "executed" {
					t.Fatal("preparation control target did not execute")
				}
			} else {
				if p != nil {
					p.Stop()
					t.Fatal("failed preparation/recheck admitted a target")
				}
				if !errors.Is(err, ErrJobUnavailable) && !errors.Is(err, context.Canceled) {
					t.Fatalf("preparation failure=%v", err)
				}
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("target effect occurred after preparation refusal")
				}
			}
		})
	}
}

func TestManagedDescendantFixture(t *testing.T) {
	mode := helperMode()
	if mode == "security_state" {
		if value, err := unix.PrctlRetInt(unix.PR_GET_NO_NEW_PRIVS, 0, 0, 0, 0); err != nil || value != 1 {
			t.Fatal("target inherited no-new-privileges=0")
		}
		var data [2]unix.CapUserData
		header := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
		if err := unix.Capget(&header, &data[0]); err != nil {
			t.Fatal(err)
		}
		mask := uint32(1 << unix.CAP_SYS_PTRACE)
		if (data[0].Effective|data[0].Permitted|data[0].Inheritable)&mask != 0 {
			t.Fatal("target inherited ptrace capability")
		}
		if err := writeFixtureFile(os.Args[len(os.Args)-1], []byte("nnp=1 forbidden_caps=0"), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	if mode == "marker" {
		if err := writeFixtureFile(os.Args[len(os.Args)-1], []byte("executed"), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	if mode != "normal_exit_tree" && mode != "detached_exit_tree" && mode != "double_fork_tree" && mode != "fork_intermediate" && mode != "nonzero_tree" && mode != "generation_tree" && mode != "generation_parent" && mode != "held_exit_tree" && mode != "temporary_owner" && mode != "acknowledged_beat" {
		return
	}
	path := os.Args[len(os.Args)-1]
	if mode == "fork_intermediate" {
		ack := os.NewFile(3, "intermediate acknowledgement")
		cmd := exec.Command(os.Args[0], "-test.run=^TestManagedDescendantFixture$", "--", "acknowledged_beat", path)
		cmd.Env = []string{Marker}
		cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
		cmd.ExtraFiles = []*os.File{ack}
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		_ = ack.Close()
		return
	}
	if mode == "temporary_owner" {
		if err := PrepareManagedOwner(); err != nil {
			t.Fatal(err)
		}
		p, err := Start(t.Context(), Spec{Path: os.Args[0], Args: []string{"-test.run=^TestManagedDescendantFixture$", "--", "held_exit_tree", path}, Timeout: 30 * time.Second, AttemptID: AttemptID{3}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := writeFixtureFile(path+".guardian", []byte(strconv.Itoa(p.cmd.Process.Pid)), 0o600); err != nil {
			t.Fatal(err)
		}
		_, _ = p.Wait()
		return
	}
	if mode == "normal_exit_tree" || mode == "held_exit_tree" || mode == "detached_exit_tree" || mode == "double_fork_tree" || mode == "nonzero_tree" || mode == "generation_tree" {
		if err := writeFixtureFile(path+".guardian", []byte(strconv.Itoa(os.Getppid())), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := writeFixtureFile(path+".leader", []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
			t.Fatal(err)
		}
		read, write, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		descendantMode := "acknowledged_beat"
		if mode == "double_fork_tree" {
			descendantMode = "fork_intermediate"
		}
		if mode == "generation_tree" {
			descendantMode = "generation_parent"
		}
		cmd := exec.Command(os.Args[0], "-test.run=^TestManagedDescendantFixture$", "--", descendantMode, path)
		cmd.Env = []string{Marker}
		cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
		cmd.ExtraFiles = []*os.File{write}
		if mode == "detached_exit_tree" || mode == "generation_tree" {
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		_ = write.Close()
		if _, err := io.ReadFull(read, make([]byte, 1)); err != nil {
			t.Fatal(err)
		}
		_ = read.Close()
		if mode == "double_fork_tree" {
			if err := cmd.Wait(); err != nil {
				t.Fatal(err)
			}
		}
		if mode == "held_exit_tree" {
			time.Sleep(time.Minute)
		}
		if mode == "normal_exit_tree" || mode == "detached_exit_tree" || mode == "double_fork_tree" || mode == "nonzero_tree" {
			_ = fixtureFile(t, path+".completion_pinned")
		}
		if mode == "nonzero_tree" {
			t.Error("controlled target failure")
		}
		return
	}
	if err := writeFixtureFile(path+".pid", []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	if mode == "generation_parent" {
		group, err := unix.Getpgid(0)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal([]int{os.Getpid(), group})
		if err := writeFixtureFile(path+".parent_group", raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	beacon, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = beacon.Close() }()
	if _, err := beacon.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	ack := os.NewFile(3, "acknowledgement")
	_, _ = ack.Write([]byte{1})
	_ = ack.Close()
	if mode == "generation_parent" {
		_ = fixtureFile(t, path+".cleanup")
		child := startHeartbeat(path + ".generation_heartbeat")
		if child == nil {
			t.Fatal("second generation did not start")
		}
		_ = fixtureFile(t, path+".generation_heartbeat")
		if err := writeFixtureFile(path+".generation_pid", []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := writeFixtureFile(path+".generation_ack", []byte("1"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for range ticker.C {
		if _, err := beacon.Write([]byte("x")); err != nil {
			return
		}
	}
}

func fixtureFile(t *testing.T, path string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	raw, err := waitFixtureBytes(ctx, path)
	if err != nil {
		t.Fatalf("fixture acknowledgement %s: %v", filepath.Base(path), err)
	}
	return raw
}

func waitFixtureBytes(ctx context.Context, path string) ([]byte, error) {
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return nil, err
	}
	defer func() { _ = unix.Close(fd) }()
	if _, err := unix.InotifyAddWatch(fd, filepath.Dir(path), unix.IN_CLOSE_WRITE|unix.IN_MOVED_TO|unix.IN_MODIFY); err != nil {
		return nil, err
	}
	for {
		if raw, err := os.ReadFile(path); err == nil && len(raw) > 0 {
			return raw, nil
		}
		if err := socketWait(ctx, fd, unix.POLLIN); err != nil {
			return nil, err
		}
		var events [4096]byte
		if _, err := unix.Read(fd, events[:]); err != nil && err != unix.EAGAIN {
			return nil, err
		}
	}
}

func fixtureHandle(t *testing.T, path string) int {
	t.Helper()
	pid, err := strconv.Atoi(string(fixtureFile(t, path)))
	if err != nil {
		t.Fatal(err)
	}
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(fd) })
	return fd
}

func assertHandleStopped(t *testing.T, fd int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := socketWait(ctx, fd, unix.POLLIN); err != nil {
		t.Fatalf("acknowledged process still live: %v", err)
	}
}

func assertHandleReaped(t *testing.T, fd int) {
	t.Helper()
	fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	if n, err := unix.Poll(fds, 0); err != nil || n != 1 {
		t.Fatalf("CHILD-FINAL-REAP: acknowledged identity not finally reaped: n=%d events=%#x err=%v", n, fds[0].Revents, err)
	}
	if err := unix.PidfdSendSignal(fd, 0, nil, 0); err != unix.ESRCH {
		t.Fatalf("CHILD-FINAL-REAP: acknowledged identity still exists (including zombie): %v", err)
	}
}

func TestChildOwnerDeath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "owner-heartbeat")
	owner := exec.Command(os.Args[0], "-test.run=^TestManagedDescendantFixture$", "--", "temporary_owner", path)
	owner.Env = []string{Marker}
	owner.Stdout, owner.Stderr = io.Discard, io.Discard
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	stop := func() { once.Do(func() { _ = owner.Process.Kill(); _ = owner.Wait() }) }
	t.Cleanup(stop)
	guardian := fixtureHandle(t, path+".guardian")
	descendant := fixtureHandle(t, path+".pid")
	if len(fixtureFile(t, path)) == 0 {
		t.Fatal("descendant had no live effect acknowledgement")
	}
	stop()
	assertHandleStopped(t, descendant)
	assertHandleStopped(t, guardian)
	t.Log("CHILD-OWNER-EOF guardian_exited=true acknowledged_descendant_stopped=true terminal_frame=unobservable_after_owner_death")
}

func TestChildConcurrentWaitStop(t *testing.T) {
	path := filepath.Join(t.TempDir(), "concurrent-heartbeat")
	spec := Spec{Path: os.Args[0], Args: []string{"-test.run=^TestManagedDescendantFixture$", "--", "held_exit_tree", path}, Timeout: 30 * time.Second, AttemptID: AttemptID{4}}
	p, err := Start(t.Context(), spec, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = fixtureFile(t, path)
	results := make(chan Result, 64)
	errs := make(chan error, 64)
	var workers sync.WaitGroup
	for range 64 {
		workers.Add(2)
		go func() { defer workers.Done(); r, e := p.Wait(); results <- r; errs <- e }()
		go func() { defer workers.Done(); p.Stop() }()
	}
	workers.Wait()
	close(results)
	close(errs)
	want, err := p.Wait()
	if err != nil || want.Completion.State != TreeQuiescent {
		t.Fatalf("cached completion=%+v %v", want, err)
	}
	for result := range results {
		if result != want {
			t.Fatalf("Wait returned a different result: %+v want %+v", result, want)
		}
	}
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	unrelated := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$", "--", "beat", filepath.Join(t.TempDir(), "unrelated"))
	unrelated.Env = []string{Marker}
	unrelated.Stdout, unrelated.Stderr = io.Discard, io.Discard
	unrelated.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := unrelated.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unrelated.Process.Kill(); _ = unrelated.Wait() }()
	fd, err := unix.PidfdOpen(unrelated.Process.Pid, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(fd) }()
	reused, err := os.FindProcess(unrelated.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reused.Release() }()
	p.cmd.Process = reused
	for range 64 {
		p.Stop()
		if _, err := p.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	if n, err := unix.Poll(poll, 0); err != nil || n != 0 {
		t.Fatalf("Stop signaled a replacement/unrelated identity: n=%d err=%v", n, err)
	}
}

func TestChildObservationFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unknown-heartbeat")
	p, err := Start(t.Context(), Spec{Path: os.Args[0], Args: []string{"-test.run=^TestManagedDescendantFixture$", "--", "held_exit_tree", path}, Timeout: 30 * time.Second, AttemptID: AttemptID{5}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	leader := fixtureHandle(t, path+".leader")
	descendant := fixtureHandle(t, path+".pid")
	_ = fixtureFile(t, path)
	defer func() {
		_ = unix.PidfdSendSignal(leader, unix.SIGKILL, nil, 0)
		_ = unix.PidfdSendSignal(descendant, unix.SIGKILL, nil, 0)
	}()
	if err := p.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	result, err := p.Wait()
	if !errors.Is(err, ErrCompletionUnknown) || result.Completion.State != CompletionUnknown || result.Completion.AttemptID != (AttemptID{5}) {
		t.Fatalf("helper death manufactured completion=%+v err=%v", result, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := reapTree(ctx); !errors.Is(err, ErrCompletionUnknown) {
		t.Fatalf("canceled cleanup=%v", err)
	}
	t.Run("native_observation_faults", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "owned-observation")
		child := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$", "--", "beat", path)
		child.Env = []string{Marker}
		child.Stdout, child.Stderr = io.Discard, io.Discard
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
		_ = fixtureFile(t, path)
		fd, err := unix.PidfdOpen(child.Process.Pid, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = unix.Close(fd) }()
		for _, fault := range []string{"missing", "partial", "empty", "pin_failure", "signal_failure", "deadline"} {
			t.Run(fault, func(t *testing.T) {
				observation := nativeTreeObservation()
				switch fault {
				case "missing":
					observation.children = func() ([]int, error) { return nil, os.ErrNotExist }
				case "partial":
					observation.children = func() ([]int, error) { return []int{child.Process.Pid}, io.ErrUnexpectedEOF }
				case "empty":
					observation.children = func() ([]int, error) { return nil, nil }
				case "pin_failure":
					observation.pin = func(int, int) (int, error) { return -1, unix.EMFILE }
				case "signal_failure":
					observation.signal = func(int, unix.Signal, *unix.Siginfo, int) error { return unix.EPERM }
				}
				ctx := t.Context()
				if fault == "deadline" {
					var cancel context.CancelFunc
					ctx, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
					defer cancel()
				}
				if err := reapObservedTree(ctx, observation); !errors.Is(err, ErrCompletionUnknown) {
					t.Fatalf("observation %s manufactured quiescence: %v", fault, err)
				}
				poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
				if n, err := unix.Poll(poll, 0); err != nil || n != 0 {
					t.Fatalf("fault fixture lost its live owned control: %d %v", n, err)
				}
			})
		}
	})
	for _, withheld := range []bool{false, true} {
		t.Run(fmt.Sprintf("final_echild_withheld_%v", withheld), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "reaping-control")
			child := helperCommand("beat", path)
			child.Args[1] = "-test.run=^TestHelperProcess$"
			child.Stdout, child.Stderr = io.Discard, io.Discard
			if err := child.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
			_ = fixtureFile(t, path)
			fd, err := unix.PidfdOpen(child.Process.Pid, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = unix.Close(fd) }()
			observation := nativeTreeObservation()
			observed, zombie, reaped, echild := false, false, false, false
			observation.children = func() ([]int, error) {
				children, err := ownedChildren()
				for _, pid := range children {
					if pid == child.Process.Pid {
						observed = true
					}
				}
				return children, err
			}
			observation.wait = func(pid int, status *unix.WaitStatus, options int, usage *unix.Rusage) (int, error) {
				if pid == child.Process.Pid && options == 0 {
					if err := unix.PidfdSendSignal(fd, 0, nil, 0); err != nil {
						t.Fatalf("terminated unreaped control lost its identity: %v", err)
					}
					zombie = true
				}
				n, err := unix.Wait4(pid, status, options, usage)
				if n == child.Process.Pid {
					reaped = true
				}
				if err == unix.ECHILD {
					echild = true
					if withheld {
						return 0, nil
					}
				}
				return n, err
			}
			ctx, cancel := context.WithTimeout(t.Context(), waitDelay)
			defer cancel()
			err = reapObservedTree(ctx, observation)
			if withheld && !errors.Is(err, ErrCompletionUnknown) || !withheld && err != nil {
				t.Fatalf("final ECHILD withheld=%v completion=%v", withheld, err)
			}
			if !observed || !zombie || !reaped || !echild {
				t.Fatalf("CHILD-ECHILD-EVIDENCE: real nonempty cleanup/ECHILD not exercised: observed=%v zombie=%v reaped=%v echild=%v", observed, zombie, reaped, echild)
			}
			assertHandleReaped(t, fd)
		})
	}
	for _, fault := range []string{"helper_zero", "truncated", "malformed", "foreign_attempt", "foreign_role", "unknown_completion", "extra_frame", "retained_writer"} {
		t.Run(fault, func(t *testing.T) {
			dir := t.TempDir()
			copyPath := filepath.Join(dir, "fixture-child.test")
			source, err := os.Open(os.Args[0])
			if err != nil {
				t.Fatal(err)
			}
			copy, err := os.OpenFile(copyPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.Copy(copy, source); err != nil {
				t.Fatal(err)
			}
			_ = copy.Close()
			_ = source.Close()
			frame := childFrame{Version: 1, Role: "managed", Phase: "terminal", AttemptID: AttemptID{5}, Completion: TreeQuiescent, CleanupDeadlineUnixNano: time.Now().Add(waitDelay).UnixNano()}
			if fault == "foreign_attempt" {
				frame.AttemptID = AttemptID{99}
			}
			if fault == "foreign_role" {
				frame.Role = "diagnostic"
			}
			if fault == "unknown_completion" {
				frame.Completion = CompletionUnknown
			}
			if fault == "retained_writer" {
				frame.CleanupDeadlineUnixNano = time.Now().Add(200 * time.Millisecond).UnixNano()
				if err := writeFixtureFile(copyPath+".retain_writer", []byte("1"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			raw, _ := json.Marshal(frame)
			raw = append(raw, '\n')
			switch fault {
			case "helper_zero":
				raw = nil
			case "truncated":
				raw = raw[:len(raw)/2]
			case "malformed":
				raw = []byte("not-json\n")
			case "extra_frame":
				raw = append(raw, raw...)
			}
			if err := writeFixtureFile(copyPath+".terminal_payload", raw, 0o600); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "unexecuted")
			ctx := context.WithValue(t.Context(), launchFixtureKey{}, launchFixture{executable: copyPath})
			p, err := Start(ctx, Spec{Path: os.Args[0], Args: []string{"-test.run=^TestManagedDescendantFixture$", "--", "marker", path}, Timeout: 10 * time.Second, AttemptID: AttemptID{5}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			res, err := p.Wait()
			if !errors.Is(err, ErrCompletionUnknown) || res.Completion.State != CompletionUnknown || res.Completion.AttemptID != (AttemptID{5}) {
				t.Fatalf("terminal fault %s manufactured completion: %+v %v", fault, res, err)
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("faulted helper executed an unvalidated target")
			}
		})
	}
}

func TestChildManagedCompletion(t *testing.T) {
	for _, mode := range []string{"normal_exit_tree", "detached_exit_tree", "double_fork_tree", "nonzero_tree"} {
		t.Run(mode, func(t *testing.T) { assertManagedCompletion(t, mode) })
	}
	t.Run("generation_during_cleanup", func(t *testing.T) {
		dir := t.TempDir()
		copyPath := filepath.Join(dir, "fixture-child.test")
		source, err := os.Open(os.Args[0])
		if err != nil {
			t.Fatal(err)
		}
		copy, err := os.OpenFile(copyPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(copy, source); err != nil {
			t.Fatal(err)
		}
		_ = copy.Close()
		_ = source.Close()
		path := filepath.Join(dir, "generation")
		if err := writeFixtureFile(copyPath+".generation", []byte(path), 0o600); err != nil {
			t.Fatal(err)
		}
		ctx := context.WithValue(t.Context(), launchFixtureKey{}, launchFixture{executable: copyPath})
		p, err := Start(ctx, Spec{Path: os.Args[0], Args: []string{"-test.run=^TestManagedDescendantFixture$", "--", "generation_tree", path}, Timeout: 30 * time.Second, AttemptID: AttemptID{12}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		generation := fixtureHandle(t, path+".generation_pid")
		var group []int
		if err := json.Unmarshal(fixtureFile(t, path+".parent_group"), &group); err != nil || len(group) != 2 || group[0] != group[1] {
			t.Fatalf("cleanup-generation parent never detached: %v %v", group, err)
		}
		if err := writeFixtureFile(path+".generation_pinned", []byte("1"), 0o600); err != nil {
			t.Fatal(err)
		}
		res, err := p.Wait()
		if err != nil || res.Completion.State != TreeQuiescent || res.Exit != 0 {
			t.Fatalf("cleanup generation=%+v %v", res, err)
		}
		assertHandleReaped(t, generation)
	})
}

func assertManagedCompletion(t *testing.T, mode string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "heartbeat")
	spec := Spec{Path: os.Args[0], Args: []string{"-test.run=^TestManagedDescendantFixture$", "--", mode, path}, Timeout: 30 * time.Second, AttemptID: AttemptID{2}}
	p, err := Start(t.Context(), spec, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Stop)
	leader := fixtureHandle(t, path+".leader")
	descendant := fixtureHandle(t, path+".pid")
	_ = fixtureFile(t, path)
	if err := writeFixtureFile(path+".completion_pinned", []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := p.Wait()
	if err != nil {
		t.Fatal(err)
	}
	wantExit := 0
	if mode == "nonzero_tree" {
		wantExit = 1
	}
	if res.Exit != wantExit {
		t.Fatalf("acknowledged leader exit=%d output=%s", res.Exit, res.Output)
	}
	if res.Completion.AttemptID != spec.AttemptID || res.Completion.State != TreeQuiescent {
		t.Fatalf("tree completion=%+v", res.Completion)
	}
	if fileSize(t, path) == 0 {
		t.Fatal("descendant never acknowledged live effects")
	}
	fds := []unix.PollFd{{Fd: int32(descendant), Events: unix.POLLIN}}
	if n, err := unix.Poll(fds, 0); err != nil || n == 0 {
		t.Fatal("CHILD-LEADER-ONLY-COMPLETION: Wait returned while its acknowledged heartbeat descendant was alive")
	}
	assertHandleReaped(t, leader)
	assertHandleReaped(t, descendant)
}
