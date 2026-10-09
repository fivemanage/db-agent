//go:build !windows

package dump

import (
	"errors"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

type process struct{ cmd *exec.Cmd }

func startProcess(c command, stdout, stderr *os.File) (*process, error) {
	cmd := exec.Command(c.path, c.args...)
	cmd.Env, cmd.Stdout, cmd.Stderr = c.env, stdout, stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &process{cmd: cmd}
	if err := unix.Setpriority(unix.PRIO_PROCESS, cmd.Process.Pid, 19); err != nil && !errors.Is(err, unix.ESRCH) {
		_ = p.kill()
		_ = cmd.Wait()
		return nil, err
	}
	return p, nil
}

func (p *process) wait() processResult {
	err := p.cmd.Wait()
	code := -1
	if p.cmd.ProcessState != nil {
		code = p.cmd.ProcessState.ExitCode()
	}
	return processResult{code: code, err: err}
}

func (p *process) kill() error {
	err := unix.Kill(-p.cmd.Process.Pid, unix.SIGKILL)
	if errors.Is(err, unix.ESRCH) {
		return nil
	}
	return err
}

func (p *process) close() {}

func platformDiskFull(error) bool { return false }

func protectDirectory(string) error { return nil }
