//go:build !unix

package main

import (
	"os"
	"os/exec"
	"syscall"
)

// interrupts are what Windows delivers to a console program: Ctrl-C and
// Ctrl-Break as os.Interrupt, and closing the console, logging off or shutting
// down as SIGTERM. See command_unix.go.
var interrupts = []os.Signal{os.Interrupt, syscall.SIGTERM}

// There is no process group to start the command in. A console process group
// is a different thing, and one created for the command would stop Ctrl-C from
// reaching it. Ending everything a command started needs a job object, which
// the standard library does not wrap, so on Windows hunk ends sh and relies on
// verifyWaitDelay for whatever sh started that still holds its output.
func ownGroup(*exec.Cmd) {}

// signalGroup ends sh. Windows has no signal to forward: os.Process.Signal
// there can only kill.
func signalGroup(p *os.Process, _ os.Signal) { _ = p.Kill() }

func killGroup(p *os.Process) { _ = p.Kill() }

// groupAlive is false: once sh is gone there is no group left to ask about.
func groupAlive(*os.Process) bool { return false }

// raise cannot re-raise a signal on Windows, so the caller exits 128 plus it.
func raise(os.Signal) {}
