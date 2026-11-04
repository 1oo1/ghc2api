package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"sync"

	"ghc2api/internal/copilot"
)

// errEmptyManagementKey 是控制台保存的配置清空 management_key 时的错误：清空会使控制台与管理接口不可用。
const errEmptyManagementKey = "不能通过控制台清空 management_key（会导致控制台与管理接口不可用）"

// yamlErrorLine 从错误信息中提取行号（yaml.v3 的错误形如 "yaml: line 3: ..."）。
var yamlErrorLine = regexp.MustCompile(`line (\d+)`)

// configField 描述一个配置项：hot 表示保存后立即生效，否则需重启；differs 比较解析后的值。
type configField struct {
	name    string
	hot     bool
	differs func(a, b appConfig) bool
}

// configFields 按固定顺序列出全部配置项，changed 与 restart_required 都按此顺序输出
// （热更新与需重启两类各自的相对顺序即文档中的字段顺序）。
var configFields = []configField{
	{"listen", false, func(a, b appConfig) bool { return a.Listen != b.Listen }},
	{"data_path", false, func(a, b appConfig) bool { return a.DataPath != b.DataPath }},
	{"logs_path", false, func(a, b appConfig) bool { return a.LogsPath != b.LogsPath }},
	{"host", false, func(a, b appConfig) bool { return a.Host != b.Host }},
	{"api_key", true, func(a, b appConfig) bool { return a.APIKey != b.APIKey }},
	{"management_key", true, func(a, b appConfig) bool { return a.ManagementKey != b.ManagementKey }},
	{"log_level", true, func(a, b appConfig) bool { return a.LogLevel != b.LogLevel }},
	{"log_to_stderr", false, func(a, b appConfig) bool { return a.LogFileOnly != b.LogFileOnly }},
	{"models_cache_ttl", true, func(a, b appConfig) bool { return a.ModelsCacheTTL != b.ModelsCacheTTL }},
	{"quota_refresh_interval", true, func(a, b appConfig) bool { return a.QuotaRefreshInterval != b.QuotaRefreshInterval }},
	{"log_max_size_mb", false, func(a, b appConfig) bool { return a.LogMaxSizeMB != b.LogMaxSizeMB }},
	{"log_max_backups", false, func(a, b appConfig) bool { return a.LogMaxBackups != b.LogMaxBackups }},
	{"session_affinity", false, func(a, b appConfig) bool { return a.SessionAffinity != b.SessionAffinity }},
	{"session_affinity_ttl", false, func(a, b appConfig) bool { return a.SessionAffinityTTL != b.SessionAffinityTTL }},
	{"session_affinity_max_per_account", true, func(a, b appConfig) bool {
		return a.SessionAffinityMaxPerAccount != b.SessionAffinityMaxPerAccount
	}},
	{"account_cooldown", true, func(a, b appConfig) bool { return a.AccountCooldown != b.AccountCooldown }},
	{"max_concurrent_requests", true, func(a, b appConfig) bool { return a.MaxConcurrentRequests != b.MaxConcurrentRequests }},
}

// changedFields 返回 hot 类别中 a 与 b 取值不同的字段名；结果永不为 nil。
func changedFields(hot bool, a, b appConfig) []string {
	names := []string{}
	for _, field := range configFields {
		if field.hot == hot && field.differs(a, b) {
			names = append(names, field.name)
		}
	}
	return names
}

// runtimeSettings 由配置生成可热更新的运行时设置。
func runtimeSettings(c appConfig) copilot.RuntimeSettings {
	return copilot.RuntimeSettings{
		APIKey: c.APIKey, ManagementKey: c.ManagementKey,
		ModelsCacheTTL: c.ModelsCacheTTL, QuotaRefreshInterval: c.QuotaRefreshInterval,
		AccountCooldown: c.AccountCooldown, SessionAffinityMaxPerAccount: c.SessionAffinityMaxPerAccount,
		MaxConcurrentRequests: c.MaxConcurrentRequests,
	}
}

// configFile 实现 copilot.ConfigStore：读取、校验、原地写入配置文件，并维护已生效的热更新字段。
// 需重启字段永远与启动值（startup）比较；热更新字段与已生效值（running）比较，保存成功后更新 running。
type configFile struct {
	path    string
	mu      sync.Mutex // 串行化 Save，并保护 running
	startup appConfig  // 进程启动时的配置，只读
	running appConfig  // 热更新字段为当前已生效值
	level   *slog.LevelVar
}

// newConfigFile 创建配置文件存储；level 为日志器的级别，保存后随 log_level 更新，可为 nil。
func newConfigFile(path string, startup appConfig, level *slog.LevelVar) *configFile {
	return &configFile{path: path, startup: startup, running: startup, level: level}
}

// configRevision 是文件内容的版本：SHA-256 的小写十六进制。
func configRevision(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Load 读取配置文件。磁盘内容无法解析时 RestartRequired 为空。
func (f *configFile) Load() (copilot.ConfigSnapshot, error) {
	data, err := os.ReadFile(f.path)
	if err != nil {
		return copilot.ConfigSnapshot{}, fmt.Errorf("无法读取配置文件 %q: %w", f.path, err)
	}
	snapshot := copilot.ConfigSnapshot{
		Path: f.path, Content: string(data), Revision: configRevision(data), Writable: f.writable(),
		RestartRequired: []string{},
	}
	if config, err := parseConfig(f.path, data); err == nil {
		snapshot.RestartRequired = changedFields(false, f.startup, config)
	}
	return snapshot, nil
}

// writable 报告配置文件能否以写方式打开（不截断）。
func (f *configFile) writable() bool {
	file, err := os.OpenFile(f.path, os.O_WRONLY, 0)
	if err != nil {
		return false
	}
	_ = file.Close()
	return true
}

// check 用与启动相同的规则校验内容，并拒绝清空 management_key。
func (f *configFile) check(content string) (appConfig, error) {
	config, err := parseConfig(f.path, []byte(content))
	if err != nil {
		return appConfig{}, err
	}
	if config.ManagementKey == "" {
		return appConfig{}, errors.New(errEmptyManagementKey)
	}
	return config, nil
}

// Validate 校验配置内容，不写文件。无效时 Line 取错误信息中的第一个行号，没有则为 0。
func (f *configFile) Validate(content string) copilot.ConfigValidation {
	config, err := f.check(content)
	if err != nil {
		result := copilot.ConfigValidation{Error: err.Error(), RestartRequired: []string{}}
		if match := yamlErrorLine.FindStringSubmatch(result.Error); match != nil {
			result.Line, _ = strconv.Atoi(match[1])
		}
		return result
	}
	return copilot.ConfigValidation{Valid: true, RestartRequired: changedFields(false, f.startup, config)}
}

// writeConfigContent 是 Save 写配置文件的唯一入口，默认原地写入；包级变量以便测试注入写入失败。
var writeConfigContent = writeInPlace

// Save 在 revision 与磁盘文件一致且内容有效时原地写入文件，并更新已生效的热更新字段与日志级别。
// 原地写入（不创建、不 rename）以兼容 Docker 单文件 bind mount 与符号链接，并保留属主与权限。
// 写入失败时尽力恢复原内容，且不更新已生效值。
func (f *configFile) Save(content, revision string) (copilot.ConfigSaveResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	current, err := os.ReadFile(f.path)
	if err != nil {
		return copilot.ConfigSaveResult{}, fmt.Errorf("无法读取配置文件 %q: %w", f.path, err)
	}
	if revision != configRevision(current) {
		return copilot.ConfigSaveResult{}, copilot.ErrConfigConflict
	}
	config, err := f.check(content)
	if err != nil {
		return copilot.ConfigSaveResult{}, fmt.Errorf("%w：%s", copilot.ErrConfigInvalid, err.Error())
	}
	if err := writeConfigContent(f.path, []byte(content)); err != nil {
		// 原地写入先截断再写，失败可能留下被截断的文件：尽力写回开头读到的原内容。
		if restoreErr := writeConfigContent(f.path, current); restoreErr != nil {
			return copilot.ConfigSaveResult{}, fmt.Errorf("写入配置文件失败: %w；恢复原内容也失败: %v", err, restoreErr)
		}
		return copilot.ConfigSaveResult{}, fmt.Errorf("写入配置文件失败: %w", err)
	}
	result := copilot.ConfigSaveResult{
		Revision:        configRevision([]byte(content)),
		Changed:         changedFields(true, f.running, config),
		RestartRequired: changedFields(false, f.startup, config),
		Settings:        runtimeSettings(config),
	}
	f.running = config
	if f.level != nil {
		if level, err := logLevel(config.LogLevel); err == nil {
			f.level.Set(level)
		}
	}
	return result, nil
}

// writeInPlace 截断并写入已存在的文件，写完后 Sync。
func writeInPlace(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}
