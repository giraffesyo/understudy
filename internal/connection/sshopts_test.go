package connection

import (
	"reflect"
	"testing"
)

func TestParseSSHArgs(t *testing.T) {
	o, err := parseSSHArgs(`-C -o ControlMaster=auto -o ControlPersist=60s -o ProxyJump=admin@bastion:2222,inner`,
		`-o 'ProxyCommand=ssh -W %h:%p gw' -oStrictHostKeyChecking=no -i ~/.ssh/k -p 2200 -o SendEnv=LANG`)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(o.ProxyJump, []string{"admin@bastion:2222", "inner"}) {
		t.Errorf("ProxyJump = %v", o.ProxyJump)
	}
	if o.ProxyCommand != "ssh -W %h:%p gw" || o.Port != 2200 || o.StrictHostKeys == nil || *o.StrictHostKeys {
		t.Errorf("parsed %+v", o)
	}
	if len(o.IdentityFiles) != 1 || !reflect.DeepEqual(o.Ignored, []string{"SendEnv=LANG"}) {
		t.Errorf("identity/ignored: %v %v", o.IdentityFiles, o.Ignored)
	}
	user, addr := splitHop("admin@bastion:2222", "me")
	if user != "admin" || addr != "bastion:2222" {
		t.Errorf("splitHop = %s %s", user, addr)
	}
	if _, addr := splitHop("inner", "me"); addr != "inner:22" {
		t.Errorf("default port: %s", addr)
	}
}
