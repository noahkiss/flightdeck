package main

// diagStartSeam runs once gff-hab has logged its start line. Only a build with the testseam tag
// sets it (diagseam_testseam.go): $GFF_TEST_HAB_PANIC makes gff-hab panic there, for the crash
// record test.
var diagStartSeam = func() {}
