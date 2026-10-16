package copilot

// 本文件覆盖“新增账号后立即刷新额度”：管理接口导入与设备登录成功新增账号（二者都经 addVerifiedAccount）后，
// 异步请求一次该账号的额度，不必等待后台轮询或控制台的账号列表。
// 测试不启动 RunQuotaRefresh，并把 quotaRefreshTick 设为 1 小时以排除后台干扰，也不调用账号列表接口，
// 因此账号入池之后的额度请求只可能来自新增账号的触发。
// 导入与设备登录的校验请求发生在入池之前（它们本身就会请求一次 /copilot_internal/user），照常返回；
// 入池之后的额度请求由 addQuotaGate 控制（失败、阻塞或正常返回）。

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// addQuotaGate 控制账号入池之后的额度请求，即新增账号触发的刷新。
type addQuotaGate struct {
	status  int           // 非 0 时，入池之后的额度请求返回该状态
	block   chan struct{} // 非 nil 时，入池之后的额度请求阻塞，直到 unblock 或请求被取消
	once    sync.Once
	started chan struct{} // 入池之后的额度请求到达时发送一次（缓冲 1，非阻塞发送）

	mu    sync.Mutex
	calls int // 入池之后的额度请求次数
}

// newAddQuotaGate 创建 gate。status 非 0 时额度请求返回该状态；blocked 为 true 时额度请求阻塞直到 unblock。
func newAddQuotaGate(status int, blocked bool) *addQuotaGate {
	g := &addQuotaGate{status: status, started: make(chan struct{}, 1)}
	if blocked {
		g.block = make(chan struct{})
	}
	return g
}

// unblock 释放阻塞中的额度请求，可重复调用。
func (g *addQuotaGate) unblock() {
	if g.block != nil {
		g.once.Do(func() { close(g.block) })
	}
}

// count 返回入池之后的额度请求次数。
func (g *addQuotaGate) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

// serve 处理入池之后的额度请求：计数，通知测试请求已到达，按配置阻塞、失败或返回正常额度。
func (g *addQuotaGate) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	g.calls++
	g.mu.Unlock()
	select {
	case g.started <- struct{}{}:
	default:
	}
	if g.block != nil {
		select {
		case <-g.block:
		case <-r.Context().Done():
			return
		}
	}
	if g.status != 0 {
		w.WriteHeader(g.status)
		_, _ = io.WriteString(w, `{"message":"down"}`)
		return
	}
	_, _ = io.WriteString(w, mgmtQuotaBody)
}

// addFixture 是一个托管池及其上游：新增的账号为 Carol（OAuth token 为 oauth-carol，账号 ID 为 carol，
// 凭证文件名保留登录名大小写，即 Carol.json）。
type addFixture struct {
	up   *mgmtUpstream
	gate *addQuotaGate
	dir  string               // 凭证目录
	pool atomic.Pointer[Pool] // 构造后写入；上游处理器据此判断账号是否已入池
}

// newAddFixture 创建没有账号的托管池，并把 quotaRefreshTick 设为 1 小时。
// 上游的额度请求经 newAddClient 分流：入池之前照常返回，入池之后交给 gate。
func newAddFixture(t *testing.T, gate *addQuotaGate) *addFixture {
	t.Helper()
	setQuotaRefreshTick(t, time.Hour)
	up := newMgmtUpstream(t)
	up.setDeviceLogin("oauth-carol", "Carol")
	f := &addFixture{up: up, gate: gate, dir: t.TempDir()}
	client := newAddClient(t, f)
	p, err := NewManagedPool(PoolConfig{
		APIKey:        testAPIKey,
		ManagementKey: mgmtKey, CredentialsDir: f.dir, CacheDir: t.TempDir(), Client: client,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.pool.Store(p)
	quotaAwaitIdle(t, p)
	// 在 server 关闭（newAddClient 注册的清理）之前解除阻塞，否则 httptest 的 Close 会一直等待阻塞中的处理器。
	// 注册顺序晚于 server 关闭，因此清理时先于 server 关闭执行。
	t.Cleanup(gate.unblock)
	return f
}

// newAddClient 返回管理接口使用的测试客户端。/copilot_internal/user 的请求在账号入池之前照常返回（校验请求），
// 入池之后交给 gate；其余请求交给 mgmtUpstream 处理（/user、设备登录、session、模型等）。
// 每个额度请求都计入 mgmtUpstream 的计数，因此 up.quotaCount 是该 token 的全部额度请求次数。
func newAddClient(t *testing.T, f *addFixture) *http.Client {
	t.Helper()
	return testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/copilot_internal/user" {
			f.up.serve(w, r)
			return
		}
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "token ")
		f.up.mu.Lock()
		f.up.quotaCalls[token]++
		f.up.mu.Unlock()
		if p := f.pool.Load(); p != nil && p.findAccount("carol") != nil {
			f.gate.serve(w, r)
			return
		}
		// 校验请求：账号尚未入池，照常返回。
		_, _ = io.WriteString(w, mgmtQuotaBody)
	})
}

// addStaysAt 在 quotaSettleWindow 内确认入池之后的额度请求次数保持为 want，用于断言“不应发生”的刷新（有限窗口，见 quotaStaysAt）。
func addStaysAt(t *testing.T, g *addQuotaGate, want int) {
	t.Helper()
	deadline := time.Now().Add(quotaSettleWindow)
	for {
		if got := g.count(); got != want {
			t.Fatalf("入池之后的额度请求次数 = %d, want %d", got, want)
		}
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// addByImport 通过管理接口导入账号，返回 HTTP 状态码与响应正文。
func addByImport(f *addFixture) (int, string) {
	w := mgmtCall(f.pool.Load(), "POST", "/v0/management/accounts", `{"credential":{"oauth_token":"oauth-carol"}}`, mgmtKey)
	return w.Code, w.Body.String()
}

// addByDeviceLogin 发起设备登录并轮询到结束。成功时返回 200；失败时返回 0 与原因。
// 不使用 testing.T：它可能在协程中运行，因此失败只以返回值报告。
func addByDeviceLogin(f *addFixture) (int, string) {
	p := f.pool.Load()
	w := mgmtCall(p, "POST", "/v0/management/login", "", mgmtKey)
	var start mgmtLoginStart
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &start) != nil {
		return w.Code, w.Body.String()
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		w = mgmtCall(p, "GET", "/v0/management/login/status?state="+start.State, "", mgmtKey)
		var result mgmtLoginResult
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &result) != nil {
			return w.Code, w.Body.String()
		}
		switch {
		case result.Status == "success":
			return http.StatusOK, ""
		case result.Status != "pending":
			return 0, result.Status + " " + result.Error
		case time.Now().After(deadline):
			return 0, "设备登录超过 15 秒仍为 pending"
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// addEntry 是新增账号的一种入口。add 发起新增并等待入口返回，成功时返回 200。
type addEntry struct {
	name string
	add  func(f *addFixture) (int, string)
}

// addEntries 是两种入口：管理接口导入，以及设备登录。二者都经 addVerifiedAccount 把账号加入池。
var addEntries = []addEntry{
	{name: "import", add: addByImport},
	{name: "device-login", add: addByDeviceLogin},
}

// TestAddedAccountQuotaRefreshedOnceAfterAdd 导入或设备登录成功后，新账号的额度被异步刷新恰好一次：
// 入池之后的额度请求只有这一次，结果写入额度缓存与 quota.json。全程不调用账号列表接口。
func TestAddedAccountQuotaRefreshedOnceAfterAdd(t *testing.T) {
	for _, entry := range addEntries {
		t.Run(entry.name, func(t *testing.T) {
			f := newAddFixture(t, newAddQuotaGate(0, false))
			if code, detail := entry.add(f); code != http.StatusOK {
				t.Fatalf("%s 失败 status=%d detail=%s, want 200", entry.name, code, detail)
			}
			quotaWaitFor(t, "新增账号后没有刷新额度", func() bool { return f.gate.count() >= 1 })
			addStaysAt(t, f.gate, 1)
			// 校验请求 1 次（入池之前）加上入池后的刷新 1 次。
			if got := f.up.quotaCount("oauth-carol"); got != 2 {
				t.Fatalf("oauth-carol 的额度请求次数 = %d, want 2（校验 1 次 + 新增触发 1 次）", got)
			}
			carol := f.pool.Load().findAccount("carol")
			if carol == nil {
				t.Fatal("导入成功后账号池缺少 carol")
			}
			quotaWaitFor(t, "新账号的额度缓存为空", func() bool {
				q, _ := carol.quota.snapshot()
				return q != nil
			})
			if raw := persistReadFile(t, persistQuotaPath(t, f.pool.Load(), "carol"))["quota"]; len(raw) == 0 || string(raw) == "null" {
				t.Fatalf("新增后 quota.json 缺少额度: %s", raw)
			}
		})
	}
}

// TestAddedAccountQuotaRefreshDoesNotBlockAdd 额度接口阻塞时，导入与设备登录仍然及时返回成功；刷新在后台继续，
// 放行后额度才写入。
func TestAddedAccountQuotaRefreshDoesNotBlockAdd(t *testing.T) {
	for _, entry := range addEntries {
		t.Run(entry.name, func(t *testing.T) {
			f := newAddFixture(t, newAddQuotaGate(0, true))
			type outcome struct {
				code   int
				detail string
			}
			done := make(chan outcome, 1)
			go func() {
				code, detail := entry.add(f)
				done <- outcome{code, detail}
			}()
			var out outcome
			select {
			case out = <-done:
			case <-time.After(quotaAsyncTimeout):
				t.Fatal("额度接口阻塞时新增账号没有及时返回：额度刷新不应阻塞导入响应")
			}
			if out.code != http.StatusOK {
				t.Fatalf("%s 失败 status=%d detail=%s, want 200", entry.name, out.code, out.detail)
			}
			reviewAwait(t, f.gate.started, "入池后的额度刷新未到达上游")

			f.gate.unblock()
			carol := f.pool.Load().findAccount("carol")
			if carol == nil {
				t.Fatal("导入成功后账号池缺少 carol")
			}
			quotaWaitFor(t, "放行后新账号的额度仍未写入", func() bool {
				q, _ := carol.quota.snapshot()
				return q != nil
			})
		})
	}
}

// TestAddedAccountQuotaFailureKeepsAddSuccessful 新增账号后的额度请求失败时，导入或设备登录仍然成功；
// 账号与凭证保留，错误记入额度缓存，且不重试。
func TestAddedAccountQuotaFailureKeepsAddSuccessful(t *testing.T) {
	for _, entry := range addEntries {
		t.Run(entry.name, func(t *testing.T) {
			f := newAddFixture(t, newAddQuotaGate(http.StatusInternalServerError, false))
			if code, detail := entry.add(f); code != http.StatusOK {
				t.Fatalf("额度接口失败时 %s status=%d detail=%s, want 200", entry.name, code, detail)
			}
			quotaWaitFor(t, "新增账号后没有刷新额度", func() bool { return f.gate.count() >= 1 })
			carol := f.pool.Load().findAccount("carol")
			if carol == nil {
				t.Fatal("额度失败后账号池缺少 carol")
			}
			quotaWaitAttempt(t, carol, time.Time{})
			if _, errText := carol.quota.snapshot(); errText == "" {
				t.Fatal("额度刷新失败后没有记录 quota_error")
			}
			if _, err := os.Stat(filepath.Join(f.dir, "Carol.json")); err != nil {
				t.Fatalf("额度失败后凭证文件应保留: %v", err)
			}
			addStaysAt(t, f.gate, 1)
		})
	}
}
