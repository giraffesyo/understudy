package vars

import (
	"testing"

	"github.com/giraffesyo/understudy/internal/template"
)

func TestExtraVarsBeatTaskVars(t *testing.T) {
	s := NewStore(template.New())
	s.SetExtraVars(map[string]any{"url": "from-extra"})
	c := s.NewContext("h", template.Position{}).WithOverlay(map[string]any{"url": "from-task"})
	if v, _ := c.Get("url"); v != "from-extra" {
		t.Fatalf("url = %v, want extra var to win over task vars", v)
	}
}
