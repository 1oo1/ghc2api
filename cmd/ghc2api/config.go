package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"ghc2api/internal/copilot"
	"gopkg.in/yaml.v3"
)

const maxConfigSize = 1 << 20

type appConfig struct {
	Listen              string        `yaml:"listen"`
	GitHubToken         string        `yaml:"github_token"`
	Credentials         string        `yaml:"credentials"`
	EditorConfig        string        `yaml:"editor_config"`
	Host                string        `yaml:"host"`
	APIKey              string        `yaml:"api_key"`
	LogLevel            string        `yaml:"log_level"`
	ModelsCacheTTLValue string        `yaml:"models_cache_ttl"`
	ModelsCacheTTL      time.Duration `yaml:"-"`

	Token               string      `yaml:"-"`
	configMode          os.FileMode `yaml:"-"`
	insecurePermissions bool        `yaml:"-"`
	credentialFile      string      `yaml:"-"`
	credentialMode      os.FileMode `yaml:"-"`
	insecureCredential  bool        `yaml:"-"`
}

// loadConfig 只读取 YAML 中的设置，不读取环境变量或命令行覆盖项。
func loadConfig(path string) (appConfig, error) {
	file, err := os.Open(path)
	if err != nil {
		return appConfig{}, fmt.Errorf("无法打开配置文件 %q: %w", path, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return appConfig{}, fmt.Errorf("无法检查配置文件 %q 的权限: %w", path, err)
	}

	data, err := io.ReadAll(io.LimitReader(file, maxConfigSize+1))
	if err != nil {
		return appConfig{}, fmt.Errorf("无法读取配置文件 %q: %w", path, err)
	}
	if len(data) > maxConfigSize {
		return appConfig{}, fmt.Errorf("配置文件 %q 超过 1 MiB 限制", path)
	}

	var config appConfig
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&config); err != nil {
		return appConfig{}, fmt.Errorf("配置文件 %q 不是有效 YAML: %w", path, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return appConfig{}, errors.New("配置文件只允许包含一个 YAML 文档")
		}
		return appConfig{}, fmt.Errorf("配置文件 %q 包含无效的尾随内容: %w", path, err)
	}
	config.configMode = info.Mode().Perm()
	config.insecurePermissions = runtime.GOOS != "windows" && config.configMode&0o077 != 0

	config.Listen = strings.TrimSpace(config.Listen)
	if config.Listen == "" {
		config.Listen = "127.0.0.1:8080"
	}
	config.GitHubToken = strings.TrimSpace(config.GitHubToken)
	config.Host = strings.TrimSpace(config.Host)
	config.APIKey = strings.TrimSpace(config.APIKey)
	config.LogLevel = strings.ToLower(strings.TrimSpace(config.LogLevel))
	if config.LogLevel == "" {
		config.LogLevel = "info"
	}
	if _, err := logLevel(config.LogLevel); err != nil {
		return appConfig{}, err
	}
	config.ModelsCacheTTL = copilot.DefaultModelsCacheTTL
	if value := strings.TrimSpace(config.ModelsCacheTTLValue); value != "" {
		ttl, err := time.ParseDuration(value)
		if err != nil || ttl <= 0 {
			return appConfig{}, fmt.Errorf("无效的 models_cache_ttl %q，必须是正的 Go duration（例如 2h）", value)
		}
		config.ModelsCacheTTL = ttl
	}
	if err := validateListen(config.Listen, config.APIKey); err != nil {
		return appConfig{}, err
	}

	credential := copilot.Credential{}
	switch {
	case config.GitHubToken != "":
		credential.Token = config.GitHubToken
	case strings.TrimSpace(config.Credentials) != "":
		credentialPath := resolveConfigPath(path, config.Credentials)
		credential, err = copilot.LoadCredential(credentialPath)
		if err != nil {
			return appConfig{}, fmt.Errorf("无法加载 credentials %q: %w", credentialPath, err)
		}
		if runtime.GOOS != "windows" {
			info, err := os.Stat(credentialPath)
			if err != nil {
				return appConfig{}, fmt.Errorf("无法检查 credentials %q 的权限: %w", credentialPath, err)
			}
			config.credentialFile = credentialPath
			config.credentialMode = info.Mode().Perm()
			config.insecureCredential = config.credentialMode&0o077 != 0
		}
	case strings.TrimSpace(config.EditorConfig) != "":
		editorPath := resolveConfigPath(path, config.EditorConfig)
		credential, err = copilot.DiscoverEditor(editorPath)
		if err != nil {
			return appConfig{}, fmt.Errorf("无法从 editor_config %q 读取 OAuth 凭证: %w", editorPath, err)
		}
	default:
		return appConfig{}, errors.New("必须设置 github_token、credentials 或 editor_config 之一")
	}
	if config.Host != "" {
		credential.Host = config.Host
	}
	config.Token = credential.Token
	config.Host, err = copilot.NormalizeHost(credential.Host)
	if err != nil {
		return appConfig{}, err
	}
	if _, err := copilot.New(copilot.Config{Token: config.Token, Host: config.Host, APIKey: config.APIKey}); err != nil {
		return appConfig{}, err
	}
	return config, nil
}

func resolveConfigPath(configFile, value string) string {
	path := filepath.Clean(value)
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(filepath.Dir(configFile), path)
}

func validateListen(address, apiKey string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("无效的 listen 地址 %q: %w", address, err)
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 0 || portNumber > 65535 {
		return fmt.Errorf("listen 端口必须是 0 到 65535 之间的数字: %q", port)
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	local := strings.EqualFold(host, "localhost") || (ip != nil && ip.IsLoopback())
	if !local && strings.TrimSpace(apiKey) == "" {
		return errors.New("非本机监听地址必须配置 api_key 服务访问密钥")
	}
	return nil
}

func logLevel(value string) (slog.Level, error) {
	switch value {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("无效的 log_level %q，必须是 debug、info、warn 或 error", value)
	}
}

func newLogger(level string, output io.Writer) (*slog.Logger, error) {
	minimum, err := logLevel(level)
	if err != nil {
		return nil, err
	}
	return slog.New(slog.NewJSONHandler(output, &slog.HandlerOptions{Level: minimum})), nil
}
