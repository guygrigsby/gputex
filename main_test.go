package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func buildGputex(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "gputex")
	out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput()
	if err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return bin
}

// registered reports whether at least one holder is recorded under home's
// gputex registry for gpu.
func registered(home, gpu string) bool {
	ents, err := os.ReadDir(filepath.Join(home, ".gputex", gpu+".holders"))
	return err == nil && len(ents) > 0
}

func waitFor(t *testing.T, cond func() bool, d time.Duration, what string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// A --queue waiter that gets Ctrl-C'd while blocked must exit, never run its
// command, and (being abandoned) leave nothing behind — the kernel unqueues it.
func TestQueuedWaiterCancelsOnSignal(t *testing.T) {
	bin := buildGputex(t)
	home := t.TempDir()
	env := append(os.Environ(), "HOME="+home)
	gpu := "test"

	holder := exec.Command(bin, "run", "holder", "--gpu", gpu, "--", "sleep", "30")
	holder.Env = env
	if err := holder.Start(); err != nil {
		t.Fatalf("start holder: %v", err)
	}
	defer holder.Process.Kill()

	waitFor(t, func() bool { return registered(home, gpu) }, 3*time.Second, "holder to take the lock")

	marker := filepath.Join(t.TempDir(), "ran")
	waiter := exec.Command(bin, "run", "waiter", "--gpu", gpu, "--queue", "--", "touch", marker)
	waiter.Env = env
	if err := waiter.Start(); err != nil {
		t.Fatalf("start waiter: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- waiter.Wait() }()

	// It must stay blocked in the queue, not bail with exit 75. (This is what
	// distinguishes a real --queue from the default non-blocking path, which
	// would exit immediately on the busy card.)
	select {
	case err := <-done:
		t.Fatalf("waiter exited before being signalled (%v); want it blocked in the queue", err)
	case <-time.After(500 * time.Millisecond):
	}

	if err := waiter.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("signal waiter: %v", err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("waiter exited 0; want non-zero after SIGINT cancel")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter did not exit after SIGINT")
	}

	if _, err := os.Stat(marker); err == nil {
		t.Fatal("waiter ran its command despite being cancelled while queued")
	}
}

// A --low (preemptible) holder must yield to a normal job: the normal job takes
// the card by auto-preempting it (no --preempt flag needed) and runs, while the
// low holder is terminated. This is how ComfyUI gives the R9700 back to training.
func TestNormalJobPreemptsLowHolder(t *testing.T) {
	bin := buildGputex(t)
	home := t.TempDir()
	env := append(os.Environ(), "HOME="+home)
	gpu := "test"

	low := exec.Command(bin, "run", "comfyui", "--gpu", gpu, "--low", "--", "sleep", "30")
	low.Env = env
	if err := low.Start(); err != nil {
		t.Fatalf("start low holder: %v", err)
	}
	defer low.Process.Kill()
	lowDone := make(chan error, 1)
	go func() { lowDone <- low.Wait() }()

	waitFor(t, func() bool { return registered(home, gpu) }, 3*time.Second, "low holder to take the lock")

	// A plain normal job (no --queue/--preempt) must NOT exit 75 here — it should
	// preempt the low holder and run its command.
	marker := filepath.Join(t.TempDir(), "ran")
	trainer := exec.Command(bin, "run", "trainer", "--gpu", gpu, "--", "touch", marker)
	trainer.Env = env
	if out, err := trainer.CombinedOutput(); err != nil {
		t.Fatalf("trainer should preempt the low holder and run; got err=%v out=%s", err, out)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("trainer did not run its command after preempting the low holder")
	}

	// The low holder must have been terminated (it yielded the card).
	select {
	case <-lowDone:
	case <-time.After(2 * time.Second):
		t.Fatal("low holder still alive after a normal job took the card; want it preempted")
	}
}

// The coexist money-path: two --low holders share the card at once, and a single
// normal job evicts BOTH and takes it exclusively (ComfyUI + llama-swap yielding
// the R9700 to training).
func TestNormalJobEvictsAllLowHolders(t *testing.T) {
	bin := buildGputex(t)
	home := t.TempDir()
	env := append(os.Environ(), "HOME="+home)
	gpu := "test"

	starts := make([]*exec.Cmd, 2)
	dones := make([]chan error, 2)
	for i, name := range []string{"comfyui", "llama-swap"} {
		c := exec.Command(bin, "run", name, "--gpu", gpu, "--low", "--", "sleep", "30")
		c.Env = env
		if err := c.Start(); err != nil {
			t.Fatalf("start %s: %v", name, err)
		}
		defer c.Process.Kill()
		d := make(chan error, 1)
		go func(c *exec.Cmd) { d <- c.Wait() }(c)
		starts[i], dones[i] = c, d
	}

	// both low holders must register and coexist (shared lock)
	waitFor(t, func() bool {
		ents, _ := os.ReadDir(filepath.Join(home, ".gputex", gpu+".holders"))
		return len(ents) == 2
	}, 3*time.Second, "two low holders to share the card")

	marker := filepath.Join(t.TempDir(), "ran")
	trainer := exec.Command(bin, "run", "trainer", "--gpu", gpu, "--", "touch", marker)
	trainer.Env = env
	if out, err := trainer.CombinedOutput(); err != nil {
		t.Fatalf("trainer should evict both low holders and run; err=%v out=%s", err, out)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("trainer did not run after evicting the low holders")
	}
	for i := range dones {
		select {
		case <-dones[i]:
		case <-time.After(2 * time.Second):
			t.Fatalf("low holder %d still alive after eviction", i)
		}
	}
}
