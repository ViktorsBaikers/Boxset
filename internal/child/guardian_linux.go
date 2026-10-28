//go:build linux

package child

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/ViktorsBaikers/Boxset/internal/logs"
	"golang.org/x/sys/unix"
)

const childEntry = "__boxset-child"
const protocolVersion = 1
const configLimit = 64 << 10
const frameLimit = 1024

type launchFixtureKey struct{}

type launchFixture struct {
	executable  string
	endpoints   func(int, int, int)
	checks      *ownerChecks
	acceptReady func()
}

type ownerChecks struct {
	prctl   func(int, uintptr, uintptr, uintptr, uintptr) error
	readInt func(int, uintptr, uintptr, uintptr, uintptr) (int, error)
	capget  func(*unix.CapUserHeader, *unix.CapUserData) error
	policy  func() ([]byte, error)
}

func nativeOwnerChecks() ownerChecks {
	return ownerChecks{prctl: unix.Prctl, readInt: unix.PrctlRetInt, capget: unix.Capget, policy: readManagedPolicy}
}

type childConfig struct {
	Version   int
	Role      string
	Spec      Spec
	Deadline  int64
	Directory string
	Input     bool
}

type childFrame struct {
	Version                 int
	Role                    string
	Phase                   string
	AttemptID               AttemptID
	Exit                    int
	TimedOut                bool
	Completion              CompletionState
	StartupDeadlineUnixNano int64
	CleanupDeadlineUnixNano int64
}

// PrepareManagedOwner protects every private endpoint for the owner's lifetime.
func PrepareManagedOwner() error {
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		return ErrJobUnavailable
	}
	return verifyOwner()
}

func verifyOwner() error { return verifyOwnerWith(nativeOwnerChecks()) }

func verifyOwnerWith(checks ownerChecks) error {
	v, err := checks.readInt(unix.PR_GET_DUMPABLE, 0, 0, 0, 0)
	if err != nil || v != 0 {
		return ErrJobUnavailable
	}
	var caps [2]unix.CapUserData
	header := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	if err := checks.capget(&header, &caps[0]); err != nil {
		return ErrJobUnavailable
	}
	mask := uint32(1) << uint(unix.CAP_SYS_PTRACE)
	if (caps[0].Effective|caps[0].Permitted|caps[0].Inheritable)&mask != 0 {
		return ErrJobUnavailable
	}
	return nil
}

func launchRestrictions(managed bool) error {
	return launchRestrictionsWith(managed, nativeOwnerChecks())
}

func launchRestrictionsWith(managed bool, checks ownerChecks) error {
	if err := verifyOwnerWith(checks); err != nil {
		return errors.Join(err, errors.New("owner_not_prepared"))
	}
	if err := checks.prctl(unix.PR_SET_PTRACER, 0, 0, 0, 0); err != nil {
		return errors.Join(ErrJobUnavailable, errors.New("ptracer_clear"))
	}
	if err := checks.prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return errors.Join(ErrJobUnavailable, errors.New("no_new_privileges"))
	}
	v, err := checks.readInt(unix.PR_GET_NO_NEW_PRIVS, 0, 0, 0, 0)
	if err != nil || v != 1 {
		return errors.Join(ErrJobUnavailable, errors.New("no_new_privileges_readback"))
	}
	if managed {
		return managedPolicyWith(checks.policy)
	}
	return nil
}

func managedPolicy() error {
	return managedPolicyWith(readManagedPolicy)
}

func readManagedPolicy() ([]byte, error) {
	f, err := os.Open("/proc/sys/kernel/yama/ptrace_scope")
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(f, 16))
	_ = f.Close()
	return raw, err
}

func managedPolicyWith(read func() ([]byte, error)) error {
	raw, err := read()
	if err != nil || len(raw) > 2 || (string(raw) != "1\n" && string(raw) != "2\n" && string(raw) != "3\n" && string(raw) != "1" && string(raw) != "2" && string(raw) != "3") {
		return ErrJobUnavailable
	}
	return nil
}

func startManaged(ctx context.Context, spec Spec, input *os.File) (*Proc, error) {
	ctx, cancel := context.WithTimeout(ctx, spec.Timeout)
	config := childConfig{Version: protocolVersion, Role: "managed", Spec: spec, Input: input != nil}
	deadline, _ := ctx.Deadline()
	config.Deadline = deadline.UnixNano()
	p, err := launchGuardian(ctx, cancel, config, input)
	if err != nil {
		cancel()
		return nil, err
	}
	return p, nil
}

func startCrashDiagnostic(ctx context.Context, dir string, input *os.File) (*Proc, error) {
	ctx, cancel := context.WithCancel(ctx)
	p, err := launchGuardian(ctx, cancel, childConfig{Version: protocolVersion, Role: "diagnostic", Directory: dir, Input: true}, input)
	if err != nil {
		cancel()
		return nil, err
	}
	return p, nil
}

func privatePair() ([2]int, error) {
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, 0)
	if err != nil {
		return pair, err
	}
	err = unix.SetsockoptInt(pair[0], unix.SOL_SOCKET, unix.SO_PASSCRED, 1)
	if err != nil {
		_ = unix.Close(pair[0])
		_ = unix.Close(pair[1])
	}
	return pair, err
}

func unnamed(fd int) bool {
	var address unix.RawSockaddrUnix
	length := uint32(unsafe.Sizeof(address))
	_, _, err := unix.Syscall(unix.SYS_GETSOCKNAME, uintptr(fd), uintptr(unsafe.Pointer(&address)), uintptr(unsafe.Pointer(&length)))
	return err == 0 && length == uint32(unsafe.Offsetof(address.Path)) && address.Family == unix.AF_UNIX
}

func socketWait(ctx context.Context, fd int, event int16) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		wait := 100
		if deadline, ok := ctx.Deadline(); ok {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return context.DeadlineExceeded
			}
			wait = min(wait, max(1, int(remaining/time.Millisecond)))
		}
		poll := []unix.PollFd{{Fd: int32(fd), Events: event}}
		n, err := unix.Poll(poll, wait)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return err
		}
		if n > 0 {
			if poll[0].Revents&unix.POLLNVAL != 0 {
				return ErrJobUnavailable
			}
			return nil
		}
	}
}

func sendBootstrap(ctx context.Context, fd int, frame childFrame, rights []int) error {
	if !unnamed(fd) {
		return ErrJobUnavailable
	}
	raw, err := json.Marshal(frame)
	if err != nil || len(raw) == 0 || len(raw) > frameLimit {
		return ErrJobUnavailable
	}
	oob := unix.UnixCredentials(&unix.Ucred{Pid: int32(os.Getpid()), Uid: uint32(os.Geteuid()), Gid: uint32(os.Getegid())})
	if len(rights) > 0 {
		oob = append(oob, unix.UnixRights(rights...)...)
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := unix.SendmsgN(fd, raw, oob, nil, unix.MSG_NOSIGNAL)
		if err == unix.EAGAIN || err == unix.EINTR {
			if err = socketWait(ctx, fd, unix.POLLOUT); err != nil {
				return err
			}
			continue
		}
		if err != nil || n != len(raw) || !unnamed(fd) {
			return ErrJobUnavailable
		}
		return nil
	}
}

func bootstrapExhausted(fd int, allowEOF bool) error {
	var payload [1]byte
	n, _, flags, _, err := unix.Recvmsg(fd, payload[:], nil, unix.MSG_PEEK|unix.MSG_DONTWAIT|unix.MSG_CMSG_CLOEXEC)
	if err == unix.EAGAIN {
		return nil
	}
	if err != nil || n > 0 || flags&(unix.MSG_TRUNC|unix.MSG_CTRUNC) != 0 || !allowEOF {
		return ErrJobUnavailable
	}
	return nil
}

func receiveBootstrap(ctx context.Context, fd int, pid int, role, phase string, rightsCount int) (frame childFrame, rights []int, err error) {
	payload := make([]byte, frameLimit+1)
	oob := make([]byte, unix.CmsgSpace(unix.SizeofUcred)+unix.CmsgSpace(4*4))
	var n, control, flags int
	for {
		if err = ctx.Err(); err != nil {
			return frame, nil, err
		}
		n, control, flags, _, err = unix.Recvmsg(fd, payload, oob, unix.MSG_CMSG_CLOEXEC)
		if err != unix.EAGAIN && err != unix.EINTR {
			break
		}
		if err = socketWait(ctx, fd, unix.POLLIN); err != nil {
			return frame, nil, err
		}
	}
	if err != nil {
		return frame, nil, err
	}
	rights, cred, arrays, valid := parseBootstrapControl(oob[:control])
	defer func() {
		if err != nil {
			for _, right := range rights {
				_ = unix.Close(right)
			}
			rights = nil
		}
	}()
	if !valid || cred == nil || cred.Pid != int32(pid) || cred.Uid != uint32(os.Geteuid()) || cred.Gid != uint32(os.Getegid()) || flags&(unix.MSG_TRUNC|unix.MSG_CTRUNC) != 0 || n < 1 || n > frameLimit || !unnamed(fd) {
		return frame, rights, ErrJobUnavailable
	}
	wantRights := len(rights) == rightsCount || rightsCount == 3 && len(rights) == 4
	if rightsCount == 0 && (arrays != 0 || len(rights) != 0) || rightsCount > 0 && (arrays != 1 || !wantRights) {
		return frame, rights, ErrJobUnavailable
	}
	if err = decodeStrict(payload[:n], &frame); err != nil {
		return frame, rights, ErrJobUnavailable
	}
	if frame.Version != protocolVersion || frame.Role != role || frame.Phase != phase || frame.AttemptID != (AttemptID{}) || frame.Completion != "" || frame.Exit != 0 || frame.TimedOut || frame.CleanupDeadlineUnixNano != 0 {
		return frame, rights, ErrJobUnavailable
	}
	if phase == "handoff" {
		if frame.StartupDeadlineUnixNano <= time.Now().UnixNano() {
			return frame, rights, ErrJobUnavailable
		}
	} else if frame.StartupDeadlineUnixNano != 0 {
		return frame, rights, ErrJobUnavailable
	}
	return frame, rights, nil
}

func parseBootstrapControl(oob []byte) (rights []int, cred *unix.Ucred, arrays int, valid bool) {
	control := len(oob)
	valid = true
	credentials, messages := 0, 0
	for offset := 0; offset < control; {
		if control-offset < unix.CmsgLen(0) {
			valid = false
			break
		}
		header := *(*unix.Cmsghdr)(unsafe.Pointer(&oob[offset]))
		length := int(header.Len)
		if length < unix.CmsgLen(0) || length > control-offset {
			valid = false
			break
		}
		body := oob[offset+unix.CmsgLen(0) : offset+length]
		messages++
		if header.Level != unix.SOL_SOCKET {
			valid = false
		}
		switch header.Type {
		case unix.SCM_RIGHTS:
			arrays++
			for i := 0; i+4 <= len(body); i += 4 {
				rights = append(rights, int(int32(binary.NativeEndian.Uint32(body[i:i+4]))))
			}
			if len(body)%4 != 0 || len(body) == 0 || len(body) > 16 {
				valid = false
			}
		case unix.SCM_CREDENTIALS:
			credentials++
			if len(body) != unix.SizeofUcred {
				valid = false
			} else {
				value := *(*unix.Ucred)(unsafe.Pointer(&body[0]))
				cred = &value
			}
		default:
			valid = false
		}
		offset += unix.CmsgSpace(len(body))
	}
	return rights, cred, arrays, valid && messages <= 2 && credentials == 1
}

func decodeStrict(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return ErrJobUnavailable
	}
	return nil
}

func pipeFrame(file *os.File, frame childFrame) error {
	raw, err := json.Marshal(frame)
	if err != nil || len(raw)+1 > frameLimit {
		return ErrCompletionUnknown
	}
	raw = append(raw, '\n')
	n, err := file.Write(raw)
	if err != nil || n != len(raw) {
		return ErrCompletionUnknown
	}
	return nil
}

func readFrame(reader *bufio.Reader, role, phase string, attempt AttemptID) (childFrame, error) {
	var frame childFrame
	raw, err := reader.ReadSlice('\n')
	if err != nil || len(raw) > frameLimit || decodeStrict(raw, &frame) != nil {
		return frame, ErrCompletionUnknown
	}
	if frame.Version != protocolVersion || frame.Role != role || frame.Phase != phase || frame.AttemptID != attempt {
		return frame, ErrCompletionUnknown
	}
	return frame, nil
}

func launchGuardian(ctx context.Context, cancel context.CancelFunc, config childConfig, input *os.File) (proc *Proc, err error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	managed := config.Role == "managed"
	fixture, _ := ctx.Value(launchFixtureKey{}).(launchFixture)
	checks := nativeOwnerChecks()
	if fixture.checks != nil {
		checks = *fixture.checks
	}
	if err := launchRestrictionsWith(managed, checks); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(config)
	if err != nil || len(raw) > configLimit {
		return nil, ErrJobUnavailable
	}
	startup, cancelStartup := context.WithTimeout(ctx, waitDelay)
	defer cancelStartup()
	deadline, _ := startup.Deadline()
	pair, err := privatePair()
	if err != nil {
		return nil, ErrJobUnavailable
	}
	socket := os.NewFile(uintptr(pair[0]), "owner bootstrap")
	remote := os.NewFile(uintptr(pair[1]), "child bootstrap")
	owned := []*os.File{socket, remote}
	defer func() {
		for _, file := range owned {
			_ = file.Close()
		}
	}()
	var inputCopy *os.File
	if input != nil {
		inputFD, e := fileDescriptor(input)
		if e != nil {
			return nil, ErrJobUnavailable
		}
		flags, e := unix.FcntlInt(uintptr(inputFD), unix.F_GETFL, 0)
		if e != nil || flags&unix.O_PATH != 0 || flags&unix.O_ACCMODE == unix.O_WRONLY {
			return nil, ErrJobUnavailable
		}
		fd, e := unix.FcntlInt(uintptr(inputFD), unix.F_DUPFD_CLOEXEC, 0)
		if e != nil {
			return nil, ErrJobUnavailable
		}
		inputCopy = os.NewFile(uintptr(fd), "child input")
		owned = append(owned, inputCopy)
	}
	self, err := os.Executable()
	if err != nil {
		return nil, ErrJobUnavailable
	}
	if fixture.executable != "" {
		self = fixture.executable
	}
	outputRead, outputWrite, err := os.Pipe()
	if err != nil {
		return nil, ErrJobUnavailable
	}
	owned = append(owned, outputWrite)
	out := &capture{limit: Cap}
	drained := make(chan struct{})
	go func() { _, _ = io.Copy(out, outputRead); _ = outputRead.Close(); close(drained) }()
	cmd := exec.Command(self, childEntry, "1", "3", config.Role)
	cmd.Env = []string{Marker}
	cmd.Stdout, cmd.Stderr = outputWrite, outputWrite
	cmd.ExtraFiles = []*os.File{remote}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		_ = outputWrite.Close()
		_ = outputRead.Close()
		<-drained
		return nil, ErrJobUnavailable
	}
	_ = outputWrite.Close()
	_ = remote.Close()
	waited := make(chan error, 1)
	var waitOnce sync.Once
	startWait := func() { waitOnce.Do(func() { go func() { waited <- cmd.Wait() }() }) }
	var resultRead, liveWrite *os.File
	var reader *bufio.Reader
	finishManaged := func(p *Proc, startupErr error) {
		finishDeadline := time.Unix(0, config.Deadline).Add(waitDelay)
		stop := context.AfterFunc(ctx, func() { _ = liveWrite.Close(); _ = resultRead.SetReadDeadline(time.Now().Add(waitDelay)) })
		terminal, frameErr := childFrame{Exit: -1}, startupErr
		if frameErr == nil {
			terminal, frameErr = readFrame(reader, "managed", "terminal", config.Spec.AttemptID)
		}
		cleanupEnd := minTime(finishDeadline, time.Now().Add(waitDelay))
		if frameErr == nil && terminal.CleanupDeadlineUnixNano > 0 {
			cleanupEnd = minTime(cleanupEnd, time.Unix(0, terminal.CleanupDeadlineUnixNano))
			_ = resultRead.SetReadDeadline(cleanupEnd)
		}
		if terminal.StartupDeadlineUnixNano != 0 || terminal.Completion == TreeQuiescent && terminal.CleanupDeadlineUnixNano <= 0 {
			frameErr = ErrCompletionUnknown
		}
		if frameErr == nil {
			if _, err := reader.ReadByte(); err != io.EOF {
				frameErr = ErrCompletionUnknown
			}
		}
		stop()
		_ = liveWrite.Close()
		_ = resultRead.Close()
		if frameErr != nil || terminal.Completion != TreeQuiescent {
			_ = outputRead.Close()
		}
		var waitErr error
		select {
		case waitErr = <-waited:
		case <-time.After(max(0, time.Until(cleanupEnd))):
			_ = cmd.Process.Kill()
			frameErr = ErrCompletionUnknown
		}
		_ = outputRead.SetReadDeadline(cleanupEnd)
		select {
		case <-drained:
		case <-time.After(max(0, time.Until(cleanupEnd))):
			_ = outputRead.Close()
			<-drained
			frameErr = ErrCompletionUnknown
		}
		result := Result{Exit: terminal.Exit, TimedOut: terminal.TimedOut || errors.Is(ctx.Err(), context.DeadlineExceeded), Completion: Completion{AttemptID: config.Spec.AttemptID, State: CompletionUnknown}}
		if frameErr == nil && waitErr == nil && terminal.Completion == TreeQuiescent {
			result.Completion.State = TreeQuiescent
		} else {
			frameErr = ErrCompletionUnknown
		}
		p.finish(result, frameErr)
	}
	started := false
	effectsPossible := false
	defer func() {
		if !started {
			if managed && effectsPossible {
				// Owner EOF must reach the guardian before its cleanup bound expires.
				_ = liveWrite.Close()
				startWait()
				proc = &Proc{ctx: ctx, cancel: cancel, cmd: cmd, out: out, done: make(chan struct{})}
				err = nil
				go finishManaged(proc, ErrCompletionUnknown)
				return
			}
			_ = cmd.Process.Kill()
			startWait()
			_ = outputRead.Close()
			select {
			case <-waited:
			case <-time.After(waitDelay):
				err = ErrCompletionUnknown
			}
			<-drained
		}
	}()
	if _, _, err := receiveBootstrap(startup, pair[0], cmd.Process.Pid, config.Role, "bootstrap-ready", 0); err != nil {
		return nil, errors.Join(ErrJobUnavailable, errors.New("child_bootstrap_ready"))
	}
	if err := bootstrapExhausted(pair[0], false); err != nil {
		return nil, err
	}
	if err := launchRestrictionsWith(managed, checks); err != nil {
		return nil, err
	}
	if err := unix.SetsockoptInt(pair[0], unix.SOL_SOCKET, unix.SO_PASSCRED, 0); err != nil {
		return nil, ErrJobUnavailable
	}
	configRead, configWrite, err := os.Pipe()
	if err != nil {
		return nil, ErrJobUnavailable
	}
	owned = append(owned, configRead, configWrite)
	var resultWrite *os.File
	resultRead, resultWrite, err = os.Pipe()
	if err != nil {
		return nil, ErrJobUnavailable
	}
	owned = append(owned, resultRead, resultWrite)
	var liveRead *os.File
	second := inputCopy
	if managed {
		liveRead, liveWrite, err = os.Pipe()
		if err != nil {
			return nil, ErrJobUnavailable
		}
		owned = append(owned, liveRead, liveWrite)
		second = liveRead
	}
	transferred := []*os.File{configRead, second, resultWrite}
	if managed && inputCopy != nil {
		transferred = append(transferred, inputCopy)
	}
	rights := make([]int, len(transferred))
	for i, file := range transferred {
		rights[i], err = fileDescriptor(file)
		if err != nil {
			return nil, ErrJobUnavailable
		}
	}
	if validateRights(rights, managed) != nil {
		return nil, ErrJobUnavailable
	}
	if err := sendBootstrap(startup, pair[0], childFrame{Version: protocolVersion, Role: config.Role, Phase: "handoff", StartupDeadlineUnixNano: deadline.UnixNano()}, rights); err != nil {
		return nil, ErrJobUnavailable
	}
	_ = socket.Close()
	_ = configRead.Close()
	_ = resultWrite.Close()
	if liveRead != nil {
		_ = liveRead.Close()
	}
	if inputCopy != nil {
		_ = inputCopy.Close()
	}
	if err := configWrite.SetWriteDeadline(deadline); err != nil {
		return nil, ErrJobUnavailable
	}
	stopCancel := context.AfterFunc(startup, func() { _ = configWrite.Close(); _ = resultRead.Close() })
	defer stopCancel()
	effectsPossible = true
	if n, err := configWrite.Write(raw); err != nil || n != len(raw) {
		return nil, ErrJobUnavailable
	}
	_ = configWrite.Close()
	if err := resultRead.SetReadDeadline(deadline); err != nil {
		return nil, ErrJobUnavailable
	}
	reader = bufio.NewReaderSize(resultRead, frameLimit+1)
	if fixture.acceptReady != nil {
		fixture.acceptReady()
	}
	ready, err := readFrame(reader, config.Role, "configured-ready", config.Spec.AttemptID)
	if err != nil || ready.Completion != "" || ready.Exit != 0 || ready.TimedOut || ready.StartupDeadlineUnixNano != 0 || ready.CleanupDeadlineUnixNano != 0 {
		return nil, errors.Join(ErrJobUnavailable, errors.New("child_configured_ready"))
	}
	if !stopCancel() {
		return nil, ErrJobUnavailable
	}
	if fixture.endpoints != nil && liveWrite != nil {
		liveFD, e := fileDescriptor(liveWrite)
		if e != nil {
			return nil, ErrJobUnavailable
		}
		resultFD, e := fileDescriptor(resultRead)
		if e != nil {
			return nil, ErrJobUnavailable
		}
		fixture.endpoints(cmd.Process.Pid, liveFD, resultFD)
	}
	// Only these endpoints survive startup in the owner; all sender aliases close.
	kept := owned[:0]
	for _, file := range owned {
		if file != resultRead && file != liveWrite {
			kept = append(kept, file)
		}
	}
	owned = kept
	p := &Proc{ctx: ctx, cancel: cancel, cmd: cmd, out: out, done: make(chan struct{})}
	started = true
	startWait()
	if !managed {
		_ = resultRead.Close()
		go p.finishDiagnostic(waited, outputRead, drained)
		return p, nil
	}
	_ = resultRead.SetReadDeadline(time.Unix(0, config.Deadline).Add(waitDelay))
	go finishManaged(p, nil)
	return p, nil
}

func fileDescriptor(file *os.File) (int, error) {
	raw, err := file.SyscallConn()
	if err != nil {
		return 0, err
	}
	var fd int
	err = raw.Control(func(value uintptr) { fd = int(value) })
	return fd, err
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func (p *Proc) finishDiagnostic(waited <-chan error, output *os.File, drained <-chan struct{}) {
	var err error
	var cleanupEnd time.Time
	select {
	case err = <-waited:
		cleanupEnd = time.Now().Add(waitDelay)
	case <-p.ctx.Done():
		cleanupEnd = time.Now().Add(waitDelay)
		_ = p.cmd.Process.Kill()
		select {
		case err = <-waited:
		case <-time.After(max(0, time.Until(cleanupEnd))):
			err = ErrCompletionUnknown
		}
	}
	_ = output.SetReadDeadline(cleanupEnd)
	select {
	case <-drained:
	case <-time.After(max(0, time.Until(cleanupEnd))):
		_ = output.Close()
		<-drained
		err = ErrCompletionUnknown
	}
	result := Result{Exit: 0}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		result.Exit = exit.ExitCode()
		err = nil
	}
	p.finish(result, err)
}

func validateRights(rights []int, managed bool) error {
	if len(rights) != 3 && (!managed || len(rights) != 4) {
		return ErrJobUnavailable
	}
	seen := make(map[[2]uint64]bool, len(rights))
	for i, fd := range rights {
		var stat unix.Stat_t
		if unix.Fstat(fd, &stat) != nil {
			return ErrJobUnavailable
		}
		identity := [2]uint64{uint64(stat.Dev), stat.Ino}
		if seen[identity] {
			return ErrJobUnavailable
		}
		seen[identity] = true
		flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
		if err != nil || flags&unix.O_PATH != 0 {
			return ErrJobUnavailable
		}
		if i < 3 {
			mode := unix.O_RDONLY
			if i == 2 {
				mode = unix.O_WRONLY
			}
			if stat.Mode&unix.S_IFMT != unix.S_IFIFO || flags&unix.O_ACCMODE != mode {
				return ErrJobUnavailable
			}
		} else if flags&unix.O_ACCMODE == unix.O_WRONLY {
			return ErrJobUnavailable
		}
	}
	return nil
}

// Enter is called explicitly before the owning binary's ordinary CLI or tests.
func Enter(args []string) (bool, int) {
	return enter(args, nil)
}

func enter(args []string, observe func(string, []int)) (bool, int) {
	if len(args) == 0 || args[0] != childEntry {
		return false, 0
	}
	if len(args) != 4 || args[1] != "1" || args[2] != "3" || (args[3] != "managed" && args[3] != "diagnostic") || len(os.Environ()) != 1 || os.Getenv(Name) != "1" {
		return true, 2
	}
	if runGuardian(args[3], observe) != nil {
		return true, 3
	}
	return true, 0
}

func runGuardian(role string, observe func(string, []int)) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	parent := os.Getppid()
	if parent <= 1 {
		return ErrJobUnavailable
	}
	parentFD, err := unix.PidfdOpen(parent, 0)
	if err != nil {
		return errors.Join(ErrJobUnavailable, errors.New("guardian_parent_pidfd"))
	}
	defer func() { _ = unix.Close(parentFD) }()
	if os.Getppid() != parent {
		return ErrJobUnavailable
	}
	if err := PrepareManagedOwner(); err != nil {
		return errors.Join(err, errors.New("guardian_prepare"))
	}
	if err := launchRestrictions(role == "managed"); err != nil {
		return err
	}
	if role == "managed" {
		if unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0) != nil {
			return errors.Join(ErrJobUnavailable, errors.New("guardian_set_subreaper"))
		}
		var value int32
		e := unix.Prctl(unix.PR_GET_CHILD_SUBREAPER, uintptr(unsafe.Pointer(&value)), 0, 0, 0)
		if e != nil || value != 1 {
			return errors.Join(ErrJobUnavailable, errors.New("guardian_read_subreaper"))
		}
		if _, err := ownedChildren(); err != nil {
			return err
		}
	}
	socket := os.NewFile(3, "guardian bootstrap")
	defer func() { _ = socket.Close() }()
	fd := 3
	if !unnamed(fd) {
		return ErrJobUnavailable
	}
	if observe != nil {
		observe("bootstrap", []int{fd})
	}
	value, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_PASSCRED)
	if err != nil || value != 0 {
		return errors.Join(ErrJobUnavailable, errors.New("guardian_passcred"))
	}
	startup, cancel := context.WithTimeout(context.Background(), waitDelay)
	defer cancel()
	if err := sendBootstrap(startup, fd, childFrame{Version: protocolVersion, Role: role, Phase: "bootstrap-ready"}, nil); err != nil {
		return err
	}
	if unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_PASSCRED, 1) != nil {
		return ErrJobUnavailable
	}
	handoff, rights, err := receiveBootstrap(startup, fd, parent, role, "handoff", 3)
	if err != nil {
		return err
	}
	if validateRights(rights, role == "managed") != nil {
		for _, right := range rights {
			_ = unix.Close(right)
		}
		return ErrJobUnavailable
	}
	for _, right := range rights {
		if err := unix.SetNonblock(right, true); err != nil {
			for _, fd := range rights {
				_ = unix.Close(fd)
			}
			return ErrJobUnavailable
		}
	}
	files := make([]*os.File, len(rights))
	for i, right := range rights {
		files[i] = os.NewFile(uintptr(right), "guardian handoff")
	}
	defer func() {
		for _, file := range files {
			_ = file.Close()
		}
	}()
	if err := bootstrapExhausted(fd, true); err != nil {
		return err
	}
	_ = socket.Close()
	deadline := time.Unix(0, handoff.StartupDeadlineUnixNano)
	if own, ok := startup.Deadline(); ok {
		deadline = minTime(deadline, own)
	}
	if files[0].SetReadDeadline(deadline) != nil {
		return ErrJobUnavailable
	}
	raw, err := io.ReadAll(io.LimitReader(files[0], configLimit+1))
	if err != nil || len(raw) > configLimit {
		return ErrJobUnavailable
	}
	_ = files[0].Close()
	var config childConfig
	if decodeStrict(raw, &config) != nil || config.Version != protocolVersion || config.Role != role {
		return ErrJobUnavailable
	}
	if role == "diagnostic" {
		if len(rights) != 3 || !config.Input || config.Directory == "" || len(config.Directory) > 4096 || strings.ContainsRune(config.Directory, 0) || config.Spec.Path != "" || config.Spec.Dir != "" || len(config.Spec.Args) != 0 || config.Spec.AttemptID != (AttemptID{}) || config.Spec.Timeout != 0 {
			return ErrJobUnavailable
		}
		if observe != nil {
			observe("diagnostic-protected", []int{rights[1], rights[2]})
		}
		if err := pipeFrame(files[2], childFrame{Version: protocolVersion, Role: role, Phase: "configured-ready"}); err != nil {
			return err
		}
		_ = files[2].Close()
		return logs.Monitor(config.Directory, files[1])
	}
	if validateSpec(config.Spec) != nil || config.Directory != "" || config.Input != (len(rights) == 4) || config.Deadline <= time.Now().UnixNano() {
		return ErrJobUnavailable
	}
	deadline = time.Unix(0, config.Deadline)
	ctx, cancelRun := context.WithDeadline(context.Background(), deadline)
	defer cancelRun()
	watching := make(chan struct{})
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		close(watching)
		var one [1]byte
		_, _ = files[1].Read(one[:])
		cancelRun()
	}()
	<-watching
	defer func() { _ = files[1].Close(); <-joined }()
	if observe != nil {
		observe("before-release", nil)
	}
	parentPoll := []unix.PollFd{{Fd: int32(parentFD), Events: unix.POLLIN}}
	n, err := unix.Poll(parentPoll, 0)
	if err != nil || n != 0 || launchRestrictions(true) != nil {
		return ErrJobUnavailable
	}
	if err := pipeFrame(files[2], childFrame{Version: protocolVersion, Role: role, Phase: "configured-ready", AttemptID: config.Spec.AttemptID}); err != nil {
		return err
	}
	if observe != nil {
		observe("protected", []int{rights[1], rights[2]})
	}
	var input *os.File
	if config.Input {
		input = files[3]
	}
	result, completeErr := superviseTree(ctx, config.Spec, input, observe)
	state := TreeQuiescent
	if completeErr != nil {
		state = CompletionUnknown
	}
	cleanupDeadline := int64(0)
	if !result.cleanupDeadline.IsZero() {
		cleanupDeadline = result.cleanupDeadline.UnixNano()
	}
	return pipeFrame(files[2], childFrame{Version: protocolVersion, Role: role, Phase: "terminal", AttemptID: config.Spec.AttemptID, Exit: result.Exit, TimedOut: result.TimedOut, Completion: state, CleanupDeadlineUnixNano: cleanupDeadline})
}

func superviseTree(ctx context.Context, spec Spec, input *os.File, observe func(string, []int)) (Result, error) {
	result := Result{Exit: -1}
	if ctx.Err() != nil || launchRestrictions(true) != nil {
		return result, ErrJobUnavailable
	}
	cmd := exec.Command(spec.Path, spec.Args...)
	cmd.Dir = spec.Dir
	cmd.Env = []string{Marker}
	if input != nil {
		cmd.Stdin = input
	}
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return result, ErrJobUnavailable
	}
	if observe != nil {
		observe("target-started", []int{cmd.Process.Pid})
	}
	fd, err := unix.PidfdOpen(cmd.Process.Pid, 0)
	if err != nil {
		result.cleanupDeadline = time.Now().Add(waitDelay)
		cleanup, cancel := context.WithDeadline(context.Background(), result.cleanupDeadline)
		defer cancel()
		_ = cmd.Process.Kill()
		waited := make(chan error, 1)
		go func() { waited <- cmd.Wait() }()
		select {
		case <-waited:
			_ = reapTree(cleanup)
		case <-cleanup.Done():
		}
		return result, ErrCompletionUnknown
	}
	defer func() { _ = unix.Close(fd) }()
	err = socketWait(ctx, fd, unix.POLLIN)
	result.TimedOut = errors.Is(ctx.Err(), context.DeadlineExceeded)
	cleanup := time.Now().Add(waitDelay)
	result.cleanupDeadline = cleanup
	// The unreaped leader pins its numeric group until the group is signaled.
	signalErr := unix.Kill(-cmd.Process.Pid, unix.SIGKILL)
	if signalErr != nil && signalErr != unix.ESRCH {
		err = ErrCompletionUnknown
	}
	if err != nil {
		_ = unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0)
	}
	cleanupCtx, cancel := context.WithDeadline(context.Background(), cleanup)
	defer cancel()
	if waitErr := socketWait(cleanupCtx, fd, unix.POLLIN); waitErr != nil {
		return result, ErrCompletionUnknown
	}
	waitErr := cmd.Wait()
	var exit *exec.ExitError
	if errors.As(waitErr, &exit) {
		result.Exit = exit.ExitCode()
	} else if waitErr == nil {
		result.Exit = 0
	} else {
		err = ErrCompletionUnknown
	}
	if observe != nil {
		observe("cleanup", nil)
	}
	if cleanupErr := reapTree(cleanupCtx); cleanupErr != nil {
		return result, ErrCompletionUnknown
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		err = nil
	}
	return result, err
}

func ownedChildren() ([]int, error) {
	dir, err := os.Open("/proc/self/task")
	if err != nil {
		return nil, ErrCompletionUnknown
	}
	tasks, err := dir.ReadDir(257)
	_ = dir.Close()
	if err != nil && err != io.EOF || len(tasks) > 256 {
		return nil, ErrCompletionUnknown
	}
	children := make([]int, 0, 32)
	remaining := 64 << 10
	seen := make(map[int]bool)
	for _, task := range tasks {
		if _, err := strconv.ParseUint(task.Name(), 10, 32); err != nil {
			return nil, ErrCompletionUnknown
		}
		file, err := os.Open(filepath.Join("/proc/self/task", task.Name(), "children"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, ErrCompletionUnknown
		}
		raw, err := io.ReadAll(io.LimitReader(file, int64(remaining+1)))
		_ = file.Close()
		if err != nil || len(raw) > remaining {
			return nil, ErrCompletionUnknown
		}
		remaining -= len(raw)
		for _, text := range strings.Fields(string(raw)) {
			pid, err := strconv.Atoi(text)
			if err != nil || pid <= 0 {
				return nil, ErrCompletionUnknown
			}
			if !seen[pid] {
				seen[pid] = true
				children = append(children, pid)
			}
			if len(children) > 4096 {
				return nil, ErrCompletionUnknown
			}
		}
	}
	return children, nil
}

type treeObservation struct {
	children func() ([]int, error)
	pin      func(int, int) (int, error)
	wait     func(int, *unix.WaitStatus, int, *unix.Rusage) (int, error)
	signal   func(int, unix.Signal, *unix.Siginfo, int) error
}

func nativeTreeObservation() treeObservation {
	return treeObservation{children: ownedChildren, pin: unix.PidfdOpen, wait: unix.Wait4, signal: unix.PidfdSendSignal}
}

func reapTree(ctx context.Context) error { return reapObservedTree(ctx, nativeTreeObservation()) }

func reapObservedTree(ctx context.Context, observation treeObservation) error {
	for {
		if ctx.Err() != nil {
			return ErrCompletionUnknown
		}
		var status unix.WaitStatus
		pid, err := observation.wait(-1, &status, unix.WNOHANG, nil)
		if err == unix.EINTR {
			continue
		}
		if err == unix.ECHILD {
			return nil
		}
		if err != nil {
			return ErrCompletionUnknown
		}
		if pid > 0 {
			continue
		}
		children, err := observation.children()
		if err != nil || len(children) == 0 {
			return ErrCompletionUnknown
		}
		for _, pid := range children {
			fd, err := observation.pin(pid, 0)
			if err != nil {
				return ErrCompletionUnknown
			}
			err = observation.signal(fd, unix.SIGKILL, nil, 0)
			if err == nil || err == unix.ESRCH {
				err = socketWait(ctx, fd, unix.POLLIN)
			}
			if err == nil {
				_, err = observation.wait(pid, &status, 0, nil)
			}
			_ = unix.Close(fd)
			if err != nil {
				return ErrCompletionUnknown
			}
		}
	}
}
