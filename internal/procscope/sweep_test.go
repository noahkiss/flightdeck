package procscope

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noahkiss/flightdeck/internal/hook"
	"golang.org/x/sys/unix"
)

func TestProcFixture(t *testing.T) {
	root := t.TempDir()
	token := strings.Repeat("a", 32)
	makeProc := func(pid int, state, uid, start, env string) {
		d := filepath.Join(root, fmt.Sprint(pid))
		if err := os.MkdirAll(d, 0700); err != nil {
			t.Fatal(err)
		}
		fields := append([]string{state, "1", "2"}, strings.Fields(strings.Repeat("0 ", 16))...)
		fields = append(fields, start)
		for name, data := range map[string]string{"stat": fmt.Sprintf("%d (name with ) spaces) %s", pid, strings.Join(fields, " ")), "status": "Uid:\t" + uid + "\t" + uid + "\t0\t0\n", "environ": env} {
			if err := os.WriteFile(filepath.Join(d, name), []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	good := "GFF_HAB=" + hook.Addr("/old/socket", token) + "\x00"
	makeProc(41, "S", "100", "123", good)
	makeProc(42, "S", "101", "123", good)
	makeProc(43, "D", "100", "123", good)
	makeProc(44, "Z", "100", "123", good)
	makeProc(45, "S", "100", "123", "GFF_HAB="+hook.Addr("/old/socket", strings.Repeat("b", 32))+"\x00")
	for _, pid := range []int{41, 42, 43, 44, 45} {
		p, ok := inspectProc(root, pid, 100, token)
		if ok != (pid == 41) {
			t.Errorf("pid %d: %+v %v", pid, p, ok)
		}
	}
	ids, err := procPIDs(root)
	if err != nil || len(ids) != 5 {
		t.Fatalf("pids %v %v", ids, err)
	}
}

func TestSweepRevalidates(t *testing.T) {
	cases := []struct {
		name                       string
		changed, cleared, canceled bool
	}{
		{name: "match"}, {name: "reused PID", changed: true}, {name: "cleared environment", cleared: true}, {name: "canceled", canceled: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls, kills := 0, 0
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			n, err := sweep(ctx, "token", func() ([]int, error) { return []int{1, os.Getpid(), 42}, nil }, func(pid int, token string) (process, bool) {
				calls++
				p := process{pid: pid, born: "first"}
				if calls == 2 {
					if tc.changed {
						p.born = "second"
					}
					if tc.cleared {
						return p, false
					}
					if tc.canceled {
						cancel()
					}
				}
				return p, true
			}, func(pid int, sig unix.Signal) error {
				kills++
				if pid != 42 || sig != unix.SIGKILL {
					t.Errorf("signal %d %v", pid, sig)
				}
				return nil
			})
			want := 1
			if tc.changed || tc.cleared || tc.canceled {
				want = 0
			}
			if n != want || kills != want || err != nil {
				t.Fatalf("n %d kills %d err %v", n, kills, err)
			}
		})
	}
}
