package diagbundle

import "github.com/noahkiss/flightdeck/internal/settings"

// schemaKey reports whether a config key is one the schema owns, which a bundle keeps as written:
// a setting's id, or a key of the config structs and the tables read by key (8.3). Any other key
// is one the user chose.
func schemaKey(k string) bool {
	if _, ok := settings.Lookup(k); ok {
		return true
	}
	return configKeys[k]
}

// configKeys are the keys of hab.toml and peers.toml that the code names: the toml tags of the
// config structs (TestConfigKeys checks that each is here) and the keys of the tables read as
// maps ([capcom], [rounds], [groups], [heavy]).
var configKeys = setOf(
	// toml tags
	"actions", "aliases", "allow", "always_on", "always_on_alert", "args", "argv", "at", "auth", "backup",
	"base_url", "by", "capabilities", "capcom", "catalog", "catch_up", "change", "command", "cooldown", "cron",
	"cwd", "debounce", "deny", "device_id", "diagnostics", "enabled", "endpoints", "env", "every", "expires_at",
	"export", "field", "filter", "git", "group", "groups", "habits_off", "harness", "harness_args", "harnesses",
	"headers", "health", "heavy", "helper", "hosts", "index", "instructions", "instructions_note", "keep",
	"key_command", "kinds", "label", "level", "log_level", "logbook", "manual", "max_running", "max_runs", "mcp",
	"mode", "model", "model_url", "name", "notify", "on", "on_finish", "order", "path", "paused", "peers",
	"project", "project_remote", "project_roots", "prompt", "provider", "read", "rounds", "scope", "secrets",
	"server", "services", "session", "settings", "source", "speech", "state", "status", "summaries", "tags",
	"tallies", "target", "text", "thinking", "threads", "timeout", "transport", "type", "url", "vectors", "wait",
	"wake_settle", "watch", "write", "zone",
	// [capcom], read by key
	"api_key", "api_key_command", "arg", "cache", "command_timeout", "crash_limit", "crash_window", "daily_usd",
	"engine", "google_key", "google_key_command", "key", "key_id", "location_stale", "maps", "max_turn_usd",
	"max_turns", "oauth_token", "oauth_token_command", "pace_daily_share", "pace_event_stop", "pace_small_at",
	"pace_wait_at", "places", "precompact_timeout", "program", "reseed_at", "reseed_early_at", "reseed_quiet",
	"reseed_quiet_at", "restart_delay", "route_backoff_max", "seed_logbook_bytes", "seed_messages",
	"seed_messages_chars", "service_id", "small_model", "summary_input_chars", "summary_max_chars",
	"summary_quiet", "team_id", "token_retry", "typing_timeout", "wake_retry", "weather",
	// [rounds], read by key
	"batch_max_bytes", "batch_max_events", "block_data_max", "block_max_bytes", "block_max_events",
	"event_files_keep", "max_watchers", "outcome_wait", "state_save_every", "vars", "watch_backoff_max",
	"watch_backoff_reset", "watch_buffer_bytes", "watch_buffer_lines", "watch_pause_after", "watch_quick_exit",
	// [groups] and [heavy], read by key
	"brief_findings", "fix_output_lines", "handoff_note_wait", "inbox_days", "inbox_max", "keep_ended",
	"lane_tries", "max_findings", "reviewer_idle", "sibling_wait", "trust_hold", "disk_resume_margin",
	"restart_hold",
)

func setOf(ks ...string) map[string]bool {
	m := make(map[string]bool, len(ks))
	for _, k := range ks {
		m[k] = true
	}
	return m
}
