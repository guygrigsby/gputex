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
  gputex run    [--gpu ID] [--wait S | --queue] "<label>" -- <cmd...>   acquire, run, release
  gputex status [--gpu ID]                                             free / busy + holder
  --wait S   poll up to S seconds, then exit 75 if still busy
  --queue    block until it's our turn (waits forever; unqueues on exit/crash)
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
	gpu, wait, queue, label, cmd := "default", 0, false, "", []string(nil)
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
		case label == "":
			label = a
		}
	}
	if label == "" || len(cmd) == 0 {
		usage()
	}

	f := mustAcquire(gpu, wait, queue)
	defer release(f)

	host, _ := os.Hostname()
	_ = writeHolder(gpu, Holder{label, os.Getpid(), host, time.Now().Format(time.RFC3339), strings.Join(cmd, " ")})
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

func mustAcquire(gpu string, wait int, queue bool) *os.File {
	// --queue: block in the kernel until it's our turn (waits forever, ignores --wait).
	if queue {
		f, err := acquireQueue(gpu)
		if err != nil {
			fmt.Fprintln(os.Stderr, "gputex:", err)
			os.Exit(1)
		}
		return f
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
