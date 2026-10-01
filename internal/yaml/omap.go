package yaml

import "github.com/giraffesyo/understudy/internal/omap"

// OMap is the insertion-ordered map YAML mappings decode to.
type OMap = omap.OMap

// NewOMap returns an empty ordered map.
func NewOMap() *OMap { return omap.NewOMap() }

// AsMap recursively converts OMaps to plain maps (see omap.AsMap).
func AsMap(v any) any { return omap.AsMap(v) }

// PlainMap flattens a top-level OMap to a plain map (see omap.PlainMap);
// the map keeps a decoded mapping's origins.
func PlainMap(v any) (map[string]any, bool) {
	m, ok := omap.PlainMap(v)
	if om, isOMap := v.(*OMap); ok && isOMap {
		if co, found := lookupContainer(om); found {
			if k, ok := containerKey(m); ok {
				containerOrigins.Store(k, &containerOrigin{keep: m, self: co.self, children: co.children})
			}
		}
	}
	return m, ok
}
