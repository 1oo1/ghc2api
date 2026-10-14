package copilot

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Credential 是独立服务的凭证格式，不修改 magpie 或编辑器的账号文件。
type Credential struct {
	Token string `json:"oauth_token"`
	Host  string `json:"host,omitempty"`
	User  string `json:"user,omitempty"`
}

func validateCredential(c Credential) (Credential, error) {
	config, err := New(Config{Token: c.Token, Host: c.Host})
	if err != nil {
		return Credential{}, err
	}
	c.Token, c.Host = config.config.Token, config.config.Host
	return c, nil
}

// LoadCredential 拒绝缺少 token、非法 host 或含有不支持字段的文件。
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
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&c); err != nil {
		return Credential{}, errors.New("凭证文件不是有效的 OAuth JSON")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return Credential{}, errors.New("凭证文件只能包含一个 JSON 对象")
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
