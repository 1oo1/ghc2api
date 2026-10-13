package copilot

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Credential 是独立服务的凭证格式，不修改 magpie 或编辑器的账号文件。
type Credential struct {
	Token string `json:"oauth_token"`
	Host  string `json:"host,omitempty"`
	User  string `json:"user,omitempty"`
	Mode  string `json:"token_mode,omitempty"`
}

func validateCredential(c Credential) (Credential, error) {
	config, err := New(Config{Token: c.Token, TokenMode: c.Mode, Host: c.Host})
	if err != nil {
		return Credential{}, err
	}
	c.Token, c.Host, c.Mode = config.config.Token, config.config.Host, config.config.TokenMode
	return c, nil
}

// LoadCredential 拒绝缺少 token、非法 host 或非法模式的文件。
func LoadCredential(path string) (Credential, error) {
	file, err := os.Open(path)
	if err != nil {
		return Credential{}, err
	}
	defer file.Close()
	raw, err := readLimited(file, 1<<20)
	if err != nil {
		return Credential{}, err
	}
	var c Credential
	if json.Unmarshal(raw, &c) != nil {
		return Credential{}, errors.New("凭证文件不是有效 JSON")
	}
	return validateCredential(c)
}

// SaveCredential 先写私有临时文件再原子替换，失败时不破坏已有登录。
func SaveCredential(path string, c Credential) error {
	c, err := validateCredential(c)
	if err != nil {
		return err
	}
	if strings.TrimSpace(path) == "" {
		return errors.New("凭证文件路径不能为空")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".ghc2api-credential-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

// DiscoverEditor 只读 apps/hosts 文件，github.com 优先，再使用企业登录。
func DiscoverEditor(configDir string) (Credential, error) {
	var enterprise *Credential
	for _, name := range []string{"apps.json", "hosts.json"} {
		file, err := os.Open(filepath.Join(configDir, "github-copilot", name))
		if err != nil {
			continue
		}
		data, err := readLimited(file, 4<<20)
		_ = file.Close()
		if err != nil {
			continue
		}
		var apps map[string]Credential
		if json.Unmarshal(data, &apps) != nil {
			continue
		}
		keys := make([]string, 0, len(apps))
		for key := range apps {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			host, _, _ := strings.Cut(key, ":")
			normalized, err := NormalizeHost(host)
			if err != nil || host == "" {
				continue
			}
			c := apps[key]
			c.Host, c.Mode = normalized, "oauth"
			c, err = validateCredential(c)
			if err != nil {
				continue
			}
			if c.Host == "" {
				return c, nil
			}
			if enterprise == nil {
				enterprise = &c
			}
		}
	}
	if enterprise != nil {
		return *enterprise, nil
	}
	return Credential{}, errors.New("指定目录没有支持的 Copilot 编辑器登录")
}
