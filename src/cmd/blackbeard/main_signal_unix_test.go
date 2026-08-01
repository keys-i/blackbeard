//go:build !windows

package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestSecondInterruptRestoresDefaultHandler(t *testing.T) {
	if os.Getenv("BLACKBEARD_SIGNAL_TEST") == "child" {
		ctx, stop := commandContext()
		defer stop()
		fmt.Println("ready")
		<-ctx.Done()
		fmt.Println("cancelled")
		select {}
	}

	command := exec.Command(os.Args[0], "-test.run=^TestSecondInterruptRestoresDefaultHandler$")
	command.Env = append(os.Environ(), "BLACKBEARD_SIGNAL_TEST=child")
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = command.Process.Kill() })

	lines := bufio.NewScanner(stdout)
	if !lines.Scan() || lines.Text() != "ready" {
		t.Fatalf("child readiness = %q, %v", lines.Text(), lines.Err())
	}
	if err := command.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	if !lines.Scan() || lines.Text() != "cancelled" {
		t.Fatalf("first interrupt = %q, %v", lines.Text(), lines.Err())
	}
	if err := command.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		exit, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("second interrupt error = %v", err)
		}
		status, ok := exit.Sys().(syscall.WaitStatus)
		if !ok || !status.Signaled() || status.Signal() != syscall.SIGINT {
			t.Fatalf("second interrupt status = %v", exit.Sys())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second interrupt did not force termination")
	}
}
