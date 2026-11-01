package speech

import "github.com/noahkiss/flightdeck/internal/speech/speechtest"

// The test binary is also the fake speech helper (speechtest): tests point Config.Helper at it
// and set speechtest.Env with t.Setenv, so only the child processes see it.
func init() { speechtest.RunIfFake() }
