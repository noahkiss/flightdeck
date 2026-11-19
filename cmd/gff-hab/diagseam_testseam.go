//go:build testseam

package main

import "os"

func init() {
	if v := os.Getenv("GFF_TEST_HAB_PANIC"); v != "" {
		diagStartSeam = func() {
			go func() { panic("test seam: " + v) }()
		}
	}
}
