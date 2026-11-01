package link

// spawnedSeam, when set, runs in Spawn after the holder answered the spawn and before the link
// follows the new pane: the program already runs there. Only a build with the testseam tag sets
// it (seam_testseam.go); a normal build leaves it nil.
var spawnedSeam func()

// attachedSeam, when set, runs on the event loop after the link attached a new holder's panes
// and before it counts the holder as attached (complete). Only link tests set it.
var attachedSeam func(instance string)
