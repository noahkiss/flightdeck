package api

// --- Hab-side settings (app.md 20.5) ---

// SettingSource says where a setting's value comes from. The server sends it, so the set is
// open: a client reads an unknown word as config, read-only.
type SettingSource string

// Setting is one hab-side setting: an entry of settings.get, settings.set and the host
// snapshot's settings array.
type Setting struct {
	ID       string        `json:"id" doc:"The setting id, such as confirm.start_elsewhere (app.md 20.3)."`
	Value    RawJSON       `json:"value" doc:"The value the hab enforces now."`
	Default  RawJSON       `json:"default" doc:"The hab's built-in value: the Balanced column (app.md 20.1)."`
	Revision uint64        `json:"revision" doc:"Raised by one on each change; starts at 1. Compared only within an epoch (spec 3.5)."`
	Source   SettingSource `json:"source" doc:"default: no client wrote it and no config sets it. client: a client wrote it. config: the hab's config file, a flag or the environment sets it, and writes are refused with locked."`
}

type SettingsGetParams struct {
	IDs []string `json:"ids,omitempty" doc:"The settings to read. Absent: every hab-side setting. An id this hab does not know is left out of the result."`
}

// SettingsResult is the result of settings.get and settings.set.
type SettingsResult struct {
	Seq
	Settings []Setting `json:"settings"`
}

// SettingWrite is one row of a settings.set call.
type SettingWrite struct {
	ID       string  `json:"id"`
	Value    RawJSON `json:"value"`
	Revision uint64  `json:"revision" doc:"The setting's revision the client last saw. Any other revision refuses the call with conflict."`
}

type SettingsSetParams struct {
	Values []SettingWrite `json:"values" doc:"Rows to write, all or nothing. Any refused row refuses the call; error.data.id names the first refused row."`
}

// SettingUpdated is the data of a settings.updated event. The event's id is the setting id and
// its revision the setting's new revision.
type SettingUpdated struct {
	Value  RawJSON       `json:"value"`
	Source SettingSource `json:"source"`
}
