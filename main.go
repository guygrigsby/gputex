// gputex — a single-GPU advisory mutex. Wrap any GPU job so two of them never
// stack on one card (Mac Metal, rig CUDA). The lock auto-releases on exit/crash.
//
//	gputex run    [--gpu ID] [--wait S] "<label>" -- <cmd...>
//	gputex status [--gpu ID]   omit --gpu to show all GPUs in ~/.gputex/gpus
package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func usage() {
	fmt.Fprintln(os.Stderr, `gputex — single-GPU mutex
  gputex run    [--gpu ID] [--wait S | --queue | --preempt | --low] "<label>" -- <cmd...>   acquire, run, release
  gputex status [--gpu ID]   omit --gpu to list all GPUs in ~/.gputex/gpus (one per line)
                             free / busy + holder
  --wait S   poll up to S seconds, then exit 75 if still busy
  --queue    block until it's our turn (waits forever; unqueues on exit/crash)
  --preempt  kill the current holder (TERM then KILL) and take the lock (same host only)
  --low      run as a shared, lowest-priority holder: many --low jobs share the
             card, all yield to (and are evicted by) any normal exclusive job
exit: 0 ok, 75 GPU busy, 1 error, 2 usage`)
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "run":
		runCmd(os.Args[2:])
	case "status":
		statusCmd(os.Args[2:])
	default:
		usage()
	}
}

func runCmd(args []string) {
	gpu, wait, queue, preemptF, low, label, cmd := "default", 0, false, false, false, "", []string(nil)
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			cmd = args[i+1:]
			i = len(args)
		case a == "--gpu":
			i++
			gpu = arg(args, i)
		case strings.HasPrefix(a, "--gpu="):
			gpu = a[len("--gpu="):]
		case a == "--wait":
			i++
			wait, _ = strconv.Atoi(arg(args, i))
		case strings.HasPrefix(a, "--wait="):
			wait, _ = strconv.Atoi(a[len("--wait="):])
		case a == "--queue":
			queue = true
		case a == "--preempt":
			preemptF = true
		case a == "--low":
			low = true
		case label == "":
			label = a
		}
	}
	if label == "" || len(cmd) == 0 {
		usage()
	}

	f := mustAcquire(gpu, wait, queue, preemptF, low)
	defer release(f)

	host, _ := os.Hostname()
	_ = addHolder(gpu, Holder{
		Label: label, PID: os.Getpid(), Host: host,
		Started: time.Now().Format(time.RFC3339), Cmd: strings.Join(cmd, " "),
		Preemptible: low,
	})
	defer removeHolder(gpu, os.Getpid())

	c := exec.Command(cmd[0], cmd[1:]...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "gputex: start:", err)
		os.Exit(1)
	}
	// forward Ctrl-C / TERM to the child so the wrapped job stops cleanly
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		for s := range sigc {
			_ = c.Process.Signal(s)
		}
	}()

	err := c.Wait()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			os.Exit(ee.ExitCode())
		}
		fmt.Fprintln(os.Stderr, "gputex:", err)
		os.Exit(1)
	}
}

func mustAcquire(gpu string, wait int, queue, preemptF, low bool) *os.File {
	// --low: a shared, lowest-priority holder (ComfyUI, llama-swap). Coexists
	// with other --low holders; blocks while an exclusive (training) holder has
	// the card. We never preempt — we yield to everyone.
	if low {
		return must(acquireShared(gpu))
	}

	// --preempt: evict every current holder and take the card by force.
	if preemptF {
		f, err := preempt(gpu, listHolders(gpu))
		if err != nil {
			fmt.Fprintln(os.Stderr, "gputex:", err)
			os.Exit(1)
		}
		return f
	}

	// Normal (exclusive) job. Try once.
	f, err := acquire(gpu)
	if err == nil {
		return f
	}
	if err != ErrBusy {
		fmt.Fprintln(os.Stderr, "gputex:", err)
		os.Exit(1)
	}
	// Busy. If every holder is preemptible (--low), evict them all — a normal job
	// always beats low ones. If any normal holder is present, don't touch it.
	holders := listHolders(gpu)
	if len(holders) > 0 && allPreemptible(holders) {
		pf, err := preempt(gpu, holders)
		if err != nil {
			fmt.Fprintln(os.Stderr, "gputex:", err)
			os.Exit(1)
		}
		return pf
	}
	// Held by another normal job (or an unregistered holder): honor --queue / --wait / plain.
	if queue {
		return must(acquireQueue(gpu))
	}
	deadline := time.Now().Add(time.Duration(wait) * time.Second)
	for {
		f, err := acquire(gpu)
		if err == nil {
			return f
		}
		if err != ErrBusy {
			fmt.Fprintln(os.Stderr, "gputex:", err)
			os.Exit(1)
		}
		if wait <= 0 || time.Now().After(deadline) {
			busyExit(gpu)
		}
		time.Sleep(time.Second)
	}
}

func must(f *os.File, err error) *os.File {
	if err != nil {
		fmt.Fprintln(os.Stderr, "gputex:", err)
		os.Exit(1)
	}
	return f
}

func allPreemptible(holders []Holder) bool {
	for _, h := range holders {
		if !h.Preemptible {
			return false
		}
	}
	return true
}

func busyExit(gpu string) {
	h := listHolders(gpu)
	if len(h) > 0 {
		fmt.Fprintf(os.Stderr, "gputex: GPU %q busy — held by %q (pid %d on %s, since %s)\n",
			gpu, h[0].Label, h[0].PID, h[0].Host, h[0].Started)
	} else {
		fmt.Fprintf(os.Stderr, "gputex: GPU %q busy\n", gpu)
	}
	os.Exit(75)
}

func statusCmd(args []string) {
	gpu, explicit := "default", false
	for i := 0; i < len(args); i++ {
		if args[i] == "--gpu" {
			i++
			gpu = arg(args, i)
			explicit = true
		} else if strings.HasPrefix(args[i], "--gpu=") {
			gpu = args[i][len("--gpu="):]
			explicit = true
		}
	}
	if explicit {
		printStatus(gpu)
		return
	}
	for _, g := range knownGPUs() {
		printStatus(g)
	}
}

// knownGPUs returns GPU IDs from ~/.gputex/gpus (one per line, # comments ok),
// falling back to lock-file discovery, then "default".
func knownGPUs() []string {
	if b, err := os.ReadFile(filepath.Join(dir(), "gpus")); err == nil {
		var gpus []string
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			if line != "" && !strings.HasPrefix(line, "#") {
				gpus = append(gpus, line)
			}
		}
		if len(gpus) > 0 {
			return gpus
		}
	}
	ents, _ := os.ReadDir(dir())
	var gpus []string
	for _, e := range ents {
		if g, ok := strings.CutSuffix(e.Name(), ".lock"); ok {
			gpus = append(gpus, g)
		}
	}
	if len(gpus) == 0 {
		return []string{"default"}
	}
	return gpus
}

func printStatus(gpu string) {
	held, holders := status(gpu)
	if !held {
		fmt.Printf("FREE  %s\n", gpu)
		return
	}
	if len(holders) == 0 {
		fmt.Printf("BUSY  %s — holder not yet registered\n", gpu)
		return
	}
	for _, h := range holders {
		kind := "exclusive"
		if h.Preemptible {
			kind = "low"
		}
		fmt.Printf("BUSY  %s — %q (%s, pid %d on %s, since %s)\n", gpu, h.Label, kind, h.PID, h.Host, h.Started)
	}
}

func arg(args []string, i int) string {
	if i < len(args) {
		return args[i]
	}
	usage()
	return ""
}
