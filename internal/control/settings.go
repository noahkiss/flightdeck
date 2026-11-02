package control

import (
	"github.com/noahkiss/flightdeck/api"
)

// Settings serves the hab-side settings (app.md 20.5). gff-hab implements it with
// internal/settings. A Set error that is an *Error keeps its kind and its data.id.
type Settings interface {
	Get(p api.SettingsGetParams) api.SettingsResult
	Set(p api.SettingsSetParams) (api.SettingsResult, error)
}

const hintNoSettings = "%s needs the hab-side settings, which this hab is running without; the settings capability is absent"

// dispatchSettings answers settings.get and settings.set.
func (c *conn) dispatchSettings(m message, s api.Seq) (any, *Error) {
	ss := c.s.cfg.Settings
	if m.Method == "settings.get" {
		var p api.SettingsGetParams
		if e := c.decodeUp(m, &p); e != nil {
			return nil, e
		}
		if ss == nil {
			return nil, appErr(api.ErrorKindUnavailable, hintNoSettings, m.Method)
		}
		res := ss.Get(p)
		res.Seq = s
		return api.NonNil(res), nil
	}
	var p api.SettingsSetParams
	if e := c.decodeUp(m, &p); e != nil {
		return nil, e
	}
	if ss == nil {
		return nil, appErr(api.ErrorKindUnavailable, hintNoSettings, m.Method)
	}
	res, err := ss.Set(p)
	if err != nil {
		return nil, actionErr(m.Method, err)
	}
	res.Seq = s
	return api.NonNil(res), nil
}
