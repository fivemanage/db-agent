package update

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type Restart struct {
	ServiceName string
	ConfigPath  string
	NoRestart   bool
}

// LaunchHelper starts a separate copy of the installed binary. The caller must
// then exit so Windows can release the installed executable before replacement.
func LaunchHelper(executablePath, stagedPath, checksum, expectedVersion string, restart Restart) error {
	if filepath.Dir(executablePath) != filepath.Dir(stagedPath) {
		return errors.New("staged update must be beside the executable")
	}
	if err := verifyFile(stagedPath, checksum); err != nil {
		return err
	}
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}
	source, err := os.Open(executablePath)
	if err != nil {
		return err
	}
	defer source.Close()
	helper, err := os.CreateTemp(filepath.Dir(executablePath), ".db-agent-helper-*"+suffix)
	if err != nil {
		return err
	}
	helperPath := helper.Name()
	complete := false
	defer func() {
		helper.Close()
		if !complete {
			os.Remove(helperPath)
		}
	}()
	if err := helper.Chmod(0700); err != nil {
		return err
	}
	if _, err := io.Copy(helper, source); err != nil {
		return err
	}
	if err := helper.Sync(); err != nil {
		return err
	}
	if err := helper.Close(); err != nil {
		return err
	}
	// The running binary proves a previous update got far enough to start. Its
	// rollback copy can be removed before preparing the next replacement.
	if err := os.Remove(executablePath + ".rollback"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	args := []string{"update-helper", "--target", executablePath, "--staged", stagedPath, "--sha256", checksum, "--expected-version", expectedVersion, "--service", restart.ServiceName, "--restart-config", restart.ConfigPath}
	if restart.NoRestart {
		args = append(args, "--no-restart")
	}
	command := exec.Command(helperPath, args...)
	if runtime.GOOS == "linux" && restart.ServiceName != "" {
		// A helper left in the agent unit's cgroup would be killed when that
		// service stops. A transient systemd unit survives that transition.
		unit := fmt.Sprintf("fivemanage-db-agent-update-%d", time.Now().UnixNano())
		command = exec.Command("systemd-run", append([]string{"--quiet", "--collect", "--unit", unit, helperPath}, args...)...)
	}
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if runtime.GOOS == "linux" && restart.ServiceName != "" {
		if err := command.Run(); err != nil {
			return fmt.Errorf("schedule update helper: %w", err)
		}
		complete = true
		return nil
	}
	if err := command.Start(); err != nil {
		return fmt.Errorf("start update helper: %w", err)
	}
	complete = true
	return command.Process.Release()
}

// RunHelper waits for the old process to release its executable, swaps in the
// verified artifact, and restores the old binary if restarting fails.
func RunHelper(target, staged, checksum, expectedVersion string, restart Restart) error {
	if !filepath.IsAbs(target) || !filepath.IsAbs(staged) || filepath.Dir(target) != filepath.Dir(staged) {
		return errors.New("update paths must be absolute and in the same directory")
	}
	if err := verifyFile(staged, checksum); err != nil {
		return err
	}
	backup := target + ".rollback"
	if _, err := os.Stat(backup); err == nil {
		return fmt.Errorf("previous rollback binary still exists: %s", backup)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if restart.ServiceName != "" {
		// A manual update may have been requested while the service was still
		// running. Replacement waits below if stopping has not completed yet.
		stopAgentService(restart.ServiceName)
	}
	// Windows holds the running executable open. Retry while the parent exits.
	time.Sleep(time.Second)
	deadline := time.Now().Add(30 * time.Second)
	var replaceErr error
	for time.Now().Before(deadline) {
		if err := verifyFile(staged, checksum); err != nil {
			return err
		}
		replaceErr = replaceWithBackup(target, staged, backup)
		if replaceErr == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if replaceErr != nil {
		return fmt.Errorf("replace agent executable: %w", replaceErr)
	}
	if err := verifyInstalledVersion(target, expectedVersion); err != nil {
		if rollbackErr := restoreBackup(target, backup); rollbackErr != nil {
			return fmt.Errorf("new executable failed validation: %v; rollback failed: %w", err, rollbackErr)
		}
		if restart.NoRestart {
			return fmt.Errorf("new executable failed validation; old binary restored: %w", err)
		}
		if retryErr := restartAgent(target, restart); retryErr != nil {
			return fmt.Errorf("new executable failed validation: %v; old binary restored but restart failed: %w", err, retryErr)
		}
		return fmt.Errorf("new executable failed validation; old binary restored: %w", err)
	}
	if restart.NoRestart {
		return nil
	}
	if err := restartAgent(target, restart); err != nil {
		if restart.ServiceName != "" {
			stopAgentService(restart.ServiceName)
		}
		rollbackErr := restoreBackup(target, backup)
		if rollbackErr != nil {
			return fmt.Errorf("restart failed: %v; rollback failed: %w", err, rollbackErr)
		}
		if retryErr := restartAgent(target, restart); retryErr != nil {
			return fmt.Errorf("new agent restart failed: %v; old binary restored but restart failed: %w", err, retryErr)
		}
		return fmt.Errorf("new agent restart failed; old binary restored: %w", err)
	}
	return nil
}

func verifyInstalledVersion(target, expected string) error {
	if _, err := parseVersion(expected); err != nil {
		return err
	}
	command := exec.Command(target, "version")
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("run new agent version command: %w: %s", err, strings.TrimSpace(string(output)))
	}
	if strings.TrimSpace(string(output)) != expected {
		return fmt.Errorf("new agent reports version %q, expected %q", strings.TrimSpace(string(output)), expected)
	}
	return nil
}

func stopAgentService(name string) {
	switch runtime.GOOS {
	case "windows":
		exec.Command("sc.exe", "stop", name).Run()
	case "linux":
		exec.Command("systemctl", "stop", name).Run()
	}
}

func verifyFile(path, expected string) error {
	if len(expected) != 64 || strings.ToLower(expected) != expected {
		return errors.New("invalid expected SHA-256 digest")
	}
	if _, err := hex.DecodeString(expected); err != nil {
		return errors.New("invalid expected SHA-256 digest")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, io.LimitReader(file, maxArtifactBytes+1)); err != nil {
		return err
	}
	if hex.EncodeToString(hash.Sum(nil)) != expected {
		return errors.New("staged update checksum mismatch")
	}
	return nil
}

func restartAgent(target string, restart Restart) error {
	if restart.ServiceName != "" {
		var command *exec.Cmd
		switch runtime.GOOS {
		case "windows":
			command = exec.Command("sc.exe", "start", restart.ServiceName)
		case "linux":
			command = exec.Command("systemctl", "start", restart.ServiceName)
		default:
			return errors.New("service restart is unsupported on this platform")
		}
		if output, err := command.CombinedOutput(); err != nil {
			return fmt.Errorf("start service: %w: %s", err, strings.TrimSpace(string(output)))
		}
		time.Sleep(3 * time.Second)
		switch runtime.GOOS {
		case "linux":
			if err := exec.Command("systemctl", "is-active", "--quiet", restart.ServiceName).Run(); err != nil {
				return fmt.Errorf("service did not stay active: %w", err)
			}
		case "windows":
			output, err := exec.Command("sc.exe", "query", restart.ServiceName).CombinedOutput()
			if err != nil || !strings.Contains(string(output), "RUNNING") {
				return fmt.Errorf("service did not stay running: %v: %s", err, strings.TrimSpace(string(output)))
			}
		}
		return nil
	}
	args := []string{"agent"}
	if restart.ConfigPath != "" {
		args = append([]string{"--config", restart.ConfigPath}, args...)
	}
	command := exec.Command(target, args...)
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		return err
	}
	finished := make(chan error, 1)
	go func() { finished <- command.Wait() }()
	select {
	case err := <-finished:
		return fmt.Errorf("agent exited soon after restart: %v", err)
	case <-time.After(3 * time.Second):
		return nil
	}
}
