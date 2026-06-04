package main

import (
	"os"
	"testing"
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

func TestMain(m *testing.M) { os.Exit(m.Run()) }
