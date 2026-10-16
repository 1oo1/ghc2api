package main

// 本文件覆盖启动配置的安全规则：api_key 必填且满足最小长度（与监听地址无关，配置文件不再接受 allow_anonymous）、
// management_key 的最小长度、生成请求的并发上限配置，以及 http.Server 的请求头上限。

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// securityMinKeyBytes 是 api_key 与 management_key 去除首尾空白后的最小字节数。
const securityMinKeyBytes = 32

// loadSecurityConfig 在包含一个账号的临时目录中写入 contents 并加载配置。
func loadSecurityConfig(t *testing.T, contents string) (appConfig, error) {
	t.Helper()
	return loadConfig(writeConfigWithAccount(t, "security", accountJSON("security-config-token"), contents))
}

// TestAPIKeyAlwaysRequired api_key 必填且必须有效，与监听地址无关：缺失或去除首尾空白后为空时，
// 本机与非本机监听地址都启动失败，错误信息提到 api_key；配置文件中的 allow_anonymous 键不再被接受（按未知字段拒绝）。
func TestAPIKeyAlwaysRequired(t *testing.T) {
	for _, listen := range []string{"127.0.0.1:0", "localhost:0", "[::1]:0", "0.0.0.0:0"} {
		listenLine := "listen: " + strconv.Quote(listen) + "\n"
		for _, tc := range []struct {
			name     string
			contents string
			wantOK   bool
		}{
			{name: "missing", contents: listenLine},
			{name: "blank", contents: listenLine + "api_key: \"   \"\n"},
			{name: "valid", contents: listenLine + "api_key: " + strconv.Quote(validAPIKey) + "\n", wantOK: true},
		} {
			t.Run(listen+"/"+tc.name, func(t *testing.T) {
				_, err := loadSecurityConfig(t, tc.contents)
				if tc.wantOK {
					if err != nil {
						t.Fatalf("合法 api_key 被拒绝: %v", err)
					}
					return
				}
				if err == nil {
					t.Fatal("api_key 缺失或为空白时未被拒绝")
				}
				if !strings.Contains(err.Error(), "api_key") {
					t.Fatalf("错误信息应提到 api_key: %v", err)
				}
			})
		}
	}

	// 即使配置了 allow_anonymous 键，缺少 api_key 也必须启动失败，该键不能用来绕过必填要求。
	t.Run("allow-anonymous-without-api-key-fails", func(t *testing.T) {
		for _, listen := range []string{"127.0.0.1:0", "localhost:0", "[::1]:0"} {
			if _, err := loadSecurityConfig(t, "listen: "+strconv.Quote(listen)+"\nallow_anonymous: true\n"); err == nil {
				t.Fatalf("listen %s：allow_anonymous 键不能绕过 api_key 必填", listen)
			}
		}
	})

	t.Run("allow-anonymous-key-rejected", func(t *testing.T) {
		_, err := loadSecurityConfig(t, "listen: 127.0.0.1:0\napi_key: "+strconv.Quote(validAPIKey)+"\nallow_anonymous: true\n")
		if err == nil {
			t.Fatal("配置中的 allow_anonymous 键未被拒绝")
		}
		if !strings.Contains(err.Error(), "allow_anonymous") {
			t.Fatalf("错误信息应指出未知字段 allow_anonymous: %v", err)
		}
	})
}

// TestSecretKeyMinimumLength api_key 与 management_key 去除首尾空白后若非空，字节数必须至少为 32；
// 长度不足时启动失败，错误信息指出是哪个字段。长度按字节计算，而不是按字符计算。
func TestSecretKeyMinimumLength(t *testing.T) {
	short := strings.Repeat("k", securityMinKeyBytes-1)
	exact := strings.Repeat("k", securityMinKeyBytes)
	// 每个汉字占 3 个字节：10 个汉字为 30 字节（不足），11 个汉字为 33 字节（足够）。
	cjkShort := strings.Repeat("密", 10)
	cjkEnough := strings.Repeat("密", 11)
	const loopbackListen = "listen: 127.0.0.1:0\n"
	for _, tc := range []struct {
		name     string
		contents string
		// wantErrPart 非空表示应失败，且错误信息必须包含该字段名；为空表示应成功加载。
		wantErrPart string
	}{
		{name: "api-key-31-bytes-fails", contents: loopbackListen + "api_key: " + strconv.Quote(short) + "\n", wantErrPart: "api_key"},
		{name: "api-key-32-bytes-loads", contents: loopbackListen + "api_key: " + strconv.Quote(exact) + "\n"},
		{name: "api-key-trimmed-to-31-bytes-fails", contents: loopbackListen + "api_key: " + strconv.Quote("  "+short+"  ") + "\n", wantErrPart: "api_key"},
		{name: "api-key-trimmed-to-32-bytes-loads", contents: loopbackListen + "api_key: " + strconv.Quote("  "+exact+"  ") + "\n"},
		{name: "management-key-31-bytes-fails", contents: "management_key: " + strconv.Quote(short) + "\n" + apiKeySetting, wantErrPart: "management_key"},
		{name: "management-key-32-bytes-loads", contents: "management_key: " + strconv.Quote(exact) + "\n" + apiKeySetting},
		{name: "management-key-counts-bytes-not-runes-fails", contents: "management_key: " + strconv.Quote(cjkShort) + "\n" + apiKeySetting, wantErrPart: "management_key"},
		{name: "management-key-cjk-33-bytes-loads", contents: "management_key: " + strconv.Quote(cjkEnough) + "\n" + apiKeySetting},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadSecurityConfig(t, tc.contents)
			if tc.wantErrPart == "" {
				if err != nil {
					t.Fatalf("长度满足要求的密钥被拒绝: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("长度不足 32 字节的密钥未被拒绝")
			}
			if !strings.Contains(err.Error(), tc.wantErrPart) {
				t.Fatalf("错误信息应指出字段 %s: %v", tc.wantErrPart, err)
			}
		})
	}
}

// TestMaxConcurrentRequestsConfig max_concurrent_requests 缺省为 32，显式 0 表示不限制，负数与非整数启动失败；
// 合法值同时体现在 appConfig 与传给账号池的 PoolConfig.MaxConcurrentRequests 中。
func TestMaxConcurrentRequestsConfig(t *testing.T) {
	for _, tc := range []struct {
		name     string
		contents string
		want     int
		wantErr  bool
		// wantErrPart 非空时错误信息必须包含该字段名。
		wantErrPart string
	}{
		{name: "default", contents: "", want: 32},
		{name: "zero-unlimited", contents: "max_concurrent_requests: 0\n", want: 0},
		{name: "custom", contents: "max_concurrent_requests: 5\n", want: 5},
		{name: "negative-fails", contents: "max_concurrent_requests: -1\n", wantErr: true, wantErrPart: "max_concurrent_requests"},
		{name: "non-integer-fails", contents: "max_concurrent_requests: many\n", wantErr: true, wantErrPart: "max_concurrent_requests"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config, err := loadSecurityConfig(t, apiKeySetting+tc.contents)
			if tc.wantErr {
				if err == nil {
					t.Fatal("非法的 max_concurrent_requests 未被拒绝")
				}
				if tc.wantErrPart != "" && !strings.Contains(err.Error(), tc.wantErrPart) {
					t.Fatalf("错误信息应指出字段 %s: %v", tc.wantErrPart, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("loadConfig() error = %v", err)
			}
			if config.MaxConcurrentRequests != tc.want {
				t.Fatalf("max_concurrent_requests = %d, want %d", config.MaxConcurrentRequests, tc.want)
			}
			if got := config.poolConfig(nil).MaxConcurrentRequests; got != tc.want {
				t.Fatalf("PoolConfig.MaxConcurrentRequests = %d, want %d", got, tc.want)
			}
		})
	}
}

// paddedHealthRequest 构造访问 /healthz 的请求，并带上指定字节数的填充请求头。
func paddedHealthRequest(t *testing.T, baseURL string, padding int) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, baseURL+"/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Padding", strings.Repeat("a", padding))
	return req
}

// TestServeLimitsRequestHeaderSize http.Server 的 MaxHeaderBytes 为 64 KiB。Go 允许的请求行加请求头的实际上限高于它：
// 全新连接上约为 MaxHeaderBytes + 4096 字节（即约 68 KiB，4096 是 bufio 余量，见 net/http 的 initialReadLimitSize）；
// 复用的 keep-alive 连接还会在空闲时预读下一个请求的前 4 KiB，因此约为 72 KiB。
// 因此：60 KiB 的请求头必须得到 200；76 KiB 的请求头在两种连接上都超过实际上限，不能得到 200，服务返回 431 或直接断开均可。
// 普通大小的请求头仍然正常处理；超限请求之后，新连接上的正常请求仍得到 200。
func TestServeLimitsRequestHeaderSize(t *testing.T) {
	addr := freeLoopbackAddressForTest(t)
	configPath := writeConfigWithAccount(t, "header-limit", accountJSON("header-limit-token"),
		appConfigYAML(addr, "", validAPIKey, "info"))
	baseURL := startServiceForTest(t, configPath, addr)
	client := &http.Client{Timeout: 10 * time.Second}

	res, err := client.Do(paddedHealthRequest(t, baseURL, 8<<10))
	if err != nil {
		t.Fatalf("8 KiB 请求头的请求失败: %v", err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("8 KiB 请求头 status = %d, want 200", res.StatusCode)
	}

	// 边界：低于两种连接上实际上限的 60 KiB 请求头必须被接受。
	res, err = client.Do(paddedHealthRequest(t, baseURL, 60<<10))
	if err != nil {
		t.Fatalf("60 KiB 请求头的请求失败: %v", err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("60 KiB 请求头 status = %d, want 200", res.StatusCode)
	}

	// 边界：超过两种连接上实际上限（约 68 KiB 与 72 KiB）的 76 KiB 请求头不能得到 200。
	res, err = client.Do(paddedHealthRequest(t, baseURL, 76<<10))
	if err == nil {
		_ = res.Body.Close()
		if res.StatusCode == http.StatusOK {
			t.Fatal("超过实际上限（约 68 至 72 KiB）的 76 KiB 请求头未被拒绝")
		}
	}

	res, err = client.Do(paddedHealthRequest(t, baseURL, 128<<10))
	if err == nil {
		_ = res.Body.Close()
		if res.StatusCode == http.StatusOK {
			t.Fatal("超过 64 KiB 的请求头未被拒绝")
		}
	}

	// 超限请求只影响它自己的连接：用新连接发送的正常请求仍得到 200。
	transport := &http.Transport{DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	fresh := &http.Client{Timeout: 10 * time.Second, Transport: transport}
	res, err = fresh.Do(paddedHealthRequest(t, baseURL, 8<<10))
	if err != nil {
		t.Fatalf("超限请求之后，新连接上的正常请求失败: %v", err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("超限请求之后，新连接上的正常请求 status = %d, want 200", res.StatusCode)
	}
}
