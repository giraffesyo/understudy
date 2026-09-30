package modules

import "github.com/giraffesyo/understudy/internal/modules/args"

// specs maps module names (short and FQCN) to their declared argument
// specs, for static validation of playbook content (tools/argscan). Modules
// that parse their arguments ad hoc (command, shell, script, setup, ping)
// are absent.
var specs = map[string]args.Spec{}

func init() {
	for spec, names := range map[*args.Spec][]string{
		&copySpec:        {"copy", "ansible.builtin.copy"},
		&blockinfileSpec: {"blockinfile", "ansible.builtin.blockinfile"},
		&iptablesSpec:    {"iptables", "ansible.builtin.iptables"},
		&groupSpec:       {"group", "ansible.builtin.group"},
		&getURLSpec:      {"get_url", "ansible.builtin.get_url"},
		&cronSpec:        {"cron", "ansible.builtin.cron"},
		&fileSpec:        {"file", "ansible.builtin.file"},
		&mountSpec:       {"mount", "ansible.posix.mount"},
		&mysqlDBSpec:     {"mysql_db", "community.mysql.mysql_db"},
		&mysqlUserSpec:   {"mysql_user", "community.mysql.mysql_user"},
		&tempfileSpec:    {"tempfile", "ansible.builtin.tempfile"},
		&findSpec:        {"find", "ansible.builtin.find"},
		&modprobeSpec:    {"modprobe", "community.general.modprobe"},
		&lineinfileSpec:  {"lineinfile", "ansible.builtin.lineinfile"},
		&pkgSpec: {"package", "ansible.builtin.package", "apt", "ansible.builtin.apt",
			"dnf", "ansible.builtin.dnf", "yum", "ansible.builtin.yum", "apk", "community.general.apk"},
		&pipSpec: {"pip", "ansible.builtin.pip"},
		&serviceSpec: {"service", "systemd", "systemd_service", "ansible.builtin.service",
			"ansible.builtin.systemd", "ansible.builtin.systemd_service"},
		&yumRepoSpec:    {"yum_repository", "ansible.builtin.yum_repository"},
		&sudoersSpec:    {"sudoers", "community.general.sudoers"},
		&keypairSpec:    {"openssh_keypair", "community.crypto.openssh_keypair"},
		&partedSpec:     {"parted", "community.general.parted"},
		&filesystemSpec: {"filesystem", "community.general.filesystem"},
		&statSpec:       {"stat", "ansible.builtin.stat"},
		&sysctlSpec:     {"sysctl", "ansible.posix.sysctl"},
		&firewalldSpec:  {"firewalld", "ansible.posix.firewalld"},
		&selinuxSpec:    {"selinux", "ansible.posix.selinux"},
		&waitForSpec:    {"wait_for", "ansible.builtin.wait_for"},
		&userSpec:       {"user", "ansible.builtin.user"},
		&replaceSpec:    {"replace", "ansible.builtin.replace"},
	} {
		for _, n := range names {
			specs[n] = *spec
		}
	}
}

// SpecOf returns a module's declared argument spec.
func SpecOf(name string) (args.Spec, bool) {
	s, ok := specs[name]
	return s, ok
}
