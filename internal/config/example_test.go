package config

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// TestExampleConfigListsEveryKey keeps config.example.toml in step with
// Config: every key appears exactly once, and the file still loads with every
// commented-out default switched on, giving the built-in defaults.
func TestExampleConfigListsEveryKey(t *testing.T) {
	raw, err := os.ReadFile("../../config.example.toml")
	if err != nil {
		t.Fatal(err)
	}
	keyLine := regexp.MustCompile(`^(?:# )?([a-z_]+) = `)
	seen := map[string]int{}
	var out []string
	for _, ln := range strings.Split(string(raw), "\n") {
		if m := keyLine.FindStringSubmatch(ln); m != nil {
			seen[m[1]]++
			ln = strings.TrimPrefix(ln, "# ")
		}
		out = append(out, ln)
	}
	ty := reflect.TypeOf(Config{})
	for i := 0; i < ty.NumField(); i++ {
		k := ty.Field(i).Tag.Get("toml")
		if seen[k] != 1 {
			t.Errorf("config.example.toml: key %s appears %d times, want 1", k, seen[k])
		}
		delete(seen, k)
	}
	for k := range seen {
		t.Errorf("config.example.toml: %s is not a config key", k)
	}

	p := filepath.Join(t.TempDir(), "all.toml")
	os.WriteFile(p, []byte(strings.Join(out, "\n")), 0o600)
	c, err := Load([]string{"-config", p}, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	d := Defaults()
	d.APIURL = c.APIURL
	c.PriceAudioPerMin, c.PriceInPerMTok, c.PriceOutPerMTok = nil, nil, nil
	if !reflect.DeepEqual(c, d) {
		t.Errorf("example values differ from Defaults():\n got %+v\nwant %+v", c, d)
	}
}
