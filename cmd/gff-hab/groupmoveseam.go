package main

// groupSkipCommitSeam, when set, makes the old home of a move write its tombstone and send no
// group.adopt_commit, as if it crashed right after the commit point (groups.md 5.7 step 6). Only
// a build with the testseam tag sets it (groupmoveseam_testseam.go), from
// $GFF_TEST_GROUP_SKIP_COMMIT.
var groupSkipCommitSeam bool
