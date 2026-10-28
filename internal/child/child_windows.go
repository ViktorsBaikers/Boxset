//go:build windows

package child

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func prepareDiagnostic(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
}

func startPlatformManaged(ctx context.Context, spec Spec, input *os.File) (*Proc, error) {
	ctx, cancel := context.WithTimeout(ctx, spec.Timeout)
	cmd := exec.CommandContext(ctx, spec.Path, spec.Args...)
	cmd.Dir = spec.Dir
	cmd.Env = []string{Marker}
	if input != nil {
		cmd.Stdin = input
	}
	out := &capture{limit: Cap}
	outputRead, outputWrite, err := os.Pipe()
	if err != nil {
		cancel()
		return nil, ErrJobUnavailable
	}
	drained := make(chan struct{})
	go func() { _, _ = io.Copy(out, outputRead); _ = outputRead.Close(); close(drained) }()
	cmd.Stdout, cmd.Stderr = outputWrite, outputWrite
	prepare(cmd)
	if err := cmd.Start(); err != nil {
		_ = outputWrite.Close()
		_ = outputRead.Close()
		<-drained
		cancel()
		return nil, err
	}
	_ = outputWrite.Close()
	cleanup, err := attached(cmd)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = outputRead.Close()
		<-drained
		cancel()
		return nil, err
	}
	p := &Proc{ctx: ctx, cancel: cancel, cmd: cmd, out: out, done: make(chan struct{})}
	go func() {
		err := cmd.Wait()
		cleanupEnd := time.Now().Add(waitDelay)
		result := Result{TimedOut: errors.Is(ctx.Err(), context.DeadlineExceeded), Completion: Completion{AttemptID: spec.AttemptID, State: CompletionUnknown}}
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			result.Exit = exit.ExitCode()
			err = nil
		}
		if cleanupErr := cleanup(cleanupEnd); cleanupErr != nil {
			err = ErrCompletionUnknown
		} else {
			result.Completion.State = TreeQuiescent
		}
		_ = outputRead.SetReadDeadline(cleanupEnd)
		select {
		case <-drained:
		case <-time.After(max(0, time.Until(cleanupEnd))):
			_ = outputRead.Close()
			<-drained
			err = ErrCompletionUnknown
			result.Completion.State = CompletionUnknown
		}
		p.finish(result, err)
	}()
	return p, nil
}

// prepare starts the child suspended: it holds its first instruction until it is
// in the job object, so no descendant can be created outside the job that kills
// the tree together.
func prepare(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_SUSPENDED}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		return cmd.Process.Kill()
	}
	cmd.WaitDelay = waitDelay
}

// attached puts the suspended child in a job object that kills every process in
// it when the last handle closes, and then resumes it. A job that cannot be
// created or assigned terminates the still-suspended child and reports
// ErrJobUnavailable: a descendant is never left to start unsupervised. Runtime
// behaviour on Windows is NOT-RUN here; docs/portability.md records that.
func attached(cmd *exec.Cmd) (func(time.Time) error, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, ErrJobUnavailable
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		_ = windows.CloseHandle(job)
		return nil, ErrJobUnavailable
	}
	pid := uint32(cmd.Process.Pid)
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, pid)
	if err != nil {
		_ = windows.CloseHandle(job)
		return nil, ErrJobUnavailable
	}
	defer func() { _ = windows.CloseHandle(process) }()
	if err := windows.AssignProcessToJobObject(job, process); err != nil {
		_ = windows.TerminateProcess(process, 1)
		_ = windows.CloseHandle(job)
		return nil, ErrJobUnavailable
	}
	if err := resumePrimary(pid); err != nil {
		_ = windows.TerminateProcess(process, 1)
		_ = windows.CloseHandle(job)
		return nil, ErrJobUnavailable
	}
	return func(until time.Time) error {
		defer func() { _ = windows.CloseHandle(job) }()
		if windows.TerminateJobObject(job, 1) != nil {
			return ErrCompletionUnknown
		}
		deadline := time.NewTimer(max(0, time.Until(until)))
		defer deadline.Stop()
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			var accounting struct {
				TotalUserTime, TotalKernelTime, ThisPeriodTotalUserTime, ThisPeriodTotalKernelTime int64
				TotalPageFaultCount, TotalProcesses, ActiveProcesses, TotalTerminatedProcesses     uint32
			}
			if windows.QueryInformationJobObject(job, windows.JobObjectBasicAccountingInformation, uintptr(unsafe.Pointer(&accounting)), uint32(unsafe.Sizeof(accounting)), nil) != nil {
				return ErrCompletionUnknown
			}
			if accounting.ActiveProcesses == 0 {
				return nil
			}
			select {
			case <-ticker.C:
			case <-deadline.C:
				return ErrCompletionUnknown
			}
		}
	}, nil
}

// resumePrimary resumes the one thread the suspended child runs on.
func resumePrimary(pid uint32) error {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return err
	}
	defer func() { _ = windows.CloseHandle(snapshot) }()
	var entry windows.ThreadEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	for err = windows.Thread32First(snapshot, &entry); err == nil; err = windows.Thread32Next(snapshot, &entry) {
		if entry.OwnerProcessID != pid {
			continue
		}
		thread, openErr := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
		if openErr != nil {
			return openErr
		}
		_, resumeErr := windows.ResumeThread(thread)
		_ = windows.CloseHandle(thread)
		return resumeErr
	}
	return ErrJobUnavailable
}
