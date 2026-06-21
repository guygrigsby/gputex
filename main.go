// gputex — a single-GPU advisory mutex. Wrap any GPU job so two of them never
// stack on one card (Mac Metal, rig CUDA). The lock auto-releases on exit/crash.
//
//	gputex run    [--gpu ID] [--wait S] "<label>" -- <cmd...>
//	gputex status [--gpu ID]
package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func usage() {
	fmt.Fprintln(os.Stderr, `gputex — single-GPU mutex
  gputex run    [--gpu ID] [--wait S | --queue | --preempt | --low] "<label>" -- <cmd...>   acquire, run, release
  gputex status [--gpu ID]                                             free / busy + holder
  --wait S   poll up to S seconds, then exit 75 if still busy
  --queue    block until it's our turn (waits forever; unqueues on exit/crash)
  --preempt  kill the current holder (TERM then KILL) and take the lock (same host only)
  --low      run as a lowest-priority, preemptible holder: yield to every other
             job (block till free), and let any normal job auto-preempt you
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
	_ = writeHolder(gpu, Holder{
		Label: label, PID: os.Getpid(), Host: host,
		Started: time.Now().Format(time.RFC3339), Cmd: strings.Join(cmd, " "),
		Preemptible: low,
	})
	defer clearHolder(gpu)

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
	// --preempt: kick out the current holder and take the lock.
	if preemptF {
		f, err := preempt(gpu)
		if err != nil {
			fmt.Fprintln(os.Stderr, "gputex:", err)
			os.Exit(1)
		}
		return f
	}
	// --low: a lowest-priority, preemptible holder (e.g. ComfyUI). We never
	// preempt — we yield to everyone — so just block until the card is free,
	// then hold it until a normal job preempts us (Preemptible is written by
	// the caller so that auto-preempt below can find us).
	if low {
		f, err := acquireQueue(gpu)
		if err != nil {
			fmt.Fprintln(os.Stderr, "gputex:", err)
			os.Exit(1)
		}
		return f
	}
	// Normal job. Try once; if the card is held by a preemptible (--low) holder,
	// kick it out — a normal job always beats a low one — without disturbing
	// another normal holder.
	f, err := acquire(gpu)
	if err == nil {
		return f
	}
	if err != ErrBusy {
		fmt.Fprintln(os.Stderr, "gputex:", err)
		os.Exit(1)
	}
	if h, ok := readHolder(gpu); ok && h.Preemptible {
		pf, err := preempt(gpu)
		if err != nil {
			fmt.Fprintln(os.Stderr, "gputex:", err)
			os.Exit(1)
		}
		return pf
	}
	// Held by a normal job: honor --queue / --wait / plain.
	if queue {
		qf, err := acquireQueue(gpu)
		if err != nil {
			fmt.Fprintln(os.Stderr, "gputex:", err)
			os.Exit(1)
		}
		return qf
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
			h, _ := readHolder(gpu)
			fmt.Fprintf(os.Stderr, "gputex: GPU %q busy — held by %q (pid %d on %s, since %s)\n",
				gpu, h.Label, h.PID, h.Host, h.Started)
			os.Exit(75)
		}
		time.Sleep(time.Second)
	}
}

func statusCmd(args []string) {
	gpu := "default"
	for i := 0; i < len(args); i++ {
		if args[i] == "--gpu" {
			i++
			gpu = arg(args, i)
		} else if strings.HasPrefix(args[i], "--gpu=") {
			gpu = args[i][len("--gpu="):]
		}
	}
	if held, h := status(gpu); held {
		fmt.Printf("BUSY  %s — %q (pid %d on %s, since %s)\n", gpu, h.Label, h.PID, h.Host, h.Started)
	} else {
		fmt.Printf("FREE  %s\n", gpu)
	}
}

func arg(args []string, i int) string {
	if i < len(args) {
		return args[i]
	}
	usage()
	return ""
}
