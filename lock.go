package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// ErrBusy is returned by acquire when the GPU lock is already held.
var ErrBusy = errors.New("busy")

// Holder records who currently holds a GPU lock (for friendly status output).
type Holder struct {
	Label   string `json:"label"`
	PID     int    `json:"pid"`
	Host    string `json:"host"`
	Started string `json:"started"`
	Cmd     string `json:"cmd"`
}

func dir() string {
	h, err := os.UserHomeDir()
	if err != nil {
		h = "/tmp"
	}
	d := filepath.Join(h, ".gputex")
	_ = os.MkdirAll(d, 0o755)
	return d
}

func lockPath(gpu string) string   { return filepath.Join(dir(), gpu+".lock") }
func holderPath(gpu string) string { return filepath.Join(dir(), gpu+".holder") }

// acquire takes a non-blocking exclusive advisory lock on the gpu's lockfile.
// Keep the returned file open to hold the lock; the OS releases it when the file
// (and thus the process) goes away, so a crash never leaves a stale lock.
func acquire(gpu string) (*os.File, error) {
	f, err := os.OpenFile(lockPath(gpu), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrBusy
		}
		return nil, err
	}
	return f, nil
}

func release(f *os.File) {
	if f == nil {
		return
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_ = f.Close()
}

func writeHolder(gpu string, h Holder) error {
	b, err := json.Marshal(h)
	if err != nil {
		return err
	}
	return os.WriteFile(holderPath(gpu), b, 0o644)
}

func readHolder(gpu string) (Holder, bool) {
	b, err := os.ReadFile(holderPath(gpu))
	if err != nil {
		return Holder{}, false
	}
	var h Holder
	if json.Unmarshal(b, &h) != nil {
		return Holder{}, false
	}
	return h, true
}

func clearHolder(gpu string) { _ = os.Remove(holderPath(gpu)) }

// status probes the lock: if it can be acquired the GPU is free (and any stale
// holder file is cleared); otherwise it's held — and because flock auto-releases
// when the holder dies, a held lock implies a live holder.
func status(gpu string) (held bool, h Holder) {
	f, err := acquire(gpu)
	if errors.Is(err, ErrBusy) {
		h, _ := readHolder(gpu)
		return true, h
	}
	if err != nil {
		return false, Holder{}
	}
	release(f)
	clearHolder(gpu)
	return false, Holder{}
}
