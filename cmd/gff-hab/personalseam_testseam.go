//go:build testseam

package main

import "os"

func init() {
	weatherBaseSeam = os.Getenv("GFF_TEST_WEATHERKIT")
	mapsBaseSeam = os.Getenv("GFF_TEST_APPLE_MAPS")
	googleBaseSeam = os.Getenv("GFF_TEST_GOOGLE_PLACES")
}
