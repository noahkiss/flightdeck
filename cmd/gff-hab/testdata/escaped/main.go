// A process fixture: its child leaves the pane's session and retains its environment.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	if os.Args[1] == "child" {
		signal.Ignore(syscall.SIGHUP, syscall.SIGTERM)
		if err := os.WriteFile(os.Args[2], []byte(fmt.Sprint(os.Getpid())), 0600); err != nil {
			panic(err)
		}
	} else {
		cmd := exec.Command(os.Args[0], "child", os.Args[1])
		cmd.Env = os.Environ()
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			panic(err)
		}
	}
	for {
		time.Sleep(time.Hour)
	}
}
