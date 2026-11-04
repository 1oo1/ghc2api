package main

// 本文件覆盖配置文件存储（configfile.go）与 parseConfig：
//   - parseConfig 与 loadConfig 的解析规则一致且不访问文件系统；
//   - configFile 的 Load / Validate / Save：revision、可写性、字段分类（热更新 changed 与需重启 restart_required）、
//     原地写入（保留注释、权限、符号链接）、日志级别热更新；
//   - 端到端：run 启动带 management_key 的服务后，经管理接口读取与保存配置，api_key 立即切换。

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"ghc2api/internal/copilot"
)

const (
	// 满足最小长度（32 字节）的另一组密钥，用于验证 api_key 与 management_key 的切换。
	cfgNewAPIKey        = "cfg-new-api-key-0123456789-abcdefghij"
	cfgNewManagementKey = "cfg-new-management-key-0123456789-abcdef"
)

// cfgBase 是最小的有效配置：包含 api_key 与非空 management_key（控制台编辑要求后者非空）。
var cfgBase = apiKeySetting + "management_key: " + strconv.Quote(validManagementKey) + "\n"

// cfgRevision 计算内容的 revision：SHA-256 的小写十六进制。
func cfgRevision(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// newCfgFile 把 content 写入临时配置文件，并以其解析结果作为启动配置创建 configFile。
func newCfgFile(t *testing.T, content string) (*configFile, string, *slog.LevelVar) {
	t.Helper()
	path := writeConfigFile(t, content)
	startup, err := parseConfig(path, []byte(content))
	if err != nil {
		t.Fatalf("启动配置应有效: %v", err)
	}
	level := new(slog.LevelVar)
	return newConfigFile(path, startup, level), path, level
}

// cfgReadFile 读取文件全部内容。
func cfgReadFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// cfgSkipIfCannotDenyWrite 在 root 或 Windows 下跳过：这些环境里 chmod 0400 不能阻止写入。
func cfgSkipIfCannotDenyWrite(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("当前环境下 chmod 0400 无法使文件不可写")
	}
}

// TestParseConfigReturnsNormalizedValues parseConfig 返回规范化后的值，相对路径按配置文件所在目录解析，且不访问文件系统。
func TestParseConfigReturnsNormalizedValues(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing")
	path := filepath.Join(dir, "config.yaml")
	content := cfgBase + "listen: \" 127.0.0.1:9999 \"\ndata_path: ./d\nsession_affinity_ttl: 120m\nlog_level: DEBUG\n"
	config, err := parseConfig(path, []byte(content))
	if err != nil {
		t.Fatalf("parseConfig() error = %v", err)
	}
	if config.Listen != "127.0.0.1:9999" || config.LogLevel != "debug" ||
		config.SessionAffinityTTL != 2*time.Hour || config.APIKey != validAPIKey || config.ManagementKey != validManagementKey ||
		config.DataPath != filepath.Join(dir, "d") || config.CacheDir != filepath.Join(dir, "d", "cache") {
		t.Errorf("规范化结果不符: %+v", config)
	}
	if config.MaxConcurrentRequests != defaultMaxConcurrentRequests || !config.SessionAffinity {
		t.Errorf("缺省值不符: max_concurrent_requests=%d session_affinity=%v", config.MaxConcurrentRequests, config.SessionAffinity)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("parseConfig 不应访问或创建文件系统路径，Stat(%q) err = %v", dir, err)
	}
}

// TestParseConfigErrorsMatchLoadConfig 各类无效配置下 parseConfig 与 loadConfig 报告相同的错误信息。
func TestParseConfigErrorsMatchLoadConfig(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		want    string // 错误信息应包含的片段
	}{
		{name: "非法 YAML", content: apiKeySetting + "listen: [x\n", want: "不是有效 YAML"},
		{name: "未知字段", content: apiKeySetting + "foo: bar\n", want: "foo"},
		{name: "多个文档", content: apiKeySetting + "---\nlog_level: info\n", want: "只允许包含一个 YAML 文档"},
		{name: "缺少 api_key", content: "log_level: info\n", want: "api_key 必填"},
		{name: "api_key 过短", content: "api_key: short\n", want: "api_key 过短"},
		{name: "management_key 过短", content: apiKeySetting + "management_key: short\n", want: "management_key 过短"},
		{name: "两个密钥相同", content: apiKeySetting + "management_key: " + strconv.Quote(validAPIKey) + "\n", want: "必须与 api_key 不同"},
		{name: "非法 listen", content: apiKeySetting + "listen: nope\n", want: "无效的 listen 地址"},
		{name: "非法 log_level", content: apiKeySetting + "log_level: loud\n", want: "无效的 log_level"},
		{name: "非法 account_cooldown", content: apiKeySetting + "account_cooldown: 0s\n", want: "无效的 account_cooldown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfigFile(t, tc.content)
			_, loadErr := loadConfig(path)
			_, parseErr := parseConfig(path, []byte(tc.content))
			if loadErr == nil || parseErr == nil {
				t.Fatalf("loadConfig err = %v, parseConfig err = %v, want both non-nil", loadErr, parseErr)
			}
			if parseErr.Error() != loadErr.Error() {
				t.Errorf("错误信息不一致:\nparseConfig: %v\nloadConfig:  %v", parseErr, loadErr)
			}
			if !strings.Contains(parseErr.Error(), tc.want) {
				t.Errorf("错误 %q 应包含 %q", parseErr, tc.want)
			}
		})
	}
}

// TestParseConfigRejectsOversizeContent 超过 1 MiB 返回现有的「超过 1 MiB 限制」错误。
func TestParseConfigRejectsOversizeContent(t *testing.T) {
	content := cfgBase + "# " + strings.Repeat("x", maxConfigSize) + "\n"
	if _, err := parseConfig("config.yaml", []byte(content)); err == nil || !strings.Contains(err.Error(), "超过 1 MiB 限制") {
		t.Fatalf("parseConfig() error = %v, want 超过 1 MiB 限制", err)
	}
}

// TestConfigFileLoad Load 返回文件内容、64 位小写十六进制 revision、构造时的路径；可写文件 writable 为 true。
func TestConfigFileLoad(t *testing.T) {
	content := "# 注释\n" + cfgBase
	f, path, _ := newCfgFile(t, content)
	snap, err := f.Load()
	if err != nil {
		t.Fatal(err)
	}
	if snap.Content != content || snap.Path != path || !snap.Writable {
		t.Errorf("snapshot = %+v", snap)
	}
	if snap.Revision != cfgRevision(content) || len(snap.Revision) != 64 || snap.Revision != strings.ToLower(snap.Revision) {
		t.Errorf("revision = %q, want %q", snap.Revision, cfgRevision(content))
	}
	if !reflect.DeepEqual(snap.RestartRequired, []string{}) {
		t.Errorf("restart_required = %#v, want 空切片", snap.RestartRequired)
	}
	// 文件被外部修改后 revision 随之变化。
	if err := os.WriteFile(path, []byte(content+"# more\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if again, _ := f.Load(); again.Revision == snap.Revision {
		t.Error("内容变化后 revision 应变化")
	}
}

// TestConfigFileLoadReadOnly 文件不可写时 writable 为 false，内容仍可读取。
func TestConfigFileLoadReadOnly(t *testing.T) {
	cfgSkipIfCannotDenyWrite(t)
	f, path, _ := newCfgFile(t, cfgBase)
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	snap, err := f.Load()
	if err != nil {
		t.Fatal(err)
	}
	if snap.Writable || snap.Content != cfgBase {
		t.Errorf("writable = %v content = %q, want false 与原内容", snap.Writable, snap.Content)
	}
}

// TestConfigFileLoadReportsReadError 文件无法读取时返回错误。
func TestConfigFileLoadReportsReadError(t *testing.T) {
	f, path, _ := newCfgFile(t, cfgBase)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Load(); err == nil {
		t.Fatal("Load() error = nil, want 读取失败")
	}
}

// cfgAllRestartYAML 修改了全部需重启字段（取值都不同于缺省值）。
const cfgAllRestartYAML = "listen: 127.0.0.1:18080\ndata_path: ./other-data\nlogs_path: ./other-logs\nhost: example.ghe.com\n" +
	"log_to_stderr: false\nlog_max_size_mb: 3\nlog_max_backups: 2\nsession_affinity: false\nsession_affinity_ttl: 30m\n"

// cfgRestartOrder 是规格规定的 restart_required 输出顺序。
var cfgRestartOrder = []string{"listen", "data_path", "logs_path", "host", "log_to_stderr",
	"log_max_size_mb", "log_max_backups", "session_affinity", "session_affinity_ttl"}

// TestConfigFileLoadRestartRequired restart_required 是磁盘内容中与启动值不同的需重启字段（按固定顺序）；
// 热更新字段的变化不计入；磁盘内容无效时为空切片且 JSON 输出 []。
func TestConfigFileLoadRestartRequired(t *testing.T) {
	f, path, _ := newCfgFile(t, cfgBase)

	if err := os.WriteFile(path, []byte(cfgBase+cfgAllRestartYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	snap, err := f.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(snap.RestartRequired, cfgRestartOrder) {
		t.Errorf("restart_required = %v, want %v", snap.RestartRequired, cfgRestartOrder)
	}

	// 只改热更新字段与注释：不需要重启。
	hot := cfgBase + "# 注释\nlog_level: debug\nmax_concurrent_requests: 3\n"
	if err := os.WriteFile(path, []byte(hot), 0o600); err != nil {
		t.Fatal(err)
	}
	if snap, _ := f.Load(); !reflect.DeepEqual(snap.RestartRequired, []string{}) {
		t.Errorf("仅热更新字段变化时 restart_required = %#v, want 空切片", snap.RestartRequired)
	}

	// 磁盘内容无效：Load 仍成功，restart_required 为空且 JSON 为 []。
	if err := os.WriteFile(path, []byte("listen: [x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	snap, err = f.Load()
	if err != nil {
		t.Fatalf("磁盘内容无效时 Load() 仍应成功: %v", err)
	}
	if !reflect.DeepEqual(snap.RestartRequired, []string{}) {
		t.Errorf("无效内容的 restart_required = %#v, want 空切片", snap.RestartRequired)
	}
	data, _ := json.Marshal(snap)
	if !strings.Contains(string(data), `"restart_required":[]`) {
		t.Errorf("JSON 应输出 restart_required:[]: %s", data)
	}
}

// TestConfigFileValidate 校验结果：有效时带 restart_required；无效时带错误、行号（提取不到为 0）与空的 restart_required。
func TestConfigFileValidate(t *testing.T) {
	startup := cfgBase + "session_affinity_ttl: 120m\n"
	f, _, _ := newCfgFile(t, startup)
	const emptyMgmt = "不能通过控制台清空 management_key（会导致控制台与管理接口不可用）"

	for _, tc := range []struct {
		name        string
		content     string
		wantValid   bool
		wantRestart []string
		wantErr     string // 错误信息应包含的片段，或完全等于 emptyMgmt
		wantLine    int
	}{
		{name: "有效且改了需重启字段", content: startup + "listen: 127.0.0.1:18080\nhost: example.ghe.com\n",
			wantValid: true, wantRestart: []string{"listen", "host"}},
		{name: "规范化等价值不算变化", content: cfgBase + "session_affinity_ttl: 2h\n", wantValid: true, wantRestart: []string{}},
		{name: "真正的变化", content: cfgBase + "session_affinity_ttl: 3h\n", wantValid: true, wantRestart: []string{"session_affinity_ttl"}},
		{name: "只改注释", content: "# 新注释\n" + startup, wantValid: true, wantRestart: []string{}},
		{name: "YAML 语法错误带行号", content: cfgBase + "b: c\n  d: e\n", wantErr: "yaml: line 4:", wantLine: 4},
		{name: "未知字段带行号", content: cfgBase + "\nfoo: bar\n", wantErr: "foo", wantLine: 4},
		{name: "api_key 过短无行号", content: "api_key: short\nmanagement_key: " + strconv.Quote(validManagementKey) + "\n",
			wantErr: "api_key 过短"},
		{name: "management_key 为空串", content: apiKeySetting + "management_key: \"\"\n", wantErr: emptyMgmt},
		{name: "未设置 management_key", content: apiKeySetting, wantErr: emptyMgmt},
		{name: "management_key 仅空白", content: apiKeySetting + "management_key: \"   \"\n", wantErr: emptyMgmt},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := f.Validate(tc.content)
			if got.Valid != tc.wantValid {
				t.Fatalf("Valid = %v, want %v; %+v", got.Valid, tc.wantValid, got)
			}
			if got.RestartRequired == nil {
				t.Error("RestartRequired 不应为 nil")
			}
			if tc.wantValid {
				if got.Error != "" || got.Line != 0 || !reflect.DeepEqual(got.RestartRequired, tc.wantRestart) {
					t.Errorf("有效结果 = %+v, want restart_required %v", got, tc.wantRestart)
				}
				return
			}
			if tc.wantErr == emptyMgmt {
				if got.Error != emptyMgmt {
					t.Errorf("Error = %q, want %q", got.Error, emptyMgmt)
				}
			} else if !strings.Contains(got.Error, tc.wantErr) {
				t.Errorf("Error = %q, want 包含 %q", got.Error, tc.wantErr)
			}
			if got.Line != tc.wantLine || len(got.RestartRequired) != 0 {
				t.Errorf("Line = %d restart_required = %v, want %d 与空", got.Line, got.RestartRequired, tc.wantLine)
			}
		})
	}
}

// TestConfigFileSaveRejectsWithoutChangingFile 版本冲突与无效内容都不写文件、不改变已生效值与日志级别。
func TestConfigFileSaveRejectsWithoutChangingFile(t *testing.T) {
	f, path, level := newCfgFile(t, cfgBase)
	snap, _ := f.Load()

	t.Run("revision 不匹配", func(t *testing.T) {
		_, err := f.Save(cfgBase+"log_level: debug\n", "0000")
		if !errors.Is(err, copilot.ErrConfigConflict) {
			t.Fatalf("Save() error = %v, want ErrConfigConflict", err)
		}
	})
	t.Run("文件被外部修改后旧 revision 冲突", func(t *testing.T) {
		external := cfgBase + "# 外部修改\n"
		if err := os.WriteFile(path, []byte(external), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := f.Save(cfgBase+"log_level: debug\n", snap.Revision)
		if !errors.Is(err, copilot.ErrConfigConflict) {
			t.Fatalf("Save() error = %v, want ErrConfigConflict", err)
		}
		if got := cfgReadFile(t, path); got != external {
			t.Errorf("冲突时不应改写文件: %q", got)
		}
		if err := os.WriteFile(path, []byte(cfgBase), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	for name, content := range map[string]string{
		"YAML 无效":           cfgBase + "listen: [x\n",
		"api_key 过短":        "api_key: short\nmanagement_key: " + strconv.Quote(validManagementKey) + "\n",
		"management_key 为空": apiKeySetting + "management_key: \"\"\nlog_level: debug\n",
	} {
		t.Run(name, func(t *testing.T) {
			before := cfgReadFile(t, path)
			cur, _ := f.Load()
			_, err := f.Save(content, cur.Revision)
			if !errors.Is(err, copilot.ErrConfigInvalid) {
				t.Fatalf("Save() error = %v, want ErrConfigInvalid", err)
			}
			if name == "management_key 为空" && !strings.Contains(err.Error(), "不能通过控制台清空 management_key") {
				t.Errorf("错误信息 = %q", err)
			}
			if got := cfgReadFile(t, path); got != before {
				t.Errorf("无效内容不应写入文件: %q", got)
			}
		})
	}
	if level.Level() != slog.LevelInfo {
		t.Errorf("被拒绝的保存不应改变日志级别，当前 %v", level.Level())
	}
}

// TestConfigFileSaveWritesInPlace 成功保存：文件内容原样写入（含注释与引号）、权限不变、返回新 revision；
// Changed 按固定顺序列出全部热更新字段，Settings 为新值，日志级别被设置；需重启字段进入 RestartRequired 而不进入 Changed。
func TestConfigFileSaveWritesInPlace(t *testing.T) {
	f, path, level := newCfgFile(t, cfgBase)
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0o640); err != nil {
			t.Fatal(err)
		}
	}
	content := "# 控制台保存\n" +
		"api_key: '" + cfgNewAPIKey + "'   # 新密钥\n" +
		"management_key: " + strconv.Quote(cfgNewManagementKey) + "\n" +
		"listen: 127.0.0.1:18080\n" +
		"log_level: debug\n" +
		"models_cache_ttl: 1h\n" +
		"quota_refresh_interval: 30m\n" +
		"session_affinity_max_per_account: 7\n" +
		"account_cooldown: 45m\n" +
		"max_concurrent_requests: 9\n"
	snap, _ := f.Load()
	res, err := f.Save(content, snap.Revision)
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if got := cfgReadFile(t, path); got != content {
		t.Errorf("文件内容应原样写入:\n%q", got)
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(path)
		if info.Mode().Perm() != 0o640 {
			t.Errorf("文件权限 = %v, want 0640", info.Mode().Perm())
		}
	}
	if res.Revision != cfgRevision(content) {
		t.Errorf("Revision = %q, want %q", res.Revision, cfgRevision(content))
	}
	wantChanged := []string{"api_key", "management_key", "log_level", "models_cache_ttl", "quota_refresh_interval",
		"session_affinity_max_per_account", "account_cooldown", "max_concurrent_requests"}
	if !reflect.DeepEqual(res.Changed, wantChanged) {
		t.Errorf("Changed = %v, want %v", res.Changed, wantChanged)
	}
	if !reflect.DeepEqual(res.RestartRequired, []string{"listen"}) {
		t.Errorf("RestartRequired = %v, want [listen]", res.RestartRequired)
	}
	wantSettings := copilot.RuntimeSettings{
		APIKey: cfgNewAPIKey, ManagementKey: cfgNewManagementKey, ModelsCacheTTL: time.Hour,
		QuotaRefreshInterval: 30 * time.Minute, AccountCooldown: 45 * time.Minute,
		SessionAffinityMaxPerAccount: 7, MaxConcurrentRequests: 9,
	}
	if res.Settings != wantSettings {
		t.Errorf("Settings = %+v, want %+v", res.Settings, wantSettings)
	}
	if level.Level() != slog.LevelDebug {
		t.Errorf("日志级别 = %v, want debug", level.Level())
	}
	if again, _ := f.Load(); again.Revision != res.Revision || again.Content != content ||
		!reflect.DeepEqual(again.RestartRequired, []string{"listen"}) {
		t.Errorf("保存后 Load = %+v", again)
	}
}

// TestConfigFileSaveOnlyCommentChanged 只改注释：Changed 与 RestartRequired 都是空切片（非 nil），文件仍被写入。
func TestConfigFileSaveOnlyCommentChanged(t *testing.T) {
	f, path, _ := newCfgFile(t, cfgBase)
	snap, _ := f.Load()
	content := "# 只是注释\n" + cfgBase
	res, err := f.Save(content, snap.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.Changed, []string{}) || !reflect.DeepEqual(res.RestartRequired, []string{}) {
		t.Errorf("Changed = %#v RestartRequired = %#v, want 都是空切片", res.Changed, res.RestartRequired)
	}
	if cfgReadFile(t, path) != content {
		t.Error("注释变化也应写入文件")
	}
}

// TestConfigFileSaveWritesThroughSymlink 配置路径是符号链接时原地写入：链接仍在，目标文件被更新。
func TestConfigFileSaveWritesThroughSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 创建符号链接需要额外权限")
	}
	dir := t.TempDir()
	target := writeConfigAt(t, dir, "real.yaml", cfgBase)
	link := filepath.Join(dir, "config.yaml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	startup, err := parseConfig(link, []byte(cfgBase))
	if err != nil {
		t.Fatal(err)
	}
	f := newConfigFile(link, startup, nil)
	snap, err := f.Load()
	if err != nil {
		t.Fatal(err)
	}
	content := cfgBase + "log_level: warn\n"
	if _, err := f.Save(content, snap.Revision); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("符号链接应保留: info=%v err=%v", info, err)
	}
	if got := cfgReadFile(t, target); got != content {
		t.Errorf("目标文件内容 = %q, want %q", got, content)
	}
}

// TestConfigFileSaveSequenceTracksRunningValues 连续保存：Changed 相对“上次已生效值”，RestartRequired 始终相对启动值。
func TestConfigFileSaveSequenceTracksRunningValues(t *testing.T) {
	f, _, level := newCfgFile(t, cfgBase)
	rev := func() string { snap, _ := f.Load(); return snap.Revision }

	// api_key 已在 cfgBase 中出现，重复键会被 YAML 拒绝，因此 step1 重写 api_key 而不是追加。
	step1 := "api_key: " + strconv.Quote(cfgNewAPIKey) + "\nmanagement_key: " + strconv.Quote(validManagementKey) + "\nlisten: 127.0.0.1:18080\n"
	step2 := step1 + "log_level: debug\n"
	step3 := cfgBase + "log_level: debug\n" // api_key 与 listen 回到启动值

	for i, tc := range []struct {
		content     string
		wantChanged []string
		wantRestart []string
	}{
		{step1, []string{"api_key"}, []string{"listen"}},
		{step2, []string{"log_level"}, []string{"listen"}}, // api_key 已生效，不再算变化；listen 仍不同于启动值
		{step3, []string{"api_key"}, []string{}},           // log_level 已是 debug；api_key 回到启动值；listen 回到启动值
	} {
		res, err := f.Save(tc.content, rev())
		if err != nil {
			t.Fatalf("第 %d 次 Save() error = %v", i+1, err)
		}
		if !reflect.DeepEqual(res.Changed, tc.wantChanged) || !reflect.DeepEqual(res.RestartRequired, tc.wantRestart) {
			t.Errorf("第 %d 次: Changed = %#v RestartRequired = %#v, want %v / %v",
				i+1, res.Changed, res.RestartRequired, tc.wantChanged, tc.wantRestart)
		}
	}
	if level.Level() != slog.LevelDebug {
		t.Errorf("日志级别 = %v, want debug", level.Level())
	}
}

// TestConfigFileSaveWithNilLevel level 为 nil 时跳过日志级别设置，不 panic。
func TestConfigFileSaveWithNilLevel(t *testing.T) {
	path := writeConfigFile(t, cfgBase)
	startup, err := parseConfig(path, []byte(cfgBase))
	if err != nil {
		t.Fatal(err)
	}
	f := newConfigFile(path, startup, nil)
	snap, _ := f.Load()
	res, err := f.Save(cfgBase+"log_level: debug\n", snap.Revision)
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if !reflect.DeepEqual(res.Changed, []string{"log_level"}) {
		t.Errorf("Changed = %v, want [log_level]", res.Changed)
	}
}

// TestConfigFileSaveWriteFailureKeepsRunningValues 文件不可写时返回「写入配置文件失败」（不是冲突/无效），
// 已生效值与日志级别不变：恢复权限后再次保存相同内容，Changed 仍包含该字段。
func TestConfigFileSaveWriteFailureKeepsRunningValues(t *testing.T) {
	cfgSkipIfCannotDenyWrite(t)
	f, path, level := newCfgFile(t, cfgBase)
	snap, _ := f.Load()
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	content := cfgBase + "log_level: debug\n"
	_, err := f.Save(content, snap.Revision)
	if err == nil || errors.Is(err, copilot.ErrConfigConflict) || errors.Is(err, copilot.ErrConfigInvalid) ||
		!strings.Contains(err.Error(), "写入配置文件失败") {
		t.Fatalf("Save() error = %v, want 写入配置文件失败的普通错误", err)
	}
	if level.Level() != slog.LevelInfo {
		t.Errorf("写入失败不应改变日志级别，当前 %v", level.Level())
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := f.Save(content, snap.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.Changed, []string{"log_level"}) {
		t.Errorf("Changed = %v, want [log_level]（写入失败时已生效值不应更新）", res.Changed)
	}
}

// --- 端到端：run 启动服务，经管理接口读取并保存配置 ---

// cfgDo 发送带 Bearer 令牌的请求，返回状态码与响应体。
func cfgDo(t *testing.T, method, url, token, body string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, data
}

// TestServeWiresConfigStoreAndHotReloadsAPIKey 设置 management_key 时 serve 接上 ConfigStore：
// 读取配置、冲突与无效内容的状态码、保存后文件原样写入且 api_key 立即切换（旧 401、新通过鉴权），并记录「配置已更新」日志。
func TestServeWiresConfigStoreAndHotReloadsAPIKey(t *testing.T) {
	addr := freeLoopbackAddressForTest(t)
	dir := isolatedConfigDir(t)
	initial := fmt.Sprintf("listen: %s\nlog_level: info\n", strconv.Quote(addr)) + cfgBase
	configPath := writeConfigInDir(t, dir, initial)
	baseURL := startServiceForTest(t, configPath, addr)

	token := fetchConsoleToken(t, func(auth string) (int, []byte) {
		req, err := http.NewRequest(http.MethodPost, baseURL+"/v0/management/session", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", auth)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		return res.StatusCode, body
	}, validManagementKey)
	configURL := baseURL + "/v0/management/config"

	status, body := cfgDo(t, http.MethodGet, configURL, token, "")
	if status != http.StatusOK {
		t.Fatalf("GET config status = %d; body=%s", status, body)
	}
	var snap copilot.ConfigSnapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.Content != initial || snap.Path != configPath || !snap.Writable || snap.Revision != cfgRevision(initial) ||
		len(snap.RestartRequired) != 0 {
		t.Fatalf("GET config = %+v", snap)
	}

	put := func(content, revision string) (int, []byte) {
		payload, _ := json.Marshal(map[string]string{"content": content, "revision": revision})
		return cfgDo(t, http.MethodPut, configURL, token, string(payload))
	}
	if status, body := put(initial+"# x\n", "stale"); status != http.StatusConflict {
		t.Errorf("revision 过期 status = %d, want 409; body=%s", status, body)
	}
	invalid := fmt.Sprintf("listen: %s\napi_key: short\nmanagement_key: %s\n", strconv.Quote(addr), strconv.Quote(validManagementKey))
	if status, body := put(invalid, snap.Revision); status != http.StatusBadRequest {
		t.Errorf("无效内容 status = %d, want 400; body=%s", status, body)
	}
	if cfgReadFile(t, configPath) != initial {
		t.Fatal("被拒绝的保存不应改写文件")
	}

	updated := fmt.Sprintf("# 控制台保存\nlisten: %s\nlog_level: debug\napi_key: %s\nmanagement_key: %s\n",
		strconv.Quote(addr), strconv.Quote(cfgNewAPIKey), strconv.Quote(validManagementKey))
	status, body = put(updated, snap.Revision)
	if status != http.StatusOK {
		t.Fatalf("PUT config status = %d; body=%s", status, body)
	}
	var saved copilot.ConfigSaveResult
	if err := json.Unmarshal(body, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Revision != cfgRevision(updated) || !reflect.DeepEqual(saved.Changed, []string{"api_key", "log_level"}) ||
		len(saved.RestartRequired) != 0 {
		t.Errorf("PUT 响应 = %+v", saved)
	}
	if got := cfgReadFile(t, configPath); got != updated {
		t.Errorf("文件内容 = %q, want %q", got, updated)
	}

	if status, _ := cfgDo(t, http.MethodGet, baseURL+"/v1/models", validAPIKey, ""); status != http.StatusUnauthorized {
		t.Errorf("旧 api_key status = %d, want 401", status)
	}
	if status, body := cfgDo(t, http.MethodGet, baseURL+"/v1/models", cfgNewAPIKey, ""); status == http.StatusUnauthorized {
		t.Errorf("新 api_key 应通过鉴权，实际 401; body=%s", body)
	}
	// management_key 未变，原令牌继续有效。
	if status, _ := cfgDo(t, http.MethodGet, configURL, token, ""); status != http.StatusOK {
		t.Errorf("management_key 未变时令牌应继续有效，status = %d", status)
	}

	// 「配置已更新」日志写入日志文件。
	logPath := filepath.Join(dir, "logs", logFileName)
	deadline := time.Now().Add(5 * time.Second)
	for {
		data, _ := os.ReadFile(logPath)
		if bytes.Contains(data, []byte(`"配置已更新"`)) {
			if bytes.Contains(data, []byte(cfgNewAPIKey)) {
				t.Error("日志不应包含配置值")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("日志文件中缺少「配置已更新」: %s", data)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestServeWithoutManagementKeyHasNoConfigEndpoints 未设置 management_key 时不创建 ConfigStore，配置接口不存在（404）。
func TestServeWithoutManagementKeyHasNoConfigEndpoints(t *testing.T) {
	addr := freeLoopbackAddressForTest(t)
	configPath := writeConfigWithAccount(t, "nomgmt", accountJSON("nomgmt-token"), appConfigYAML(addr, "", validAPIKey, "info"))
	baseURL := startServiceForTest(t, configPath, addr)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v0/management/config"},
		{http.MethodPost, "/v0/management/config/validate"},
		{http.MethodPut, "/v0/management/config"},
	} {
		// 管理接口关闭时路径交给业务路由处理：带有效 api_key 时是普通 404。
		if status, body := cfgDo(t, tc.method, baseURL+tc.path, validAPIKey, "{}"); status != http.StatusNotFound {
			t.Errorf("%s %s status = %d, want 404; body=%s", tc.method, tc.path, status, body)
		}
	}
}

// TestConfigFileSaveRestoresOnWriteFailure 写入在截断之后失败时，Save 尽力把原内容写回，避免留下被截断的配置文件。
// 通过包级变量 writeConfigContent（Save 写文件的唯一入口）注入失败：第一次调用截断文件后返回错误，之后的调用照常写入。
func TestConfigFileSaveRestoresOnWriteFailure(t *testing.T) {
	f, path, level := newCfgFile(t, cfgBase)
	real := writeConfigContent
	t.Cleanup(func() { writeConfigContent = real })
	calls := 0
	writeConfigContent = func(p string, data []byte) error {
		calls++
		if calls == 1 {
			if err := os.Truncate(p, 0); err != nil {
				t.Fatal(err)
			}
			return errors.New("模拟磁盘已满")
		}
		return real(p, data)
	}

	_, err := f.Save(cfgBase+"log_level: debug\n", cfgRevision(cfgBase))
	if err == nil || !strings.Contains(err.Error(), "写入配置文件失败") {
		t.Fatalf("Save error = %v, want 含「写入配置文件失败」", err)
	}
	if got := cfgReadFile(t, path); got != cfgBase {
		t.Errorf("写入失败后文件内容 = %q, want 恢复为原内容 %q", got, cfgBase)
	}
	if calls != 2 {
		t.Errorf("writeConfigContent 调用次数 = %d, want 2（写入一次、恢复一次）", calls)
	}
	if level.Level() != slog.LevelInfo {
		t.Errorf("写入失败后日志级别 = %v, want 保持 info", level.Level())
	}
}
