package inventory

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/giraffesyo/understudy/internal/yaml"
)

// Decrypt transparently decrypts vault-encrypted group_vars/host_vars
// files. Set before Load when a vault password is available; nil leaves
// files untouched (an encrypted file then fails to parse with a clear
// vault error).
var Decrypt func([]byte) ([]byte, error)

// Load builds an inventory from -i sources: files (INI or YAML by sniffing),
// directories (each file loaded), or literal host lists ("h1,h2,").
// group_vars/ and host_vars/ next to file sources are applied, then any
// varsDirs (e.g. the playbook directory) in order — later wins.
func Load(sources []string, varsDirs []string) (*Inventory, error) {
	inv := New()
	var adjacentDirs []string

	for _, src := range sources {
		if strings.Contains(src, ",") {
			for _, name := range strings.Split(src, ",") {
				if name = strings.TrimSpace(name); name != "" {
					names, err := ExpandRange(name)
					if err != nil {
						return nil, err
					}
					for _, n := range names {
						addHostToGroup(inv.Groups["ungrouped"], inv.ensureHost(n))
					}
				}
			}
			continue
		}
		info, err := os.Stat(src)
		if err != nil {
			return nil, fmt.Errorf("inventory source %q: %w", src, err)
		}
		if info.IsDir() {
			entries, err := os.ReadDir(src)
			if err != nil {
				return nil, err
			}
			var names []string
			for _, e := range entries {
				if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
					continue
				}
				switch e.Name() {
				case "group_vars", "host_vars":
					continue
				}
				names = append(names, e.Name())
			}
			sort.Strings(names)
			for _, name := range names {
				if err := loadFile(inv, filepath.Join(src, name)); err != nil {
					return nil, err
				}
			}
			adjacentDirs = append(adjacentDirs, src)
		} else {
			if err := loadFile(inv, src); err != nil {
				return nil, err
			}
			adjacentDirs = append(adjacentDirs, filepath.Dir(src))
		}
	}

	if len(inv.Hosts) == 0 {
		// Implicit localhost, like Ansible with an empty inventory.
		h := inv.ensureHost("localhost")
		h.Vars["ansible_connection"] = "local"
		addHostToGroup(inv.Groups["ungrouped"], h)
	}

	if err := inv.finalize(); err != nil {
		return nil, err
	}

	// vars directories: inventory-adjacent first, then explicit (playbook)
	// dirs — later application wins on key conflicts.
	for _, dir := range append(adjacentDirs, varsDirs...) {
		if err := applyVarsDirs(inv, dir); err != nil {
			return nil, err
		}
	}
	return inv, nil
}

// loadFile sniffs INI vs YAML and loads one inventory file.
func loadFile(inv *Inventory, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if looksLikeYAML(path, data) {
		return LoadYAML(inv, data, path)
	}
	return LoadINI(inv, data, path)
}

func looksLikeYAML(path string, data []byte) bool {
	switch filepath.Ext(path) {
	case ".yml", ".yaml", ".json":
		return true
	}
	for _, line := range strings.Split(string(data), "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		// INI inventories open with [section] or bare host lines; YAML
		// inventories open with "all:" or "---".
		if strings.HasPrefix(t, "---") {
			return true
		}
		if strings.HasPrefix(t, "[") {
			return false
		}
		if strings.HasSuffix(t, ":") || strings.Contains(t, ": ") {
			return true
		}
		return false
	}
	return false
}

// applyVarsDirs loads group_vars/ and host_vars/ under dir. Both file
// (group_vars/web.yml, bare group_vars/web) and directory
// (group_vars/web/*.yml) forms are supported.
func applyVarsDirs(inv *Inventory, dir string) error {
	if err := applyVarsDir(inv, filepath.Join(dir, "group_vars"), func(name string) map[string]any {
		if g, ok := inv.Groups[name]; ok {
			return g.Vars
		}
		return nil
	}); err != nil {
		return err
	}
	return applyVarsDir(inv, filepath.Join(dir, "host_vars"), func(name string) map[string]any {
		if h, ok := inv.Hosts[name]; ok {
			return h.Vars
		}
		return nil
	})
}

func applyVarsDir(inv *Inventory, dir string, lookup func(string) map[string]any) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		full := filepath.Join(dir, name)
		info, err := os.Stat(full)
		if err != nil {
			return err
		}
		base := strings.TrimSuffix(strings.TrimSuffix(name, ".yml"), ".yaml")
		target := lookup(base)
		if target == nil {
			continue // vars for a group/host not in this inventory
		}
		if info.IsDir() {
			subEntries, err := os.ReadDir(full)
			if err != nil {
				return err
			}
			var subNames []string
			for _, se := range subEntries {
				if !se.IsDir() && (strings.HasSuffix(se.Name(), ".yml") || strings.HasSuffix(se.Name(), ".yaml")) {
					subNames = append(subNames, se.Name())
				}
			}
			sort.Strings(subNames)
			for _, sn := range subNames {
				if err := mergeVarsFile(filepath.Join(full, sn), target); err != nil {
					return err
				}
			}
			continue
		}
		if strings.HasPrefix(name, ".") {
			continue
		}
		if err := mergeVarsFile(full, target); err != nil {
			return err
		}
	}
	return nil
}

func mergeVarsFile(path string, into map[string]any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if Decrypt != nil {
		if data, err = Decrypt(data); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	v, err := yaml.Unmarshal(data, path)
	if err != nil {
		return err
	}
	if v == nil {
		return nil
	}
	m, ok := yaml.PlainMap(v)
	if !ok {
		return fmt.Errorf("%s: vars file must contain a mapping", path)
	}
	for k, val := range m {
		into[k] = val
	}
	return nil
}
