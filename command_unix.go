//go:build unix

package main

import (
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
)

// interrupts are the signals that end a --verify or --try command and put the
// batch back rather than kill hunk where it stands. SIGKILL is not among them
// because nothing can be.
var interrupts = []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP}

// ownGroup starts the command as the leader of a process group of its own, so
// that everything it starts can be ended together. Until 2026-10-06 it shared
// hunk's group, and a signal to hunk alone left it running, orphaned.
func ownGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// signalGroup sends sig to every process in the group p leads. Every signal
// that reaches here is a syscall.Signal; one that were not would send signal 0,
// which is nothing, and the SIGKILL after the grace would end the group.
func signalGroup(p *os.Process, sig os.Signal) {
	s, _ := sig.(syscall.Signal)
	_ = syscall.Kill(-p.Pid, s)
}

// killGroup is SIGKILL to the whole group, for what the first signal did not
// end.
func killGroup(p *os.Process) { _ = syscall.Kill(-p.Pid, syscall.SIGKILL) }

// groupAlive reports whether anything is left in the group p led. Signal 0
// sends nothing and says whether there was anybody to send it to; EPERM means
// there was, and that it is not ours to signal.
func groupAlive(p *os.Process) bool {
	err := syscall.Kill(-p.Pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// raise dies of sig, as hunk would have before it caught it, so that a parent
// sees a signal death and not an exit code (§4.1). It returns only if the
// signal did not end the process, and the caller then exits 128 plus it.
//
// No test's coverage can count it: it ends the process before the counters are
// written. TestHunkDiesOfTheSignal asserts what it does instead.
func raise(sig os.Signal) {
	signal.Reset(sig)
	if p, err := os.FindProcess(os.Getpid()); err == nil {
		_ = p.Signal(sig)
	}
	time.Sleep(time.Second)
}
