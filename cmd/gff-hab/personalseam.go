package main

// The personal tools' service bases (capcom-service.md 8.3, 8.4). Empty is the real service.
// Only a build with the testseam tag sets them (personalseam_testseam.go), from
// $GFF_TEST_WEATHERKIT, $GFF_TEST_APPLE_MAPS and $GFF_TEST_GOOGLE_PLACES, so end-to-end tests
// talk to a fake server and never to Apple or Google.
var (
	weatherBaseSeam string
	mapsBaseSeam    string
	googleBaseSeam  string
)
