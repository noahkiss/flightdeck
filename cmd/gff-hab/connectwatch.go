package main

import (
	"fmt"
	"os"
	"time"

	"github.com/noahkiss/flightdeck/internal/devicestore"
)

// devicePoll is how often `gff-hab connect -device` looks at the device list.
const devicePoll = time.Second

// devicePaired says whether the device list at path holds id as a paired device. Every other
// outcome is an error, an unreadable list among them: the check fails closed (transport.md 6.6).
func devicePaired(path, id string) error {
	devs, err := (&devicestore.Store{Path: path}).List()
	if err != nil {
		return fmt.Errorf("the device list cannot be read, so this device's session ends: %v", err)
	}
	for _, d := range devs {
		if d.ID != id {
			continue
		}
		if d.State == devicestore.StatePaired {
			return nil
		}
		return fmt.Errorf("device %s is %s", id, d.State)
	}
	return fmt.Errorf("device %s is not in the device list %s", id, path)
}

// watchDevice calls stop once the device is no longer paired in the list at path. It reads the
// list again only when the file's size, time or identity changed. It returns when done closes.
func watchDevice(path, id string, every time.Duration, stop func(error), done <-chan struct{}) {
	var last os.FileInfo
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
		}
		fi, err := os.Stat(path)
		if err == nil && last != nil && os.SameFile(fi, last) && fi.Size() == last.Size() && fi.ModTime().Equal(last.ModTime()) {
			continue
		}
		last = fi
		if err := devicePaired(path, id); err != nil {
			stop(err)
			return
		}
	}
}
