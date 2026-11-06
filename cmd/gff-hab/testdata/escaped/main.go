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
	if os.Args[1] == "child" || os.Args[1] == "child-term" {
		signal.Ignore(syscall.SIGHUP)
		if os.Args[1] == "child-term" {
			ch := make(chan os.Signal, 1)
			signal.Notify(ch, syscall.SIGTERM)
			go func() { <-ch; _ = os.WriteFile(os.Args[2]+".term", []byte("cleaned"), 0600); os.Exit(0) }()
		} else {
			signal.Ignore(syscall.SIGTERM)
		}
		if err := os.WriteFile(os.Args[2], []byte(fmt.Sprint(os.Getpid())), 0600); err != nil {
			panic(err)
		}
	} else {
		for _, mode := range []string{"child", "child-term"} {
			path := os.Args[1]
			if mode == "child-term" {
				path += ".graceful"
			}
			cmd := exec.Command(os.Args[0], mode, path)
			cmd.Env = os.Environ()
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			if err := cmd.Start(); err != nil {
				panic(err)
			}
		}
	}
	for {
		time.Sleep(time.Hour)
	}
}
