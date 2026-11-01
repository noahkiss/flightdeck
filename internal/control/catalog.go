package control

import (
	"github.com/noahkiss/flightdeck/api"
)

// EventCatalogUpdated is the event type of a changed catalog: this hab's own or a peer's copy.
// Its id is the catalog's device id.
const EventCatalogUpdated api.EventType = "catalog.updated"

// Catalog serves this hab's catalog and the copies it holds of its peers' catalogs (mesh.md 5).
// gff-hab implements it with internal/catalog. An error that is an *Error keeps its kind.
type Catalog interface {
	// Get returns this hab's own catalog when its generation differs from since.
	Get(since uint64) api.CatalogGetResult
	// Put stores the catalog a peer link handed over. from is the link's hello peer field.
	Put(from api.PeerHello, c api.Catalog) error
	// List returns the merged view.
	List() api.CatalogList
}

const hintNoCatalog = "%s needs the catalog, which this hab is running without; the catalog capability is absent"

// dispatchCatalog answers catalog.get, catalog.put and catalog.list.
func (c *conn) dispatchCatalog(m message, s api.Seq) (any, *Error) {
	cs := c.s.cfg.Catalog
	switch m.Method {
	case "catalog.get":
		var p api.CatalogGetParams
		if e := c.decodeUp(m, &p); e != nil {
			return nil, e
		}
		if cs == nil {
			return nil, appErr(api.ErrorKindUnavailable, hintNoCatalog, m.Method)
		}
		res := cs.Get(p.Since)
		res.Seq = s
		return api.NonNil(res), nil
	case "catalog.put":
		var p api.CatalogPutParams
		if e := c.decodeUp(m, &p); e != nil {
			return nil, e
		}
		if cs == nil {
			return nil, appErr(api.ErrorKindUnavailable, hintNoCatalog, m.Method)
		}
		// Only a peer link hands over a catalog, and only its own hab's (mesh.md 5.2: one hop).
		if c.peer == nil {
			return nil, appErr(api.ErrorKindInvalid, "catalog.put needs a peer link: client.kind hab with the hello's peer field")
		}
		if p.Catalog.DeviceID != c.peer.DeviceID {
			return nil, appErr(api.ErrorKindInvalid, "catalog.put: the catalog is device %s, the peer link is %s", p.Catalog.DeviceID, c.peer.DeviceID)
		}
		if err := cs.Put(*c.peer, p.Catalog); err != nil {
			return nil, actionErr(m.Method, err)
		}
		return api.Empty{Seq: s}, nil
	default: // catalog.list
		var p api.CatalogListParams
		if e := c.decodeUp(m, &p); e != nil {
			return nil, e
		}
		if cs == nil {
			return nil, appErr(api.ErrorKindUnavailable, hintNoCatalog, m.Method)
		}
		res := cs.List()
		res.Seq = s
		return api.NonNil(res), nil
	}
}
