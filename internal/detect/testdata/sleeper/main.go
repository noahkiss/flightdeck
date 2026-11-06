// Command sleeper sleeps for the seconds its first argument names. Detection tests copy it
// under the name of an agent, such as claude: unlike a system sleep, which may be one binary
// that picks its program from argv[0], it runs under any name.
package main

import (
	"os"
	"strconv"
	"time"
)

func main() {
	if len(os.Args) != 2 {
		os.Exit(2)
	}
	n, err := strconv.Atoi(os.Args[1])
	if err != nil {
		os.Exit(2)
	}
	time.Sleep(time.Duration(n) * time.Second)
}
