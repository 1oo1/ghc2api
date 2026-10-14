package copilot

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestNormalizeHost(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"", ""}, {"github.com", ""}, {"HTTPS://GITHUB.COM/", ""},
		{" acme.ghe.com ", "acme.ghe.com"}, {"https://ACME.ghe.com/", "acme.ghe.com"},
		{"x.ghe.com", "x.ghe.com"}, {"a-b.ghe.com", "a-b.ghe.com"},
	} {
		t.Run(tc.input, func(t *testing.T) {
			got, err := NormalizeHost(tc.input)
			if err != nil || got != tc.want {
				t.Fatalf("NormalizeHost(%q) = %q, %v; want %q", tc.input, got, err, tc.want)
			}
		})
	}
	for _, input := range []string{"evil.example", "https://evil.example", "api.acme.ghe.com", "a.b.ghe.com", "-a.ghe.com", "a-.ghe.com", "https://github.com:443", "https://github.com/path", "https://github.com?secret=1", "https://github.com#fragment", "https://user" + "@" + "github.com"} {
		t.Run(input, func(t *testing.T) {
			if _, err := NormalizeHost(input); err == nil {
				t.Fatalf("NormalizeHost(%q) should fail", input)
			}
		})
	}
}

func TestCredentialSaveLoadAndPrivatePermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "credential.json")
	want := Credential{Token: "oauth-secret", Host: "acme.ghe.com", User: "alice"}
	if err := SaveCredential(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadCredential(path)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("LoadCredential = %+v, %v; want %+v", got, err, want)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("credential permissions = %v, %v", info, err)
	}
	var raw map[string]any
	bytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(bytes, &raw); err != nil {
		t.Fatal(err)
	}
	if raw["oauth_token"] != "oauth-secret" || raw["host"] != "acme.ghe.com" || raw["user"] != "alice" {
		t.Fatalf("credential JSON fields = %v", raw)
	}
	if _, ok := raw["token_mode"]; ok {
		t.Fatalf("credential JSON still stores token_mode: %v", raw)
	}
	if _, ok := reflect.TypeOf(Credential{}).FieldByName("Mode"); ok {
		t.Fatal("Credential must not expose a token mode")
	}
	// 覆盖已有的宽松权限文件时也必须收紧，不能把 token 留成全局可读。
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	want.Token = "replacement-secret"
	if err := SaveCredential(path, want); err != nil {
		t.Fatal(err)
	}
	info, _ = os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("replacement credential permissions = %v", info.Mode().Perm())
	}
}

func TestCredentialsRejectInvalidInput(t *testing.T) {
	for _, raw := range []string{
		`broken-json`,
		`{}`,
		`{"oauth_token":""}`,
		`{"oauth_token":"t","host":"evil.example"}`,
		`{"oauth_token":"t","token_mode":"cli"}`,
	} {
		t.Run(raw, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "credential.json")
			if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadCredential(path); err == nil {
				t.Fatalf("LoadCredential(%q) should reject", raw)
			}
		})
	}
	for _, c := range []Credential{{}, {Token: "t", Host: "evil.example"}} {
		if err := SaveCredential(filepath.Join(t.TempDir(), "credential.json"), c); err == nil {
			t.Fatalf("SaveCredential(%+v) should reject", c)
		}
	}
}

func TestDiscoverEditorPrefersGitHubAndFallsBackToHosts(t *testing.T) {
	for _, tc := range []struct{ name, apps, hosts, token, host string }{
		{"github-over-enterprise", `{"acme.ghe.com:Iv1.app":{"oauth_token":"enterprise","user":"alice"}}`, `{"github.com":{"oauth_token":"github","user":"bob"}}`, "github", ""},
		{"apps-before-hosts", `{"github.com:Iv1.app":{"oauth_token":"apps-token","user":"alice"}}`, `{"github.com":{"oauth_token":"hosts-token"}}`, "apps-token", ""},
		{"malformed-apps", `not-json`, `{"github.com":{"oauth_token":"hosts-token","user":"alice"}}`, "hosts-token", ""},
		{"enterprise-only", `{"acme.ghe.com:Iv1.app":{"oauth_token":"enterprise","user":"alice"}}`, `{}`, "enterprise", "acme.ghe.com"},
		{"ignore-impostor-host", `{"github.com.evil.example":{"oauth_token":"evil"},"github.com:Iv1.app":{"oauth_token":"safe"}}`, `{}`, "safe", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := t.TempDir()
			dir := filepath.Join(config, "github-copilot")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			for name, value := range map[string]string{"apps.json": tc.apps, "hosts.json": tc.hosts} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			c, err := DiscoverEditor(config)
			if err != nil || c.Token != tc.token || c.Host != tc.host {
				t.Fatalf("DiscoverEditor = %+v, %v; want token=%q host=%q", c, err, tc.token, tc.host)
			}
		})
	}
}

func TestDiscoverEditorMissingOrUnsupportedFails(t *testing.T) {
	config := t.TempDir()
	if _, err := DiscoverEditor(config); err == nil {
		t.Fatal("missing editor token should fail")
	}
	dir := filepath.Join(config, "github-copilot")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw := `{"evil.example":{"oauth_token":"secret"},"github.com.evil.example":{"oauth_token":"impostor"}}`
	if err := os.WriteFile(filepath.Join(dir, "apps.json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := DiscoverEditor(config); err == nil {
		t.Fatal("unsupported editor host should fail")
	}
}
