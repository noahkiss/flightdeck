package vt_test

import (
	"testing"

	"github.com/noahkiss/flightdeck/internal/vt"
	"github.com/noahkiss/flightdeck/internal/vt/vttest"
)

func TestFake(t *testing.T) {
	vttest.Run(t, vt.Fake{}, "fixture.golden")
}
