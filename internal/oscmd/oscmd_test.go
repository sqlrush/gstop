package oscmd

import (
	"testing"
	"time"

	"gstop/internal/logging"
)

func TestRunnerTimeoutStopsForegroundCommand(t *testing.T) {
	runner := New(logging.New("oscmd-test", ""), time.Second, 50*time.Millisecond)
	start := time.Now()
	if _, ok := runner.Run("sleep 30", true); ok {
		t.Fatal("timed-out command unexpectedly succeeded")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("timed-out command returned after %v, want under 1s", elapsed)
	}
}

func TestRunnerCancelStopsForegroundCommand(t *testing.T) {
	runner := New(logging.New("oscmd-test", ""), time.Second, 30*time.Second)
	done := make(chan bool, 1)
	go func() {
		_, ok := runner.Run("sleep 30", true)
		done <- ok
	}()
	time.Sleep(50 * time.Millisecond)
	runner.Cancel()

	select {
	case ok := <-done:
		if ok {
			t.Fatal("canceled command unexpectedly succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("Runner.Cancel did not stop an in-flight command")
	}
}
