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
	// PID must be a live process or listHolders prunes it; use our own.
	want := Holder{Label: "kohya-train", PID: os.Getpid(), Host: "rig", Started: "now", Cmd: "bash 03_train.sh", Preemptible: true}
	if err := addHolder(gpu, want); err != nil {
		t.Fatal(err)
	}
	got := listHolders(gpu)
	if len(got) != 1 || got[0].Label != want.Label || got[0].PID != want.PID || got[0].Cmd != want.Cmd || !got[0].Preemptible {
		t.Fatalf("roundtrip: got %+v", got)
	}
}

func TestListHoldersPrunesDead(t *testing.T) {
	isolate(t)
	gpu := "test"
	// a registry entry for a dead PID must be pruned on read
	_ = addHolder(gpu, Holder{Label: "ghost", PID: 999999})
	if h := listHolders(gpu); len(h) != 0 {
		t.Fatalf("want dead holder pruned, got %+v", h)
	}
}

func TestListHoldersPrunesReusedPID(t *testing.T) {
	isolate(t)
	gpu := "test"
	// pid reuse: the recorded pid is alive but belongs to a different process
	// (start time mismatch) — must be pruned, not treated as the holder
	_ = addHolder(gpu, Holder{Label: "ghost", PID: os.Getpid(), StartTime: 12345})
	if h := listHolders(gpu); len(h) != 0 {
		t.Fatalf("want reused-pid holder pruned, got %+v", h)
	}
}

func TestStatusClearsStaleHolder(t *testing.T) {
	isolate(t)
	gpu := "test"
	// a holder file with no lock held = stale; status (free) should clear it
	_ = addHolder(gpu, Holder{Label: "ghost", PID: os.Getpid()})
	if held, _ := status(gpu); held {
		t.Fatal("status: want free (lock not held), got held")
	}
	if h := listHolders(gpu); len(h) != 0 {
		t.Fatal("stale holder file should have been cleared")
	}
}

func TestPreemptFreeTakesIt(t *testing.T) {
	isolate(t)
	f, err := preempt("test", nil) // nothing held: should just acquire
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
	// live PID (so listHolders keeps it) but a different host — can't signal it
	_ = addHolder(gpu, Holder{Label: "remote", PID: os.Getpid(), Host: "some-other-host"})
	if _, err := preempt(gpu, listHolders(gpu)); err == nil {
		t.Fatal("preempt: want refusal for cross-host holder, got nil")
	}
}

func TestSharedHoldersCoexist(t *testing.T) {
	isolate(t)
	gpu := "test"
	a, err := acquireShared(gpu)
	if err != nil {
		t.Fatalf("first shared: %v", err)
	}
	defer release(a)
	// a second shared lock must be granted alongside the first (readers coexist)
	got := make(chan *os.File, 1)
	go func() { f, _ := acquireShared(gpu); got <- f }()
	select {
	case f := <-got:
		if f == nil {
			t.Fatal("second shared acquire failed")
		}
		release(f)
	case <-time.After(time.Second):
		t.Fatal("second shared lock blocked; readers should coexist")
	}
	// but an exclusive (non-blocking) acquire must see the card busy
	if _, err := acquire(gpu); err != ErrBusy {
		t.Fatalf("exclusive over shared: want ErrBusy, got %v", err)
	}
}

func TestMain(m *testing.M) { os.Exit(m.Run()) }
