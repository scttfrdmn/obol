package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// account.json is the daemon-owned per-account metadata that lives beside the
// kernel's WAL/snapshot in an account's state dir. It records what the kernel
// snapshot deliberately does not: the account NAME and its ACCESS lists (the
// kernel knows nothing about "users"). This is what makes runtime-created
// accounts and attach/detach changes survive a restart — the daemon discovers
// account dirs and reads this file to reconstruct the registry.
const accountMetaFile = "account.json"

// accountMeta is exactly what account.json holds — the name and access lists.
// Balance/rate/window are the kernel's (in snapshot.json) and are deliberately
// NOT duplicated here.
type accountMeta struct {
	Name        string   `json:"name"`
	AllowUsers  []string `json:"allow_users,omitempty"`
	AllowGroups []string `json:"allow_groups,omitempty"`
}

// writeAccountMeta atomically writes an account's metadata (name + access lists)
// into its state dir. Same temp+rename discipline as the kernel snapshot.
func writeAccountMeta(dir string, a AccountConfig) error {
	data, err := json.Marshal(accountMeta{Name: a.Name, AllowUsers: a.AllowUsers, AllowGroups: a.AllowGroups})
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, accountMetaFile+".tmp")
	final := filepath.Join(dir, accountMetaFile)
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, final) // atomic: account.json is old or new, never partial
}

// readAccountMeta loads an account's metadata into an AccountConfig (name +
// access only). When the file is absent (an account dir created before metadata
// existed), it falls back to the dir's base name and empty access — a safe,
// open-access default.
func readAccountMeta(dir string) (AccountConfig, error) {
	data, err := os.ReadFile(filepath.Join(dir, accountMetaFile)) //nolint:gosec // daemon-owned path
	if err != nil {
		if os.IsNotExist(err) {
			return AccountConfig{Name: filepath.Base(dir)}, nil
		}
		return AccountConfig{}, err
	}
	var m accountMeta
	if err := json.Unmarshal(data, &m); err != nil {
		return AccountConfig{}, err
	}
	if m.Name == "" {
		m.Name = filepath.Base(dir)
	}
	return AccountConfig{Name: m.Name, AllowUsers: m.AllowUsers, AllowGroups: m.AllowGroups}, nil
}
