package content

import (
	"os"
	"testing"

	"github.com/noahkiss/flightdeck/internal/testgit"
)

// TestMain keeps the user's global and system git config out of every git these tests run.
func TestMain(m *testing.M) {
	testgit.Hermetic()
	os.Exit(m.Run())
}
