package yaml

import "github.com/giraffesyo/understudy/internal/omap"

// OMap is the insertion-ordered map YAML mappings decode to.
type OMap = omap.OMap

// NewOMap returns an empty ordered map.
func NewOMap() *OMap { return omap.NewOMap() }

// AsMap recursively converts OMaps to plain maps (see omap.AsMap).
func AsMap(v any) any { return omap.AsMap(v) }

// PlainMap flattens a top-level OMap to a plain map (see omap.PlainMap).
func PlainMap(v any) (map[string]any, bool) { return omap.PlainMap(v) }
