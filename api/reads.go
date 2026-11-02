package api

// --- Read tools (capcom.md 4, 5) ---
//
// The read tools serve every agent through gff-hab's CLI and MCP server. They never write. A
// call with no host reads every host: this hab, and each peer over the link this hab dialed,
// under that peer's own read scope. A call that comes over a peer link is answered from the
// called hab alone (one hop).

// ReadHostState says how one host answered a read. The server sends it, so the set is open: a
// client shows an unknown word as given.
type ReadHostState string

// ReadHost is one host's part in a read that spans hosts.
type ReadHost struct {
	Label    string        `json:"label" doc:"This hab's label for the peer; this hab's own host id for itself."`
	DeviceID string        `json:"device_id,omitempty"`
	Own      bool          `json:"own,omitempty" doc:"This hab."`
	State    ReadHostState `json:"state" doc:"ok; partial: it ran out of time and its results may be missing some; offline; update_needed: it has no read tools; error."`
	Reason   string        `json:"reason,omitempty" doc:"Why the host is not ok, in a few words."`
	LeftOut  int           `json:"left_out,omitempty" doc:"Transcripts the host's read scope left out."`
}

type TranscriptListParams struct {
	Host    string `json:"host,omitempty" doc:"A peer label or device id, this hab's host id or device id, or self. Absent: every host."`
	Project string `json:"project,omitempty" doc:"A project name, or a folder: sessions in it or under it."`
	Harness string `json:"harness,omitempty" doc:"claude or codex."`
	Since   string `json:"since,omitempty" doc:"RFC 3339 time: sessions written to at or after it."`
	Until   string `json:"until,omitempty" doc:"RFC 3339 time: sessions started before it."`
	Limit   int    `json:"limit,omitempty" doc:"Most sessions to return. Default 50, at most 200."`
}

// TranscriptSessions answers transcript.list: past and current sessions, newest first.
type TranscriptSessions struct {
	Seq
	Sessions []TranscriptSession `json:"sessions" doc:"Newest written first."`
	Hosts    []ReadHost          `json:"hosts" doc:"Every host the read asked, with how it answered."`
	More     bool                `json:"more,omitempty" doc:"More sessions matched than were returned."`
}

// TranscriptSession is one harness session found in its native transcript.
type TranscriptSession struct {
	Host     string          `json:"host" doc:"As in ReadHost.label."`
	DeviceID string          `json:"device_id,omitempty"`
	Harness  string          `json:"harness"`
	ID       string          `json:"id" doc:"The harness's own session id: what transcript.read and the harness's resume take."`
	Folder   string          `json:"folder,omitempty" doc:"The working folder the transcript recorded."`
	Project  string          `json:"project,omitempty" doc:"The name of the catalog project that holds the folder."`
	Title    string          `json:"title,omitempty" doc:"The first words the user typed, shortened."`
	Started  string          `json:"started,omitempty" doc:"RFC 3339 time of the first record."`
	Updated  string          `json:"updated" doc:"RFC 3339 time the transcript was last written."`
	Live     *TranscriptLive `json:"live,omitempty" doc:"The session runs in a pane on its host now."`
}

// TranscriptLive is the Flightdeck session a transcript belongs to while its agent runs.
type TranscriptLive struct {
	Session string      `json:"session" doc:"The Flightdeck session id on its host."`
	Name    string      `json:"name"`
	State   AgentStatus `json:"state"`
}

type TranscriptSearchParams struct {
	Words   string `json:"words" doc:"Words that must all appear in a message, any case. Double quotes keep a phrase together."`
	Host    string `json:"host,omitempty" doc:"A peer label or device id, this hab's host id or device id, or self. Absent: every host."`
	Project string `json:"project,omitempty" doc:"A project name, or a folder."`
	Harness string `json:"harness,omitempty" doc:"claude or codex."`
	Since   string `json:"since,omitempty" doc:"RFC 3339 time: transcripts written to at or after it."`
	Limit   int    `json:"limit,omitempty" doc:"Most hits to return. Default 50, at most 50."`
}

// TranscriptHits answers transcript.search: hits newest first.
type TranscriptHits struct {
	Seq
	Hits  []TranscriptHit `json:"hits" doc:"Newest first, across every host."`
	Hosts []ReadHost      `json:"hosts" doc:"Every host the search asked, with how it answered."`
	More  bool            `json:"more,omitempty" doc:"More hits matched than were returned."`
}

// TranscriptHit is one message that matched a search.
type TranscriptHit struct {
	Host     string   `json:"host"`
	DeviceID string   `json:"device_id,omitempty"`
	Harness  string   `json:"harness"`
	Session  string   `json:"session" doc:"The harness's own session id."`
	Folder   string   `json:"folder,omitempty"`
	Project  string   `json:"project,omitempty"`
	Message  string   `json:"message" doc:"The message id, as chat.page and transcript.read give it."`
	Kind     ChatKind `json:"kind"`
	At       string   `json:"at"`
	Snippet  string   `json:"snippet" doc:"The text around the first match, at most 240 characters."`
}

type TranscriptReadParams struct {
	Host    string `json:"host,omitempty" doc:"A peer label or device id, this hab's host id or device id, or self. Absent: this hab."`
	Session string `json:"session" doc:"The harness's own session id, or a Flightdeck session id or name on that host."`
	Harness string `json:"harness,omitempty" doc:"claude or codex. Absent: found from the session."`
	Before  string `json:"before,omitempty" doc:"Message id: return the messages before it. Absent means the tail."`
	Limit   int    `json:"limit,omitempty" doc:"Most messages to return. Default 50, at most 200."`
}

// TranscriptPage is one page of a past or current session's messages, oldest first.
type TranscriptPage struct {
	Seq
	Host     string        `json:"host"`
	Harness  string        `json:"harness"`
	Session  string        `json:"session" doc:"The harness's own session id."`
	Folder   string        `json:"folder,omitempty"`
	Messages []ChatMessage `json:"messages"`
	More     bool          `json:"more" doc:"Older messages exist before the first one."`
}

type NoticeGetParams struct {
	ID   string `json:"id" doc:"The notice id, from notice.raise or a notice.raised event."`
	Host string `json:"host,omitempty" doc:"A peer label or device id, this hab's host id or device id, or self. Absent: this hab."`
}

// NoticeRecord answers notice.get: a notice this hab raised, and the session it is about.
type NoticeRecord struct {
	Seq
	Host    string   `json:"host"`
	Notice  Notice   `json:"notice"`
	Session *Session `json:"session,omitempty" doc:"The session the notice is about, while its record exists."`
}

type FileReadParams struct {
	Host   string `json:"host,omitempty" doc:"A peer label or device id, this hab's host id or device id, or self. Absent: this hab."`
	Path   string `json:"path" doc:"An absolute path, or one under ~/ on that host."`
	Offset int64  `json:"offset,omitempty" doc:"Byte offset to start at."`
	Limit  int    `json:"limit,omitempty" doc:"Most bytes to return. Default 65536, at most 262144."`
}

// FileData answers file.read: a run of a text file's bytes.
type FileData struct {
	Seq
	Host   string `json:"host"`
	Path   string `json:"path" doc:"The path as asked, cleaned."`
	Size   int64  `json:"size"`
	Offset int64  `json:"offset"`
	Text   string `json:"text,omitempty" doc:"The run, as UTF-8 text. Absent for a binary file."`
	Binary bool   `json:"binary,omitempty" doc:"The file is not UTF-8 text; no bytes are returned."`
	More   bool   `json:"more,omitempty" doc:"Bytes follow the run."`
}

// --- Logbook reads (logbook.md 10.3). They answer unavailable until Logbook (Part 12) exists;
// Part 12 owns these shapes and may add fields. ---

type LogbookFindParams struct {
	Words   string `json:"words,omitempty" doc:"Words to search for. Absent: list records."`
	Type    string `json:"type,omitempty"`
	Status  string `json:"status,omitempty"`
	Project string `json:"project,omitempty"`
	Tag     string `json:"tag,omitempty"`
	Host    string `json:"host,omitempty"`
	Author  string `json:"author,omitempty"`
	Since   string `json:"since,omitempty" doc:"RFC 3339 time."`
	Limit   int    `json:"limit,omitempty"`
}

// LogbookRecord is one Logbook record as a read returns it.
type LogbookRecord struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Status  string `json:"status,omitempty"`
	Title   string `json:"title"`
	Project string `json:"project,omitempty"`
	Body    string `json:"body,omitempty" doc:"Absent in a search result."`
	Updated string `json:"updated,omitempty" doc:"RFC 3339 time."`
}

// LogbookRecords answers logbook.find.
type LogbookRecords struct {
	Seq
	Records []LogbookRecord `json:"records"`
	More    bool            `json:"more,omitempty"`
}

type LogbookGetParams struct {
	ID string `json:"id"`
}

// LogbookRecordResult answers logbook.get.
type LogbookRecordResult struct {
	Seq
	Record LogbookRecord `json:"record"`
}
