package copilot

import "regexp"

// maxGitHubUserNameLength 是用户名的最大长度。企业托管用户（EMU）的登录名较长，需要容纳短码后缀。
const maxGitHubUserNameLength = 100

// gitHubUserNamePattern 只允许字母与数字组成的片段，片段之间以单个 "-" 或 "_" 分隔，
// 因此不以分隔符开头或结尾，也不含连续分隔符。
var gitHubUserNamePattern = regexp.MustCompile(`^[A-Za-z0-9]+(?:[-_][A-Za-z0-9]+)*$`)

// ValidGitHubUserName 报告 name 是否为合法的 GitHub 用户名（凭证文件名去掉 .json 后的部分）。
// 这是全项目唯一的用户名规则：启动加载、管理导入、设备登录与 NewManagedPool 共用。
func ValidGitHubUserName(name string) bool {
	return len(name) >= 1 && len(name) <= maxGitHubUserNameLength && gitHubUserNamePattern.MatchString(name)
}
