package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"ghc2api/internal/copilot"
)

// credentialsDirName 是 data_path 下存放凭证文件的子目录。
const credentialsDirName = "credentials"

// credentialFileExt 是凭证文件的扩展名；去掉它后的文件名即 GitHub 用户名。
const credentialFileExt = ".json"

// maxGitHubUserNameLength 是 GitHub 用户名允许的最大长度。
const maxGitHubUserNameLength = 39

// githubUserName 只允许字母和数字组成的片段，片段之间以单个连字符分隔，
// 因此不会以连字符开头或结尾，也不含连续连字符。
var githubUserName = regexp.MustCompile(`^[A-Za-z0-9]+(-[A-Za-z0-9]+)*$`)

// loadAccounts 扫描 credentialsDir 这一层的凭证文件，每个文件对应一个账号，结果按文件名字节序排列。
// 只处理扩展名为 .json 的普通文件（符号链接按其目标判断）；隐藏文件、其他扩展名、子目录和指向目录的链接都被忽略。
// 顶层 host 非空时覆盖所有账号的 host。账号重复由 copilot.NewPool 检查。
func loadAccounts(credentialsDir, topHost string) ([]accountConfig, error) {
	entries, err := os.ReadDir(credentialsDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("credentials 目录 %q 不存在，请放入以 GitHub 用户名命名的 .json 凭证文件", credentialsDir)
		}
		return nil, fmt.Errorf("无法读取 credentials 目录 %q: %w", credentialsDir, err)
	}
	var accounts []accountConfig
	// GitHub 用户名不区分大小写，因此仅大小写不同的文件名视为同一账号；记录已见用户名对应的文件名。
	fileByUser := make(map[string]string, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if !isCredentialCandidate(name) {
			continue
		}
		path := filepath.Join(credentialsDir, name)
		regular, err := isRegularFile(path)
		if err != nil {
			return nil, fmt.Errorf("无法检查凭证文件 %q: %w", name, err)
		}
		if !regular {
			continue
		}
		account, err := loadAccount(path, name, topHost)
		if err != nil {
			return nil, err
		}
		key := strings.ToLower(account.User)
		if previous, duplicate := fileByUser[key]; duplicate {
			return nil, fmt.Errorf("凭证文件 %q 与 %q 的用户名仅大小写不同，视为重复账号", previous, name)
		}
		fileByUser[key] = name
		accounts = append(accounts, account)
	}
	if len(accounts) == 0 {
		return nil, fmt.Errorf("credentials 目录 %q 中没有凭证文件，请放入以 GitHub 用户名命名的 .json 文件", credentialsDir)
	}
	return accounts, nil
}

// isCredentialCandidate 只看文件名：隐藏文件（含临时文件）和非 .json 扩展名的条目被忽略。
func isCredentialCandidate(name string) bool {
	return !strings.HasPrefix(name, ".") && strings.HasSuffix(name, credentialFileExt)
}

// isRegularFile 通过 os.Stat 跟随符号链接，只接受指向普通文件的路径；悬空链接按错误处理。
func isRegularFile(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	return info.Mode().IsRegular(), nil
}

// loadAccount 读取一个凭证文件，校验文件名是合法的 GitHub 用户名，且 user 字段（若非空）与文件名一致。
// 错误信息中的文件名都带 .json 后缀，便于定位。
func loadAccount(path, fileName, topHost string) (accountConfig, error) {
	user := strings.TrimSuffix(fileName, credentialFileExt)
	if !validGitHubUserName(user) {
		return accountConfig{}, fmt.Errorf("凭证文件 %q 的文件名不是合法的 GitHub 用户名："+
			"只能包含字母、数字和单个连字符，不能以连字符开头或结尾，长度 1 到 %d", fileName, maxGitHubUserNameLength)
	}
	credential, err := copilot.LoadCredential(path)
	if err != nil {
		return accountConfig{}, fmt.Errorf("无法加载凭证文件 %q: %w", fileName, err)
	}
	if credential.User != "" && !strings.EqualFold(credential.User, user) {
		return accountConfig{}, fmt.Errorf("凭证文件 %q 的 user 字段 %q 与文件名中的用户名不一致", fileName, credential.User)
	}
	if topHost != "" {
		credential.Host = topHost
	}
	host, err := copilot.NormalizeHost(credential.Host)
	if err != nil {
		return accountConfig{}, err
	}
	account := accountConfig{Token: credential.Token, Host: host, User: user, credentialFile: path}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			return accountConfig{}, fmt.Errorf("无法检查凭证文件 %q 的权限: %w", fileName, err)
		}
		account.credentialMode = info.Mode().Perm()
		account.insecureCredential = account.credentialMode&0o077 != 0
	}
	return account, nil
}

// validGitHubUserName 检查用户名的长度与字符规则。
func validGitHubUserName(name string) bool {
	return len(name) >= 1 && len(name) <= maxGitHubUserNameLength && githubUserName.MatchString(name)
}
