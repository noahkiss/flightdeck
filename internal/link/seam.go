package link

// spawnedSeam, when set, runs in Spawn after the holder answered the spawn and before the link
// follows the new pane: the program already runs there. Only a build with the testseam tag sets
// it (seam_testseam.go); a normal build leaves it nil.
var spawnedSeam func()
