//go:build testseam

package main

import "os"

func init() {
	groupSkipCommitSeam = os.Getenv("GFF_TEST_GROUP_SKIP_COMMIT") == "1"
}
