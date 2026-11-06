package api

// --- The heavy-run lock and the disk watch (groups.md 8) ---

// HeavyState is where one heavy run stands: holding the lock, or waiting for it. The server
// sends it, so the set is open: a client shows an unknown word as given.
type HeavyState string

type HeavyTakeParams struct {
	Ticket    string `json:"ticket,omitempty" doc:"A ticket from an earlier heavy.take: this connection takes it over. After a gff-hab restart the hab no longer knows it; holding and since then give back its place."`
	Holding   bool   `json:"holding,omitempty" doc:"With a ticket the hab does not know: the run held the lock before the hab restarted, so it holds again at once."`
	Since     string `json:"since,omitempty" doc:"With a ticket the hab does not know: the RFC 3339 time it first asked, from the last answer, so it waits in its old place."`
	Granted   string `json:"granted,omitempty" doc:"With holding: the RFC 3339 time it was granted, from the last answer, so a long hold counts from then."`
	PaneToken string `json:"pane_token,omitempty" doc:"The token in the calling pane's GFF_HAB: the hab releases the lock when that pane closes, and names the pane's session in heavy.status."`
	Command   string `json:"command,omitempty" doc:"The command the run wraps, for heavy.status. At most 200 bytes are kept."`
	WaitMS    int    `json:"wait_ms,omitempty" doc:"Wait up to this long for the lock; 0 answers at once. At most 600000."`
}

// HeavyTaken answers heavy.take. The ticket is held for as long as the calling connection is
// open: the hab releases it when the connection drops.
type HeavyTaken struct {
	Seq
	Ticket  string     `json:"ticket"`
	State   HeavyState `json:"state" doc:"holding or waiting."`
	Ahead   int        `json:"ahead" doc:"Waiting: the runs before this one, holders included."`
	Since   string     `json:"since" doc:"RFC 3339 time the ticket first asked. Send it back after a restart."`
	Granted string     `json:"granted,omitempty" doc:"Holding: RFC 3339 time it was granted."`
	Reason  string     `json:"reason,omitempty" doc:"Waiting: why nothing is granted now, beyond the runs ahead: disk low, or the hold after a gff-hab start."`
}

type HeavyReleaseParams struct {
	Ticket string `json:"ticket"`
}

type HeavyStatusParams struct{}

// HeavyRun is one run that holds or waits for the lock.
type HeavyRun struct {
	Ticket  string     `json:"ticket"`
	State   HeavyState `json:"state"`
	Command string     `json:"command,omitempty"`
	Session string     `json:"session,omitempty" doc:"The session of the pane the run asked from, when it named one."`
	Pane    string     `json:"pane,omitempty"`
	Since   string     `json:"since" doc:"RFC 3339 time it first asked."`
	Granted string     `json:"granted,omitempty" doc:"RFC 3339 time it was granted."`
}

// HeavyVolume is the free space of one volume the disk watch reads.
type HeavyVolume struct {
	Path       string `json:"path" doc:"The folder whose volume this is: the hab's state folder or the worktree root."`
	FreeBytes  uint64 `json:"free_bytes" doc:"What an ordinary user may still write."`
	TotalBytes uint64 `json:"total_bytes"`
}

// HeavyStatus answers heavy.status: who holds the lock, who waits, and the disk state.
type HeavyStatus struct {
	Seq
	Slots        int           `json:"slots" doc:"heavy.slots: runs that may hold the lock at once."`
	Holders      []HeavyRun    `json:"holders" doc:"In the order they were granted."`
	Waiters      []HeavyRun    `json:"waiters" doc:"In the order they will be granted."`
	DiskLow      bool          `json:"disk_low,omitempty" doc:"Free space fell below heavy.disk_min_free and has not come back above it by [heavy] disk_resume_margin: nothing is granted."`
	MinFreeBytes uint64        `json:"min_free_bytes" doc:"heavy.disk_min_free in bytes."`
	Volumes      []HeavyVolume `json:"volumes"`
	HoldUntil    string        `json:"hold_until,omitempty" doc:"RFC 3339 time the hold after a gff-hab start ends; absent once it ended."`
}

// CatalogHeavy is the heavy-run lock and disk watch of the publishing host, as peers see it
// (groups.md 8).
type CatalogHeavy struct {
	Slots   int           `json:"slots"`
	Holding int           `json:"holding" doc:"Runs that hold the lock."`
	Waiting int           `json:"waiting" doc:"Runs that wait for it."`
	DiskLow bool          `json:"disk_low,omitempty" doc:"Nothing is granted for lack of free space."`
	Volumes []HeavyVolume `json:"volumes" doc:"The state folder's volume and the worktree root's. free_bytes is published when it moved by 1 GiB or more, or crossed the threshold."`
}
