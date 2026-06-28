package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// ErrBusy is returned by acquire when the GPU lock can't be taken right now.
var ErrBusy = errors.New("busy")

// Holder records who holds a GPU lock (for status and for preemption).
type Holder struct {
	Label string `json:"label"`
	// Framework is the stack the job runs on (e.g. "lmkit-go", "pytorch"), set by
	// gputex run --framework. Empty if unset. Surfaced by the metrics exporter so
	// every GPU job is labelled by framework on the workers dashboard.
	Framework string `json:"framework,omitempty"`
	PID       int    `json:"pid"`
	Host      string `json:"host"`
	Started   string `json:"started"`
	Cmd       string `json:"cmd"`
	// Preemptible marks a lowest-priority shared holder (gputex run --low, e.g.
	// ComfyUI / llama-swap). Several may share the card at once; any normal
	// (exclusive) job evicts them all on acquire, so the card frees for training
	// without low holders ever blocking a trainer or each other.
	Preemptible bool `json:"preemptible,omitempty"`
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
func holdersDir(gpu string) string { return filepath.Join(dir(), gpu+".holders") }
func holderFile(gpu string, pid int) string {
	return filepath.Join(holdersDir(gpu), strconv.Itoa(pid)+".json")
}

// The lock is a single flock'd file used as a reader/writer lock:
//   - normal (exclusive) jobs take LOCK_EX — one at a time, and only once every
//     shared holder has released.
//   - --low (shared) holders take LOCK_SH — many coexist, but all block while an
//     exclusive holder has the card.
// flock auto-releases when the holding process dies, so a crash never strands
// the lock. The holder registry (holdersDir) is a separate, liveness-checked
// record of *who* holds it, because one shared lock can have many holders and
// the kernel won't tell us their PIDs.

// acquire takes a non-blocking exclusive lock: fails with ErrBusy if anyone
// (exclusive or shared) holds it.
func acquire(gpu string) (*os.File, error) { return flockOpen(gpu, syscall.LOCK_EX|syscall.LOCK_NB) }

// acquireQueue blocks in the kernel for an exclusive lock until it's ours.
func acquireQueue(gpu string) (*os.File, error) { return flockOpen(gpu, syscall.LOCK_EX) }

// acquireShared blocks for a shared lock: coexists with other shared holders,
// waits while an exclusive holder (training) has the card.
func acquireShared(gpu string) (*os.File, error) { return flockOpen(gpu, syscall.LOCK_SH) }

func flockOpen(gpu string, how int) (*os.File, error) {
	f, err := os.OpenFile(lockPath(gpu), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
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

// --- holder registry: a dir of <pid>.json, liveness-checked on read ---

func addHolder(gpu string, h Holder) error {
	if err := os.MkdirAll(holdersDir(gpu), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(h)
	if err != nil {
		return err
	}
	return os.WriteFile(holderFile(gpu, h.PID), b, 0o644)
}

func removeHolder(gpu string, pid int) { _ = os.Remove(holderFile(gpu, pid)) }

// listHolders returns the live holders, pruning any whose process is gone (a
// crashed holder's flock auto-releases, but its registry file lingers until we
// notice the PID is dead).
func listHolders(gpu string) []Holder {
	ents, err := os.ReadDir(holdersDir(gpu))
	if err != nil {
		return nil
	}
	var out []Holder
	for _, e := range ents {
		p := filepath.Join(holdersDir(gpu), e.Name())
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var h Holder
		if json.Unmarshal(b, &h) != nil || !alive(h.PID) {
			_ = os.Remove(p)
			continue
		}
		out = append(out, h)
	}
	return out
}

// alive reports whether pid is a live process we could signal.
func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// preempt evicts the given holders (SIGTERM, then SIGKILL after a ~10s grace)
// and takes the exclusive lock once they're gone. It re-scans each pass so a
// low holder that (re)appears mid-eviction is signalled too. Refuses if any
// holder is on another host — a remote PID isn't ours to signal.
func preempt(gpu string, holders []Holder) (*os.File, error) {
	host, _ := os.Hostname()
	for _, h := range holders {
		if h.Host != host {
			return nil, fmt.Errorf("held by pid %d on %s — can't preempt across hosts", h.PID, h.Host)
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		for _, h := range listHolders(gpu) {
			if h.Host == host && h.PID > 0 {
				_ = syscall.Kill(h.PID, syscall.SIGTERM)
			}
		}
		if f, err := acquire(gpu); err == nil {
			return f, nil
		} else if !errors.Is(err, ErrBusy) {
			return nil, err
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, h := range listHolders(gpu) {
		if h.Host == host && h.PID > 0 {
			_ = syscall.Kill(h.PID, syscall.SIGKILL)
		}
	}
	return acquireQueue(gpu) // block until the dead holders' lock frees
}

// status reports whether the card is held (probes the flock so a holder that
// hasn't registered yet still counts) and lists the registered holders.
func status(gpu string) (held bool, holders []Holder) {
	f, err := acquire(gpu)
	if errors.Is(err, ErrBusy) {
		return true, listHolders(gpu)
	}
	if err != nil {
		return false, nil
	}
	// free: drop any stale registry entries
	release(f)
	_ = os.RemoveAll(holdersDir(gpu))
	return false, nil
}
