//go:build !windows

package agent

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	piCancelHelperBinEnv    = "MULTICA_PI_CANCEL_HELPER_BIN"
	piCancelChildPidFileEnv = "MULTICA_PI_CANCEL_CHILD_PIDFILE"
)

// runFakePiCancelHelper is dispatched from TestMain when the fake pi script
// re-executes this test binary. It reproduces Pi's bash child topology: the
// agent process is the leader of its own group, and the command it runs gets a
// separate detached process group. On SIGTERM it performs the same cleanup as
// Pi's print mode (kill tracked detached children, exit 143). A SIGKILL from
// the daemon cannot run this code, which is exactly the leak under test.
func runFakePiCancelHelper() {
	_, _ = io.Copy(io.Discard, os.Stdin)

	child := exec.Command("sleep", "600")
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := child.Start(); err != nil {
		os.Exit(2)
	}
	pidFile := os.Getenv(piCancelChildPidFileEnv)
	if pidFile != "" {
		_ = os.WriteFile(pidFile, []byte(strconv.Itoa(child.Process.Pid)), 0o600)
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM)
	<-signals
	_ = syscall.Kill(-child.Process.Pid, syscall.SIGTERM)
	time.Sleep(150 * time.Millisecond)
	os.Exit(143)
}

func TestPiCancellationTerminatesDetachedToolChildren(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process-group fixture")
	}
	previousGrace := piTerminateGraceNanos.Swap(int64(time.Second))
	t.Cleanup(func() { piTerminateGraceNanos.Store(previousGrace) })

	helper, err := os.Executable()
	if err != nil {
		t.Fatalf("test binary path: %v", err)
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	fakePath := filepath.Join(dir, "pi")
	script := "#!/bin/sh\nexec \"$" + piCancelHelperBinEnv + "\"\n"
	writeTestExecutable(t, fakePath, []byte(script))

	backend, err := New("pi", Config{
		ExecutablePath: fakePath,
		BuiltinRuntime: true,
		Logger:         slog.Default(),
		Env: map[string]string{
			piCancelHelperEnv:       "1",
			piCancelHelperBinEnv:    helper,
			piCancelChildPidFileEnv: pidFile,
		},
	})
	if err != nil {
		t.Fatalf("new pi backend: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	session, err := backend.Execute(ctx, "prompt-ignored", ExecOptions{
		ResumeSessionID: filepath.Join(dir, "session.jsonl"),
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	var childPid int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, readErr := os.ReadFile(pidFile)
		if readErr == nil && strings.TrimSpace(string(data)) != "" {
			childPid, _ = strconv.Atoi(strings.TrimSpace(string(data)))
			if childPid > 0 {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if childPid <= 0 {
		t.Fatal("fake pi never started its detached child")
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-childPid, syscall.SIGKILL)
		_ = syscall.Kill(childPid, syscall.SIGKILL)
	})

	cancel()
	result := waitPiResult(t, session, 10*time.Second)
	if result.Status != "aborted" {
		t.Fatalf("status = %q, want aborted (error=%q)", result.Status, result.Error)
	}

	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		err := syscall.Kill(childPid, 0)
		if err == syscall.ESRCH {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("detached bash child %d survived cancellation", childPid)
}
