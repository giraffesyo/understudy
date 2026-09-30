package modules

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// Expectations come from running the Python modules' helpers
// (community.general.parted, ansible.posix.mount, community.general.modprobe).

func TestPartedParseInfo(t *testing.T) {
	out := "BYT;\n/dev/sdb:286061MB:scsi:512:512:msdos:ATA TOSHIBA THNSFJ25:;\n" +
		"1:1.05MB:106MB:105MB:fat32::esp, boot;\n2:106MB:368MB:262MB:ext2::;\n"
	pt := &parted{unit: "MB"}
	generic, parts, fail := pt.parsePartitionInfo(out)
	if fail != nil {
		t.Fatal(fail.Msg)
	}
	got, _ := json.Marshal(map[string]any{"generic": generic, "partitions": parts})
	want := `{"generic":{"dev":"/dev/sdb","logical_block":512,"model":"ATA TOSHIBA THNSFJ25","physical_block":512,"size":286061,"table":"msdos","unit":"mb"},` +
		`"partitions":[{"begin":1.05,"end":106,"flags":["esp","boot"],"fstype":"fat32","name":"","num":1,"size":105,"unit":"mb"},` +
		`{"begin":106,"end":368,"flags":[],"fstype":"ext2","name":"","num":2,"size":262,"unit":"mb"}]}`
	if string(got) != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestPartedSizes(t *testing.T) {
	for _, c := range []struct {
		n    int64
		unit string
		size float64
		out  string
	}{
		{1234567890, "KiB", 1234567890, "kib"},
		{5368709120, "", 5368, "MB"},
		{999, "MB", 999, "mb"},
	} {
		s, u := formatDiskSize(c.n, c.unit)
		if s != c.size || u != c.out {
			t.Errorf("formatDiskSize(%d, %q) = %v %q", c.n, c.unit, s, u)
		}
	}
	if got := convertToBytes(106, "MB"); got != 106000000 {
		t.Errorf("106MB = %d", got)
	}
	if got := convertToBytes(2.5, "GiB"); got != 2684354560 {
		t.Errorf("2.5GiB = %d", got)
	}
	if !checkSizeFormat("10%") || !checkSizeFormat("1GiB") || checkSizeFormat("1gib") {
		t.Error("checkSizeFormat")
	}
}

func TestMountFstabEdits(t *testing.T) {
	dir := t.TempDir()
	fstab := filepath.Join(dir, "fstab")
	os.WriteFile(fstab, []byte("# comment\n/dev/sda1 / ext4 defaults 0 1\n/dev/sdb1 /data xfs noatime\n"), 0o644)
	a := mountArgs{"name": "/data", "src": "/dev/sdb1", "fstype": "xfs", "opts": "noatime", "dump": "0", "passno": "0", "fstab": fstab}
	// A 4-field line's missing dump/passno are ints in the module, never
	// equal to the string arguments: the line is rewritten once.
	if _, changed, err := setMount(a, false, false); err != nil || !changed {
		t.Fatalf("4-field entry: changed=%v err=%v", changed, err)
	}
	if _, changed, err := setMount(a, false, false); err != nil || changed {
		t.Fatalf("unchanged entry reported changed=%v err=%v", changed, err)
	}
	a["opts"] = "defaults,noauto"
	if _, changed, _ := setMount(a, false, true); !changed {
		t.Fatal("opts change not detected")
	}
	if a.s("backup_file") == "" {
		t.Error("backup requested but no backup_file")
	}
	b := mountArgs{"name": "/mnt/my dir", "src": "srv:/x", "fstype": "nfs", "opts": "defaults", "dump": "0", "passno": "0", "fstab": fstab}
	setMount(b, false, false)
	data, _ := os.ReadFile(fstab)
	want := "# comment\n/dev/sda1 / ext4 defaults 0 1\n/dev/sdb1 /data xfs defaults,noauto 0 0\nsrv:/x /mnt/my\\040dir nfs defaults 0 0\n"
	if string(data) != want {
		t.Fatalf("fstab:\n%s\nwant:\n%s", data, want)
	}
	if changed, _ := unsetMount(b, false, false); !changed {
		t.Fatal("unset not changed")
	}
	data, _ = os.ReadFile(fstab)
	if strings.Contains(string(data), "my\\040dir") {
		t.Fatalf("entry not removed:\n%s", data)
	}
}

func TestRemoveNoLogValues(t *testing.T) {
	if got := removeNoLogValues("secret", []string{"secret"}); got != "VALUE_SPECIFIED_IN_NO_LOG_PARAMETER" {
		t.Error(got)
	}
	if got := removeNoLogValues("secret,noauto", []string{"secret"}); got != "********,noauto" {
		t.Error(got)
	}
}

func TestModprobePersistence(t *testing.T) {
	dir := t.TempDir()
	oldLoad, oldParams := modulesLoadLocation, parametersFilesLocation
	modulesLoadLocation, parametersFilesLocation = filepath.Join(dir, "load"), filepath.Join(dir, "opts")
	defer func() { modulesLoadLocation, parametersFilesLocation = oldLoad, oldParams }()
	os.MkdirAll(modulesLoadLocation, 0o755)
	os.MkdirAll(parametersFilesLocation, 0o755)
	os.WriteFile(filepath.Join(modulesLoadLocation, "x.conf"), []byte("dummy # comment\nother\n"), 0o644)
	os.WriteFile(filepath.Join(parametersFilesLocation, "x.conf"), []byte("options dummy numdummies=2\n"), 0o644)
	m := &modprobe{name: "dummy", params: "numdummies=2"}
	m.reModule = regexp.MustCompile(`^ *dummy *(?:[#;].*)?\n?\z`)
	m.reParams = regexp.MustCompile(`^options dummy \w+=\S+ *(?:[#;].*)?\n?\z`)
	m.reParamVal = regexp.MustCompile(`^options dummy (\w+=\S+) *(?:[#;].*)?\n?\z`)
	if !m.loadedPersistently() || !m.paramsIsSet() {
		t.Fatal("persistent state not detected")
	}
	m.commentOut(m.modulesFiles(), m.reModule, false)
	data, _ := os.ReadFile(filepath.Join(modulesLoadLocation, "x.conf"))
	// The module rewrites with "\n".join(lines) over newline-kept lines.
	if string(data) != "#dummy # comment\n\nother\n" {
		t.Fatalf("got %q", data)
	}
	if m.loadedPersistently() {
		t.Fatal("still persistent")
	}
	if !reflect.DeepEqual(m.permanentParams(), map[string]bool{"numdummies=2": true}) {
		t.Fatal(m.permanentParams())
	}
}
