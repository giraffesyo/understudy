package modules

import (
	"os"
	"testing"
)

func TestPGPIdentifyKeys(t *testing.T) {
	data, err := os.ReadFile("testdata/docker.asc")
	if err != nil {
		t.Fatal(err)
	}
	if !isPGPPubkey(data) {
		t.Fatal("not detected as pubkey")
	}
	keys, err := pgpIdentifyKeys(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0].Fingerprint != "060A61C51B558A7F742B77AAC52FEB6B621E9F35" || keys[0].KeyID != "C52FEB6B621E9F35" {
		t.Fatalf("got %+v", keys)
	}
	bin, err := pgpDearmor(data)
	if err != nil {
		t.Fatal(err)
	}
	keys2, err := pgpIdentifyKeys(bin)
	if err != nil || len(keys2) != 1 || keys2[0] != keys[0] {
		t.Fatalf("binary: %+v %v", keys2, err)
	}
}
