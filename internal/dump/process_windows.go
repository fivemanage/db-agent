package dump

import (
	"errors"
	"os"
	"runtime"
	"sort"
	"strings"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/fivemanage/db-agent/internal/securefile"
)

type process struct {
	handle windows.Handle
	job    windows.Handle
}

func startProcess(c command, stdout, stderr *os.File) (*process, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	keepJob := false
	defer func() {
		if !keepJob {
			_ = windows.CloseHandle(job)
		}
	}()
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		return nil, err
	}
	stdin, err := os.Open(os.DevNull)
	if err != nil {
		return nil, err
	}
	defer stdin.Close()
	handles := make([]windows.Handle, 3)
	for i, file := range []*os.File{stdin, stdout, stderr} {
		if err := windows.DuplicateHandle(windows.CurrentProcess(), windows.Handle(file.Fd()), windows.CurrentProcess(), &handles[i], 0, true, windows.DUPLICATE_SAME_ACCESS); err != nil {
			return nil, err
		}
		defer windows.CloseHandle(handles[i])
	}
	attributes, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return nil, err
	}
	defer attributes.Delete()
	if err := attributes.Update(windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST, unsafe.Pointer(&handles[0]), uintptr(len(handles))*unsafe.Sizeof(handles[0])); err != nil {
		return nil, err
	}
	si := windows.StartupInfoEx{StartupInfo: windows.StartupInfo{
		Cb: uint32(unsafe.Sizeof(windows.StartupInfoEx{})), Flags: windows.STARTF_USESTDHANDLES,
		StdInput: handles[0], StdOutput: handles[1], StdErr: handles[2],
	}, ProcThreadAttributeList: attributes.List()}
	application, err := windows.UTF16PtrFromString(c.path)
	if err != nil {
		return nil, err
	}
	commandLine, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(append([]string{c.path}, c.args...)))
	if err != nil {
		return nil, err
	}
	env := append([]string(nil), c.env...)
	for _, value := range env {
		if strings.ContainsRune(value, 0) {
			return nil, windows.ERROR_INVALID_PARAMETER
		}
	}
	sort.Slice(env, func(i, j int) bool { return strings.ToUpper(env[i]) < strings.ToUpper(env[j]) })
	environment := utf16.Encode([]rune(strings.Join(env, "\x00") + "\x00\x00"))
	var pi windows.ProcessInformation
	// Assign while suspended: assigning an already-running process has a race
	// where it can spawn children that escape the cancellation job.
	flags := uint32(windows.CREATE_SUSPENDED | windows.CREATE_NO_WINDOW | windows.CREATE_UNICODE_ENVIRONMENT | windows.BELOW_NORMAL_PRIORITY_CLASS | windows.EXTENDED_STARTUPINFO_PRESENT)
	err = windows.CreateProcess(application, commandLine, nil, nil, true, flags, &environment[0], nil, &si.StartupInfo, &pi)
	runtime.KeepAlive(handles)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(pi.Thread)
	if err := windows.AssignProcessToJobObject(job, pi.Process); err != nil {
		_ = windows.TerminateProcess(pi.Process, 1)
		_, _ = windows.WaitForSingleObject(pi.Process, windows.INFINITE)
		_ = windows.CloseHandle(pi.Process)
		return nil, err
	}
	if _, err := windows.ResumeThread(pi.Thread); err != nil {
		_ = windows.TerminateJobObject(job, 1)
		_, _ = windows.WaitForSingleObject(pi.Process, windows.INFINITE)
		_ = windows.CloseHandle(pi.Process)
		return nil, err
	}
	keepJob = true
	return &process{handle: pi.Process, job: job}, nil
}

func (p *process) wait() processResult {
	if _, err := windows.WaitForSingleObject(p.handle, windows.INFINITE); err != nil {
		return processResult{err: err}
	}
	var code uint32
	err := windows.GetExitCodeProcess(p.handle, &code)
	return processResult{code: int(code), err: err}
}

func (p *process) kill() error { return windows.TerminateJobObject(p.job, 1) }

func (p *process) close() {
	_ = windows.CloseHandle(p.job)
	_ = windows.CloseHandle(p.handle)
}

func platformDiskFull(err error) bool {
	return errors.Is(err, windows.ERROR_DISK_FULL) || errors.Is(err, windows.ERROR_HANDLE_DISK_FULL)
}

func protectDirectory(path string) error {
	sd, err := windows.SecurityDescriptorFromString(securefile.PrivateDACL)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}
