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
const (
	defaultLogMaxSizeMB = 10
	defaultLogBackups   = 5
	maxLogMaxSizeMB     = 1 << 20
	maxLogBackups       = 100
)

type appConfig struct {
	Listen              string        `yaml:"listen"`
	DataPath            string        `yaml:"data_path"`
	LogsPath            string        `yaml:"logs_path"`
	Host                string        `yaml:"host"`
	APIKey              string        `yaml:"api_key"`
	ManagementKey       string        `yaml:"management_key"` // 为空时禁用 Web 控制台与管理接口
	LogLevel            string        `yaml:"log_level"`
	ModelsCacheTTLValue string        `yaml:"models_cache_ttl"`
	ModelsCacheTTL      time.Duration `yaml:"-"`
	// SessionAffinityValue 保留原值以区分缺省（默认开启）与显式关闭。
	SessionAffinityValue    *bool         `yaml:"session_affinity"`
	SessionAffinityTTLValue string        `yaml:"session_affinity_ttl"`
	SessionAffinity         bool          `yaml:"-"`
	SessionAffinityTTL      time.Duration `yaml:"-"`
	LogMaxSizeMBValue       *string       `yaml:"log_max_size_mb"` // 保留原值以便报告字段级校验错误。
	LogMaxBackupsValue      *string       `yaml:"log_max_backups"` // 显式解析以区分缺省值和非法输入。
	LogMaxSizeMB            int64         `yaml:"-"`
	LogMaxBackups           int           `yaml:"-"`

	CacheDir            string      `yaml:"-"`
	configMode          os.FileMode `yaml:"-"`
	insecurePermissions bool        `yaml:"-"`
	// Accounts 按凭证文件名排序，每项对应 credentials 目录中的一个 GitHub Copilot 账号。
	Accounts []accountConfig `yaml:"-"`
}

// accountConfig 是一个 GitHub Copilot 账号的凭证及其来源文件信息。
type accountConfig struct {
	Token              string
	Host               string // 规范化后的 host
	User               string // 凭证文件名去掉 .json 后的 GitHub 用户名，保留原有大小写
	Disabled           bool   // 凭证文件中的 disabled 字段；为 true 时启动后不参与选择
	Priority           int    // 凭证文件中的 priority 字段；省略时为 0，数值越大越优先被选择
	credentialFile     string
	credentialMode     os.FileMode
	insecureCredential bool
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
	config.DataPath = strings.TrimSpace(config.DataPath)
	if config.DataPath == "" {
		config.DataPath = "./data"
	}
	config.DataPath = resolveConfigPath(path, config.DataPath)
	config.CacheDir = filepath.Join(config.DataPath, "cache")
	config.LogsPath = strings.TrimSpace(config.LogsPath)
	if config.LogsPath == "" {
		config.LogsPath = "./logs"
	}
	config.LogsPath = resolveConfigPath(path, config.LogsPath)
	config.Host = strings.TrimSpace(config.Host)
	config.APIKey = strings.TrimSpace(config.APIKey)
	config.ManagementKey = strings.TrimSpace(config.ManagementKey)
	if config.ManagementKey != "" && config.ManagementKey == config.APIKey {
		return appConfig{}, errors.New("management_key 必须与 api_key 不同")
	}
	config.SessionAffinity = true
	if config.SessionAffinityValue != nil {
		config.SessionAffinity = *config.SessionAffinityValue
	}
	config.SessionAffinityTTL = copilot.DefaultSessionAffinityTTL
	if value := strings.TrimSpace(config.SessionAffinityTTLValue); value != "" {
		ttl, err := time.ParseDuration(value)
		if err != nil || ttl <= 0 {
			return appConfig{}, fmt.Errorf("无效的 session_affinity_ttl %q，必须是大于 0 的 Go duration（例如 2h）", value)
		}
		config.SessionAffinityTTL = ttl
	}
	config.LogLevel = strings.ToLower(strings.TrimSpace(config.LogLevel))
	if config.LogLevel == "" {
		config.LogLevel = "info"
	}
	if _, err := logLevel(config.LogLevel); err != nil {
		return appConfig{}, err
	}
	if config.LogMaxSizeMBValue == nil {
		config.LogMaxSizeMB = defaultLogMaxSizeMB
	} else {
		value, err := strconv.ParseInt(strings.TrimSpace(*config.LogMaxSizeMBValue), 10, 64)
		if err != nil {
			return appConfig{}, fmt.Errorf("无效的 log_max_size_mb %q", *config.LogMaxSizeMBValue)
		}
		config.LogMaxSizeMB = value
		if config.LogMaxSizeMB < 1 || config.LogMaxSizeMB > maxLogMaxSizeMB {
			return appConfig{}, fmt.Errorf("无效的 log_max_size_mb %d，必须是 1 到 %d 之间的整数",
				config.LogMaxSizeMB, maxLogMaxSizeMB)
		}
	}
	if config.LogMaxBackupsValue == nil {
		config.LogMaxBackups = defaultLogBackups
	} else {
		value, err := strconv.Atoi(strings.TrimSpace(*config.LogMaxBackupsValue))
		if err != nil {
			return appConfig{}, fmt.Errorf("无效的 log_max_backups %q", *config.LogMaxBackupsValue)
		}
		config.LogMaxBackups = value
		if config.LogMaxBackups < 0 || config.LogMaxBackups > maxLogBackups {
			return appConfig{}, fmt.Errorf("无效的 log_max_backups %d，必须是 0 到 %d 之间的整数",
				config.LogMaxBackups, maxLogBackups)
		}
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

	credentialsDir := filepath.Join(config.DataPath, credentialsDirName)
	// 启用管理接口时可以从零个账号开始，因此凭证目录不存在时自动创建，并允许没有凭证文件。
	allowEmpty := config.ManagementKey != ""
	if allowEmpty {
		if err := os.MkdirAll(credentialsDir, 0o700); err != nil {
			return appConfig{}, fmt.Errorf("无法创建 credentials 目录 %q: %w", credentialsDir, err)
		}
	}
	config.Accounts, err = loadAccounts(credentialsDir, config.Host, allowEmpty)
	if err != nil {
		return appConfig{}, err
	}
	// 启动前用与 serve 相同的配置校验账号，确保 loadConfig 通过即可创建账号池。
	if _, err := copilot.NewManagedPool(config.poolConfig(nil), config.poolAccounts()); err != nil {
		return appConfig{}, err
	}
	return config, nil
}

// poolConfig 生成账号池的服务级配置；账号相关的设置由 poolAccounts 提供。
// 关闭会话亲和时 SessionAffinityTTL 为 0。
func (c appConfig) poolConfig(logger *slog.Logger) copilot.PoolConfig {
	affinityTTL := c.SessionAffinityTTL
	if !c.SessionAffinity {
		affinityTTL = 0
	}
	return copilot.PoolConfig{
		APIKey: c.APIKey, ManagementKey: c.ManagementKey,
		CredentialsDir: filepath.Join(c.DataPath, credentialsDirName), CacheDir: c.CacheDir,
		Host: c.Host, ModelsCacheTTL: c.ModelsCacheTTL, SessionAffinityTTL: affinityTTL,
		Logger: logger,
	}
}

// poolAccounts 把已加载的账号转换为账号池的输入，保持凭证文件名的顺序。
func (c appConfig) poolAccounts() []copilot.PoolAccount {
	accounts := make([]copilot.PoolAccount, 0, len(c.Accounts))
	for _, account := range c.Accounts {
		accounts = append(accounts, copilot.PoolAccount{
			Token: account.Token, Host: account.Host, User: account.User, Disabled: account.Disabled,
			Priority: account.Priority,
		})
	}
	return accounts
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
