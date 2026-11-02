package api

// --- Logbook sync over the peer link (logbook.md 7) ---

// LogbookSeq is one entry of a Logbook version vector: per origin, the highest seq a replica
// holds with no gap below it.
type LogbookSeq struct {
	Origin string `json:"origin" doc:"The origin hab's device id."`
	Seq    uint64 `json:"seq"`
}

// LogbookPullParams asks for the ops the caller lacks (logbook.pull).
type LogbookPullParams struct {
	Have []LogbookSeq `json:"have" doc:"The caller's version vector. An origin it does not list counts as 0."`
	Skip []string     `json:"skip,omitempty" doc:"Origins the caller refuses (two hosts share them, logbook.md 7.4); none of their ops are sent."`
}

// LogbookPullResult answers logbook.pull with one page of ops.
type LogbookPullResult struct {
	Seq
	Ops    [][]byte     `json:"ops" doc:"Op bytes as stored, per origin in seq order: at most 2,000 ops or 4 MiB, or one larger op."`
	Vector []LogbookSeq `json:"vector" doc:"The answering hab's version vector."`
	More   bool         `json:"more,omitempty" doc:"More ops follow: pull again with the new vector."`
	Skip   []string     `json:"skip,omitempty" doc:"Origins the answering hab refuses; send it none of their ops."`
}

// LogbookPushParams hands over ops the callee lacks (logbook.push).
type LogbookPushParams struct {
	Ops [][]byte `json:"ops" doc:"Op bytes as stored, per origin in seq order: at most 2,000 ops or 4 MiB, or one larger op."`
}

// LogbookPushResult answers logbook.push with the callee's vector after it applied the ops.
type LogbookPushResult struct {
	Seq
	Vector []LogbookSeq `json:"vector"`
	Skip   []string     `json:"skip,omitempty" doc:"Origins the answering hab refuses; send it none of their ops."`
}
