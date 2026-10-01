package inventory

import (
	"reflect"
	"sync"
	"testing"

	"github.com/giraffesyo/understudy/internal/vars"
)

func groupList(inv *Inventory, name string) []string {
	return inv.groupHostNames(inv.Groups[name])
}

func TestAddDynamicHost(t *testing.T) {
	inv, err := Load([]string{"h1,h2"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	changed, affected, err := inv.AddDynamicHost("n1", []string{"color"}, map[string]any{"color": "blue"}, []string{"dyn"})
	if err != nil || !changed || !reflect.DeepEqual(affected, []string{"n1"}) {
		t.Fatalf("first add: changed=%v affected=%v err=%v", changed, affected, err)
	}
	if got := groupList(inv, "all"); !reflect.DeepEqual(got, []string{"n1", "h1", "h2"}) {
		t.Errorf("all = %v (a new host is the all group's own)", got)
	}
	if got := groupList(inv, "dyn"); !reflect.DeepEqual(got, []string{"n1"}) {
		t.Errorf("dyn = %v", got)
	}
	if v := inv.EffectiveVars(inv.Host("n1"))["color"]; v != (vars.Final{V: "blue"}) {
		t.Errorf("color = %#v, want a final value", v)
	}
	if changed, _, _ := inv.AddDynamicHost("n1", []string{"color"}, map[string]any{"color": "blue"}, []string{"dyn"}); changed {
		t.Error("the same add again reported a change")
	}
	if changed, _, _ := inv.AddDynamicHost("n1", []string{"color"}, map[string]any{"color": "green"}, nil); !changed {
		t.Error("a changed variable reported no change")
	}
	// A host in no group is ungrouped.
	inv.AddDynamicHost("lone", nil, nil, nil)
	if got := groupList(inv, "ungrouped"); !reflect.DeepEqual(got, []string{"h1", "h2", "lone"}) {
		t.Errorf("ungrouped = %v", got)
	}
}

func TestAddDynamicGroup(t *testing.T) {
	inv, err := Load([]string{"h1,h2"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	changed, _, err := inv.AddDynamicGroup("h1", "g", []string{"p"})
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if got := inv.GroupNames(inv.Host("h1")); !reflect.DeepEqual(got, []string{"g", "p"}) {
		t.Errorf("group_names = %v (ancestors included, ungrouped left)", got)
	}
	if changed, _, _ := inv.AddDynamicGroup("h1", "g", []string{"p"}); changed {
		t.Error("the same group_by again reported a change")
	}
	if _, _, err := inv.AddDynamicGroup("h1", "p", []string{"p"}); err == nil || err.Error() != "can't add group to itself" {
		t.Errorf("err = %v", err)
	}
	if _, _, err := inv.AddDynamicGroup("nope", "g", nil); err == nil || err.Error() != "nope cannot be matched in inventory" {
		t.Errorf("err = %v", err)
	}
}

func TestReplayDynamic(t *testing.T) {
	old, _ := Load([]string{"h1,h2"}, nil)
	old.AddDynamicHost("n1", nil, nil, []string{"dyn"})
	old.AddDynamicGroup("h2", "g", []string{"all"})
	fresh, _ := Load([]string{"h1"}, nil)
	if err := fresh.ReplayDynamic(old); err != nil {
		t.Fatal(err)
	}
	if got := groupList(fresh, "dyn"); !reflect.DeepEqual(got, []string{"n1"}) {
		t.Errorf("dyn = %v", got)
	}
	// h2 left the inventory: its group_by is skipped, the group not made.
	if _, ok := fresh.Groups["g"]; ok {
		t.Error("group g made for a host the refresh dropped")
	}
}

func TestDynamicConcurrent(t *testing.T) {
	inv, _ := Load([]string{"h1,h2,h3,h4"}, nil)
	var wg sync.WaitGroup
	for _, h := range []string{"h1", "h2", "h3", "h4"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				inv.AddDynamicGroup(h, "g_"+h, []string{"parent"})
				inv.GroupsMap()
				inv.Match("parent")
				inv.EffectiveVars(inv.Host(h))
			}
		}()
	}
	wg.Wait()
	if got := len(groupList(inv, "parent")); got != 4 {
		t.Errorf("parent has %d hosts", got)
	}
}
