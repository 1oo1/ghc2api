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

// TestCredentialDisabledFlagRoundTrip disabled 为 true 时写入文件，为 false 时省略该字段，读回后保持原值。
func TestCredentialDisabledFlagRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alice.json")
	if err := SaveCredential(path, Credential{Token: "oauth-disabled", User: "alice", Disabled: true}); err != nil {
		t.Fatal(err)
	}
	c, err := LoadCredential(path)
	if err != nil || !c.Disabled {
		t.Fatalf("LoadCredential(disabled) = %+v, %v; want Disabled=true", c, err)
	}
	fields := readCredentialFieldsForTest(t, path)
	if fields["disabled"] != true {
		t.Fatalf("禁用的凭证文件 disabled 字段 = %v, want true", fields["disabled"])
	}

	if err := SaveCredential(path, Credential{Token: "oauth-disabled", User: "alice"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := readCredentialFieldsForTest(t, path)["disabled"]; ok {
		t.Fatal("启用的凭证不应写入 disabled 字段")
	}
	c, err = LoadCredential(path)
	if err != nil || c.Disabled {
		t.Fatalf("LoadCredential(enabled) = %+v, %v; want Disabled=false", c, err)
	}
}

// TestCredentialDisabledFieldReadFromFile 手写的凭证文件中 disabled 被读取，省略时为 false，非布尔值被拒绝。
func TestCredentialDisabledFieldReadFromFile(t *testing.T) {
	for _, tc := range []struct {
		name    string
		raw     string
		want    bool
		wantErr bool
	}{
		{name: "true", raw: `{"oauth_token":"t","disabled":true}`, want: true},
		{name: "false", raw: `{"oauth_token":"t","disabled":false}`, want: false},
		{name: "omitted", raw: `{"oauth_token":"t"}`, want: false},
		{name: "not-a-boolean", raw: `{"oauth_token":"t","disabled":"yes"}`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "credential.json")
			if err := os.WriteFile(path, []byte(tc.raw), 0o600); err != nil {
				t.Fatal(err)
			}
			c, err := LoadCredential(path)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("LoadCredential(%s) should reject", tc.raw)
				}
				return
			}
			if err != nil || c.Disabled != tc.want {
				t.Fatalf("LoadCredential(%s) = %+v, %v; want Disabled=%v", tc.raw, c, err, tc.want)
			}
		})
	}
}

// readCredentialFieldsForTest 以原始 JSON 对象读取凭证文件，用于断言文件中是否存在某个字段。
func readCredentialFieldsForTest(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	return fields
}
