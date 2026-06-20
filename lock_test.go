package main

import (
	"os"
	"testing"
	"time"
)

// isolate ~/.gputex into a temp dir so tests never touch real locks.
func isolate(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
}

func TestAcquireBusyRelease(t *testing.T) {
	isolate(t)
	gpu := "test"

	f1, err := acquire(gpu)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	// a second acquire (separate open file description) must see it busy
	if _, err := acquire(gpu); err != ErrBusy {
		t.Fatalf("second acquire: want ErrBusy, got %v", err)
	}

	if held, _ := status(gpu); !held {
		t.Fatal("status: want held while locked")
	}

	release(f1)

	if held, _ := status(gpu); held {
		t.Fatal("status: want free after release")
	}

	f2, err := acquire(gpu)
	if err != nil {
		t.Fatalf("re-acquire after release: %v", err)
	}
	release(f2)
}

// --queue blocks in LOCK_EX until the holder releases, then proceeds.
func TestQueueBlocksUntilRelease(t *testing.T) {
	isolate(t)
	gpu := "test"

	f1, err := acquire(gpu)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	got := make(chan *os.File, 1)
	go func() {
		f, err := acquireQueue(gpu) // must block while f1 holds the lock
		if err != nil {
			t.Errorf("acquireQueue: %v", err)
			got <- nil
			return
		}
		got <- f
	}()

	// while we hold the lock, the queued waiter must not proceed
	select {
	case <-got:
		t.Fatal("acquireQueue returned while lock held; want it to block")
	case <-time.After(200 * time.Millisecond):
	}

	release(f1)

	select {
	case f := <-got:
		if f == nil {
			t.Fatal("acquireQueue failed")
		}
		release(f)
	case <-time.After(2 * time.Second):
		t.Fatal("acquireQueue did not unblock within 2s of release")
	}
}

func TestHolderRoundTrip(t *testing.T) {
	isolate(t)
	gpu := "test"
	want := Holder{Label: "kohya-train", PID: 4242, Host: "rig", Started: "now", Cmd: "bash 03_train.sh"}
	if err := writeHolder(gpu, want); err != nil {
		t.Fatal(err)
	}
	got, ok := readHolder(gpu)
	if !ok || got.Label != want.Label || got.PID != want.PID || got.Cmd != want.Cmd {
		t.Fatalf("roundtrip: got %+v ok=%v", got, ok)
	}
}

func TestStatusClearsStaleHolder(t *testing.T) {
	isolate(t)
	gpu := "test"
	// a holder file with no lock held = stale; status (free) should clear it
	_ = writeHolder(gpu, Holder{Label: "ghost", PID: 999999})
	if held, _ := status(gpu); held {
		t.Fatal("status: want free (lock not held), got held")
	}
	if _, ok := readHolder(gpu); ok {
		t.Fatal("stale holder file should have been cleared")
	}
}

func TestPreemptFreeTakesIt(t *testing.T) {
	isolate(t)
	f, err := preempt("test") // nothing held: should just acquire
	if err != nil {
		t.Fatalf("preempt on free lock: %v", err)
	}
	release(f)
}

func TestPreemptRefusesCrossHost(t *testing.T) {
	isolate(t)
	gpu := "test"
	hold, err := acquire(gpu) // keep it busy so preempt reaches the holder check
	if err != nil {
		t.Fatal(err)
	}
	defer release(hold)
	_ = writeHolder(gpu, Holder{Label: "remote", PID: 4242, Host: "some-other-host"})
	if _, err := preempt(gpu); err == nil {
		t.Fatal("preempt: want refusal for cross-host holder, got nil")
	}
}

func TestMain(m *testing.M) { os.Exit(m.Run()) }
