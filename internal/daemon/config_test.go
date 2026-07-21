package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "obold.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadConfigValid(t *testing.T) {
	p := writeConfig(t, `{
		"accounts": [
			{"name": "lab_smith", "balance": 100000, "rate": 1, "window": "720h"},
			{"name": "lab_jones", "balance": 50000, "rate": 2, "allow_groups": ["jones"]}
		]
	}`)
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(c.Accounts) != 2 {
		t.Fatalf("accounts = %d, want 2", len(c.Accounts))
	}
	if !c.Accounts[1].restricted() {
		t.Error("lab_jones should be restricted (has allow_groups)")
	}
	if c.Accounts[0].restricted() {
		t.Error("lab_smith should be open (no allow-list)")
	}
}

func TestLoadConfigRejectsBad(t *testing.T) {
	cases := map[string]string{
		"no accounts":      `{"accounts": []}`,
		"dup name":         `{"accounts":[{"name":"a","balance":1,"rate":1},{"name":"a","balance":1,"rate":1}]}`,
		"empty name":       `{"accounts":[{"name":"","balance":1,"rate":1}]}`,
		"negative balance": `{"accounts":[{"name":"a","balance":-5,"rate":1}]}`,
		"zero rate":        `{"accounts":[{"name":"a","balance":1,"rate":0}]}`,
		"bad window":       `{"accounts":[{"name":"a","balance":1,"rate":1,"window":"nope"}]}`,
		"unknown field":    `{"accounts":[{"name":"a","balance":1,"rate":1,"bogus":true}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadConfig(writeConfig(t, body)); err == nil {
				t.Errorf("expected LoadConfig to reject %q", name)
			}
		})
	}
}
