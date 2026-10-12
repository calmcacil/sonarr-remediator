package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStrictPrecedenceAndSecrets(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "key")
	os.WriteFile(secret, []byte("sensitive-key\n"), 0600)
	path := filepath.Join(dir, "config.yml")
	os.WriteFile(path, []byte("sonarr:\n  url: http://old:8989/base\n  api_key_file: "+secret+"\npoll_interval: 2m\n"), 0600)
	c, e := Load(path, []string{"SRA_SONARR__URL=https://new/base", "SRA_POLL_INTERVAL=30s", "SRA_SCOPE__INCLUDE_SERIES_IDS=[1, 2]"})
	if e != nil {
		t.Fatal(e)
	}
	if c.Sonarr.URL != "https://new/base" || c.PollInterval != 30*time.Second || c.Sonarr.APIKey != "sensitive-key" || len(c.Scope.IncludeSeriesIDs) != 2 || !c.DryRun {
		t.Fatalf("wrong precedence/defaults: %#v", c.Scope)
	}
	for _, env := range [][]string{{"SRA_NOT_REAL=1"}, {"SRA_DRY_RUN=null"}, {"SRA_POLL_INTERVAL=0s"}, {"SRA_CLEANUP__BLOCKLIST=true"}, {"SRA_SONARR__API_KEY=should-not-be-printed"}, {"SRA_SCOPE__INCLUDE_SERIES_IDS=[0]"}} {
		_, e = Load(path, env)
		if e == nil || strings.Contains(e.Error(), "should-not-be-printed") {
			t.Fatalf("invalid config allowed/leaked: %v", env)
		}
	}
	os.WriteFile(path, []byte("sonarr:\n  url: http://x\n  api_key: hidden\nunknown: true\n"), 0600)
	if _, e = Load(path, nil); e == nil || strings.Contains(e.Error(), "hidden") {
		t.Fatal("unknown key accepted/leaked")
	}
}
func TestConfigurationRejectsMalformedInput(t *testing.T) {
	for _, body := range []string{"dry_run: true\ndry_run: false\n", "dry_run: true\n---\ndry_run: false\n", "poll_interval: bananas\n", "sonarr:\n  url: http://user:pass@x\n  api_key: secret\n"} {
		p := filepath.Join(t.TempDir(), "config.yml")
		os.WriteFile(p, []byte(body), 0600)
		if _, e := Load(p, []string{"SRA_SONARR__URL=http://x", "SRA_SONARR__API_KEY=secret"}); e == nil && !strings.Contains(body, "user:pass") {
			t.Fatalf("accepted %q", body)
		}
	}
}
