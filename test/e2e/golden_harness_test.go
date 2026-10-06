//go:build golden

package e2e

import (
	"reflect"
	"testing"
)

func TestGoldenTaskOrder(t *testing.T) {
	const first = "TASK [include] ***\nok: [h1]\nok: [h2]\n"
	const a = "TASK [debug] ***\nok: [h1]\n"
	const b = "TASK [set_fact] ***\nok: [h2]\n"
	const last = "RUNNING HANDLER [finish] ***\nok: [h1]\nok: [h2]\n"
	wantNames := []string{"debug", "finish", "include", "set_fact"}
	wantHosts := map[string][]string{
		"h1": {"include", "debug", "finish"},
		"h2": {"include", "set_fact", "finish"},
	}
	for _, output := range []string{first + a + b + last, first + b + a + last} {
		names, hosts := taskOrder(output)
		if !reflect.DeepEqual(names, wantNames) || !reflect.DeepEqual(hosts, wantHosts) {
			t.Fatalf("taskOrder = %v, %v; want %v, %v", names, hosts, wantNames, wantHosts)
		}
	}

	t.Run("order within a host", func(t *testing.T) {
		_, hosts := taskOrder(first + last + a + b)
		want := []string{"include", "finish", "debug"}
		if !reflect.DeepEqual(hosts["h1"], want) {
			t.Fatalf("h1 order = %v; want %v", hosts["h1"], want)
		}
	})
	t.Run("repeated and silent banners", func(t *testing.T) {
		names, hosts := taskOrder(a + a + "TASK [meta] ***\n")
		if !reflect.DeepEqual(names, []string{"debug", "debug", "meta"}) ||
			!reflect.DeepEqual(hosts["h1"], []string{"debug", "debug"}) {
			t.Fatalf("taskOrder lost a banner or result: %v, %v", names, hosts)
		}
	})
}
