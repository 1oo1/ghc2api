package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunHelp(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"help"}, {"serve", "--help"}, {"login", "--help"}} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if err := run(context.Background(), args, &stdout, &stderr); err != nil {
				t.Fatalf("help error = %v", err)
			}
			if stdout.Len()+stderr.Len() == 0 {
				t.Fatal("help output is empty")
			}
		})
	}
}

func TestRunRejectsUnknownCommandAndFlag(t *testing.T) {
	for _, args := range [][]string{{"unknown"}, {"serve", "--unknown"}, {"login", "--unknown"}} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			if err := run(context.Background(), args, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
				t.Fatal("unknown command/flag accepted")
			}
		})
	}
}

func TestServeRejectsUnsafeBindingWithoutAPIKey(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "test-token")
	t.Setenv("GHC2API_API_KEY", "")
	// 使用取消的上下文避免错误实现意外启动服务后永久阻塞。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := run(ctx, []string{"serve", "--listen", ":0"}, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || (!strings.Contains(strings.ToLower(err.Error()), "api") && !strings.Contains(err.Error(), "密钥")) {
		t.Fatalf("non-loopback without API key must fail validation, got %v", err)
	}
}

func TestServeRejectsUnsupportedHost(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "test-token")
	t.Setenv("GHC2API_API_KEY", "local-key")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := run(ctx, []string{"serve", "--host", "evil.example"}, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Fatal("unsafe host accepted by serve")
	}
}

func testCredentialFile(t *testing.T, raw string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "credential.json")
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func testEditorConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "github-copilot"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "github-copilot", "apps.json"), []byte(`{"github.com":{"oauth_token":"editor-token"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLoadConfigTokenPriorityAndPreservedCredentialFields(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GHC2API_API_KEY", "api-secret")
	path := testCredentialFile(t, `{"oauth_token":"file-token","host":"acme.ghe.com","token_mode":"cli","user":"alice"}`)
	editor := testEditorConfig(t)
	c, err := loadConfig(path, editor, "", "")
	if err != nil || c.Token != "file-token" || c.Host != "acme.ghe.com" || c.TokenMode != "cli" || c.APIKey != "api-secret" {
		t.Fatalf("file configuration = %+v, %v", c, err)
	}
	c, err = loadConfig(path, editor, "github.com", "oauth")
	if err != nil || c.Host != "" || c.TokenMode != "oauth" || c.Token != "file-token" {
		t.Fatalf("explicit overrides = %+v, %v", c, err)
	}
	t.Setenv("GITHUB_TOKEN", "environment-token")
	c, err = loadConfig(path, editor, "", "")
	if err != nil || c.Token != "environment-token" || c.APIKey != "api-secret" {
		t.Fatalf("environment priority = %+v, %v", c, err)
	}
}

func TestLoadConfigEditorFallback(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GHC2API_API_KEY", "")
	editor := testEditorConfig(t)
	c, err := loadConfig("", editor, "", "")
	if err != nil || c.Token != "editor-token" || c.Host != "" {
		t.Fatalf("editor fallback = %+v, %v", c, err)
	}
}

func TestLoadConfigExplicitInvalidFileCannotFallBack(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	editor := testEditorConfig(t)
	for _, path := range []string{
		testCredentialFile(t, "invalid JSON"),
		testCredentialFile(t, `{"oauth_token":"file-token","host":"evil.example"}`),
		filepath.Join(t.TempDir(), "missing.json"),
		t.TempDir(),
	} {
		t.Run(path, func(t *testing.T) {
			if _, err := loadConfig(path, editor, "", ""); err == nil {
				t.Fatal("explicit invalid/unreadable credential fell back to editor")
			}
		})
	}
}
