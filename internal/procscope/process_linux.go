package procscope

import "os"

func listProcesses() ([]int, error) { return procPIDs("/proc") }
func inspectProcess(pid int, token string) (process, bool) {
	return inspectProc("/proc", pid, os.Getuid(), token)
}
