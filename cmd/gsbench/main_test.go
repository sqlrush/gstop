//go:build darwin || linux

package main

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestCommandContextLeavesInterruptsToOperatingSystem(t *testing.T) {
	ctx := commandContext()
	if ctx == nil {
		t.Fatal("command context is nil")
	}
	if ctx.Done() != nil {
		t.Fatal("command context intercepts process cancellation")
	}
}

func TestGSBenchProcessExitsOnFirstInterrupt(t *testing.T) {
	t.Setenv("GSBENCH_SIGNAL_TEST_PASSWORD", "test-only")
	dir := t.TempDir()
	binary := filepath.Join(dir, "gsbench")
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build gsbench: %v\n%s", err, output)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	configPath := filepath.Join(dir, "gsbench.cfg")
	config := fmt.Sprintf(`[database]
host = 127.0.0.1
port = %d
database = postgres
user = bench
password_env = GSBENCH_SIGNAL_TEST_PASSWORD
connect_timeout = 1m

[run]
scenarios = 101
duration = 1m

[data]
schema = gsbench
`, port)
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	accepted := make(chan net.Conn, 1)
	acceptErr := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			acceptErr <- err
			return
		}
		accepted <- conn
	}()
	var stdout, stderr bytes.Buffer
	command := exec.Command(binary, "doctor", "-c", configPath)
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	var conn net.Conn
	select {
	case conn = <-accepted:
		defer conn.Close()
	case err := <-acceptErr:
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatalf("accept gsbench connection: %v", err)
	case <-time.After(5 * time.Second):
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatalf("gsbench did not enter blocking database connect\nstdout=%s\nstderr=%s", stdout.String(), stderr.String())
	}

	started := time.Now()
	if err := command.Process.Signal(os.Interrupt); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatal(err)
	}
	wait := make(chan error, 1)
	go func() { wait <- command.Wait() }()
	select {
	case err := <-wait:
		if err == nil {
			t.Fatal("gsbench exited normally after SIGINT; want signal termination")
		}
		status, ok := command.ProcessState.Sys().(syscall.WaitStatus)
		if !ok || !status.Signaled() || status.Signal() != syscall.SIGINT {
			t.Fatalf("process status=%v, want SIGINT termination", command.ProcessState)
		}
		if elapsed := time.Since(started); elapsed > time.Second {
			t.Fatalf("first Ctrl+C exit took %s, want <=1s", elapsed)
		}
	case <-time.After(2 * time.Second):
		_ = command.Process.Kill()
		<-wait
		t.Fatal("first Ctrl+C did not terminate gsbench")
	}
}
