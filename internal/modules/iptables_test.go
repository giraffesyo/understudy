package modules

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// testdata/iptables_rules.json holds [params, construct_rule(params),
// push_arguments('/sbin/iptables', '-I', params+rule_num=3,wait=5)]
// triples produced by ansible-core 2.21's iptables module.
func TestIptablesConstructRule(t *testing.T) {
	data, err := os.ReadFile("testdata/iptables_rules.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases [][3]json.RawMessage
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		var raw map[string]any
		var wantRule, wantPush []string
		json.Unmarshal(c[0], &raw)
		json.Unmarshal(c[1], &wantRule)
		json.Unmarshal(c[2], &wantPush)
		p, err := iptablesSpec.Parse(raw)
		if err != nil {
			t.Fatalf("%s: %v", c[0], err)
		}
		ip := newIptablesParams(p)
		if got := ip.construct(); !reflect.DeepEqual(got, wantRule) {
			t.Errorf("%s\n got  %q\n want %q", c[0], got, wantRule)
		}
		n, w := "3", "5"
		ip.str["rule_num"], ip.str["wait"] = &n, &w
		if got := ip.push("/sbin/iptables", "-I", true); !reflect.DeepEqual(got, wantPush) {
			t.Errorf("push %s\n got  %q\n want %q", c[0], got, wantPush)
		}
	}
}

func TestIptablesValidate(t *testing.T) {
	for _, c := range []struct {
		raw  map[string]any
		want string
	}{
		{map[string]any{"chain": "INPUT", "set_dscp_mark": "1"}, "missing parameter(s) required by 'set_dscp_mark': jump"},
		{map[string]any{"chain": "INPUT", "jump": "TEE"}, "jump is TEE but all of the following are missing: gateway"},
		{map[string]any{"jump": "ACCEPT"}, "flush is False but all of the following are missing: chain"},
		{map[string]any{"chain": "INPUT", "flush": true, "policy": "DROP"}, "parameters are mutually exclusive: flush|policy"},
	} {
		p, err := iptablesSpec.Parse(c.raw)
		if err != nil {
			t.Fatal(err)
		}
		if err := iptablesValidate(c.raw, p); err == nil || err.Error() != c.want {
			t.Errorf("%v: got %v, want %q", c.raw, err, c.want)
		}
	}
}
