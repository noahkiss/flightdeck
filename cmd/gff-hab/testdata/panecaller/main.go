// A process fixture for the pane caller check (hab-logic-api.md 4.1). Run in a pane, it starts
// a copy of itself with an empty environment, so GFF_HAB is gone, which dials the control
// socket directly. "direct" waits for that copy. "orphan" exits at once, so the copy loses its
// parent and keeps only the pane's session. The copy writes what the hab answered to the out
// file: one line per call, the call and its error kind, or "ok".
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
)

func main() {
	mode, sock, out := os.Args[1], os.Args[2], os.Args[3]
	if mode == "dial" {
		// The pane ends when its first process exits; its terminal then hangs up this copy.
		signal.Ignore(syscall.SIGHUP)
		write(out+".ready", "")
		if parent := os.Args[4]; parent != "" {
			// Wait until the parent that started this copy has exited.
			for i := 0; i < 500 && fmt.Sprint(os.Getppid()) == parent; i++ {
				time.Sleep(10 * time.Millisecond)
			}
		}
		write(out, dial(sock))
		return
	}
	parent := ""
	if mode == "orphan" {
		parent = fmt.Sprint(os.Getpid())
	}
	cmd := exec.Command(os.Args[0], "dial", sock, out, parent)
	cmd.Env = []string{}
	if err := cmd.Start(); err != nil {
		panic(err)
	}
	if mode == "direct" {
		cmd.Wait()
		return
	}
	for i := 0; i < 500; i++ {
		if _, err := os.Stat(out + ".ready"); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func dial(sock string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var lines []string
	if os.Getenv("GFF_HAB") != "" {
		lines = append(lines, "env GFF_HAB is set")
	}
	c, err := control.Dial(sock)
	if err != nil {
		return "dial " + err.Error()
	}
	_, err = c.Hello(ctx, api.ClientInfo{Name: "panecaller", Version: "0", Kind: api.ClientKindCli}, "", "")
	lines = append(lines, "hello "+kind(err))
	err = c.Call(ctx, "devices.list", api.DevicesListParams{}, nil)
	lines = append(lines, "devices.list "+kind(err))
	err = c.Call(ctx, "structure.get", api.StructureGetParams{}, nil)
	lines = append(lines, "structure.get "+kind(err))
	c.Close()
	h, err := control.Dial(sock)
	if err != nil {
		return "dial " + err.Error()
	}
	_, err = h.HelloAs(ctx, api.HelloParams{Client: api.ClientInfo{Name: "gff-hab", Version: "0", Kind: api.ClientKindHab},
		Peer: &api.PeerHello{DeviceID: "SHA256:claimed", Host: "elsewhere"}})
	lines = append(lines, "hello-hab "+kind(err))
	h.Close()
	return strings.Join(lines, "\n") + "\n"
}

func kind(err error) string {
	if err == nil {
		return "ok"
	}
	var ce *control.Error
	if errors.As(err, &ce) {
		return string(ce.Data.Kind)
	}
	return "error"
}

func write(path, s string) {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(s), 0o600); err != nil {
		panic(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		panic(err)
	}
}
