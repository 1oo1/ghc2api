package copilot

import (
	"strings"
	"testing"
)

// TestValidGitHubUserName 用户名为 1 到 100 个字符；由字母、数字组成的片段以单个 "-" 或 "_" 分隔，
// 不以分隔符开头或结尾。"_" 用于支持 GHE.com 的企业托管用户登录名（如 mona-cat_octo）。
func TestValidGitHubUserName(t *testing.T) {
	for _, name := range []string{
		"a", "A", "0", "alice", "a-b", "a_b", "mona-cat_octo", "A1-b2-C3", "x_1-y2",
		strings.Repeat("a", 39), strings.Repeat("a", 100),
	} {
		if !ValidGitHubUserName(name) {
			t.Errorf("ValidGitHubUserName(%q) = false, want true", name)
		}
	}
	for _, name := range []string{
		"", "-", "_", "-a", "_a", "a-", "a_", "a--b", "a__b", "a-_b", "a_-b",
		"a.b", "a b", "a/b", `a\b`, "..", ".", "a+b", "a@b", "é", "a\x00b",
		strings.Repeat("a", 101),
	} {
		if ValidGitHubUserName(name) {
			t.Errorf("ValidGitHubUserName(%q) = true, want false", name)
		}
	}
}
