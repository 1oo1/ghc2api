package copilot

// 账号错误计数与长冷却的测试公共部分。
// 本组测试（cooldown_*_test.go）尽量通过 HTTP 入口与测试上游观察行为，只依赖下面列出的少量内部名字。
//
// 实现者必须提供的新内部名字（测试按此书写）：
//   - const DefaultAccountCooldown = 2 * time.Hour：库层缺省冷却时长。
//   - PoolConfig.AccountCooldown time.Duration：账号冷却时长；<=0 时使用 DefaultAccountCooldown。
//     NewPool（只接受 []Config，没有 PoolConfig）固定使用 DefaultAccountCooldown。
//   - AccountView.CooldownReason string，JSON 名 cooldown_reason（omitempty）：仅冷却中输出，值为触发冷却的原因，
//     即触发那次错误的状态码字符串（"401"、"503" 等），网络错误为 "network"。
//   - 进入冷却时输出恰好一条 Warn 日志，msg 含"冷却"二字，并带上账号（account 下标或 user 字段），不含 token。
//   - cooldown.json：账号缓存目录下，经 writeCache 写入（0600），内容恰为 {"until":"RFC3339 时间","reason":"403"}。
//     解除冷却时删除该文件（或写为空内容）。
//
// 沿用并保持语义的内部名字（旧测试也在用）：
//   - (*Service).setCooldown(until time.Time)：只设置截止时间，不记录 reason、不清缓存、不写盘。
//   - (*Service).available(now time.Time) bool、(*Service).cacheFile(name string) string。
//   - (*Service).session、(*Service).models：内存中的 session 与模型列表缓存（字段）。
//   - (*poolAccount).record(reqCtx context.Context, w http.ResponseWriter, handle func(http.ResponseWriter), countErrors bool)。
//   - (*Pool).accountViews()、(*Pool).findAccount(id)、(*Pool).choose(key, tried)、(*Pool).affinity（lookup、bind、activeCounts）、
//     (*Pool).SaveSessionAffinity()、(*Pool).config。
//   - sessionAffinityFileName、affinityFile、affinityFileEntry、readCacheFile（affinity_store.go、cache.go）。
// 缓存文件名：session.json、models.json、quota.json（已有），cooldown.json（新增）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// cooldownSlack 是比较冷却截止时间时允许的误差：持久化使用 RFC3339（秒精度），且计时与测试断言之间有少量延迟。
const cooldownSlack = time.Second

// cooldownFixture 是一组可“重启”的托管池：凭证、缓存目录与上游保持不变，每次 start 相当于一次进程启动。
// 测试可以在 start 之前修改 config（如 AccountCooldown、SessionAffinityTTL、Client）与 accounts（如 Priority）。
type cooldownFixture struct {
	t        *testing.T
	up       *mgmtUpstream
	config   PoolConfig
	accounts []PoolAccount
}

// newCooldownFixture 为每个用户写入凭证（token 为 "oauth-<user>"，登录名即用户名），创建托管池所需的目录与上游。
// 管理密钥已启用，因此管理接口可用；缓存目录固定为临时目录。
func newCooldownFixture(t *testing.T, users ...string) *cooldownFixture {
	t.Helper()
	dir := t.TempDir()
	up := newMgmtUpstream(t)
	f := &cooldownFixture{t: t, up: up, config: PoolConfig{
		APIKey: testAPIKey, ManagementKey: mgmtKey, CredentialsDir: dir, CacheDir: t.TempDir(), Client: up.client,
	}}
	for _, user := range users {
		token := "oauth-" + user
		if err := SaveCredential(filepath.Join(dir, user+".json"), Credential{Token: token, User: user}); err != nil {
			t.Fatal(err)
		}
		up.setLogin(token, user)
		f.accounts = append(f.accounts, PoolAccount{Token: token, User: user})
	}
	return f
}

// start 以当前 config 与 accounts 创建托管池，并等待启动后的异步额度刷新结束。
// 同一 fixture 多次 start 使用同一缓存目录，相当于重启。
func (f *cooldownFixture) start() *Pool {
	f.t.Helper()
	p, err := NewManagedPool(f.config, f.accounts)
	if err != nil {
		f.t.Fatal(err)
	}
	quotaAwaitIdle(f.t, p)
	// 清理先于 TempDir 的删除：进入冷却的异步副作用可能仍在写 cooldown.json。每个池（含重启后的新池）都要注册。
	f.t.Cleanup(p.WaitCooldownEffects)
	return p
}

// cooldownView 直接读取账号视图（不经 HTTP，不触发额度刷新）。账号不存在时终止测试。
func cooldownView(t *testing.T, p *Pool, id string) AccountView {
	t.Helper()
	for _, view := range p.accountViews() {
		if view.ID == id {
			return view
		}
	}
	t.Fatalf("账号列表中缺少 %q", id)
	return AccountView{}
}

// cooldownBody 返回上游错误正文。正文不含 OAuth token 字样，便于断言日志不泄露 token。
func cooldownBody(status int) string {
	return fmt.Sprintf(`{"error":"upstream %d"}`, status)
}

// cooldownSetGeneration 设置账号生成请求的上游状态：非 200 时固定返回该状态与 cooldownBody；200 表示恢复正常。
func cooldownSetGeneration(u *mgmtUpstream, account string, status int) {
	if status == http.StatusOK {
		u.pool.mu.Lock()
		delete(u.pool.generationResult, account)
		u.pool.mu.Unlock()
		return
	}
	u.pool.failGeneration(account, poolFailure{status, cooldownBody(status)})
}

// cooldownChat 通过 HTTP 入口发送一次 chat 请求（不带会话信号，不参与会话亲和）。
func cooldownChat(p *Pool) *httptest.ResponseRecorder {
	return poolRequest(p, "POST", "/v1/chat/completions", poolChatBody)
}

// cooldownChatCancelled 发送一次已取消上下文的 chat 请求，模拟客户端在请求开始前断开。
func cooldownChatCancelled(p *Pool) *httptest.ResponseRecorder {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(poolChatBody)).WithContext(ctx)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	withTestAPIKey(p.Handler()).ServeHTTP(w, r)
	return w
}

// assertNoAccount 断言请求得到“没有可用账号”的 503，且正文包含规定的提示。
func assertNoAccount(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "没有可用的 GitHub Copilot 账号") {
		t.Fatalf("status=%d body=%s, want 503 没有可用的 GitHub Copilot 账号", w.Code, w.Body.String())
	}
}

// cooldownAssertWindow 断言冷却截止时间 until 位于 [before+d, after+d] 附近（允许 cooldownSlack 误差）。
// before 与 after 分别取在触发冷却的请求之前与之后，因此截止时间应落在这一区间内。
func cooldownAssertWindow(t *testing.T, until, before, after time.Time, d time.Duration) {
	t.Helper()
	low, high := before.Add(d-cooldownSlack), after.Add(d+cooldownSlack)
	if until.Before(low) || until.After(high) {
		t.Fatalf("冷却截止时间 %s 不在 [%s, %s] 内，时长应为 %s", until, low, high, d)
	}
}

// cooldownWarnings 返回 Warn 级别且 msg 含"冷却"的日志条目（即进入冷却的日志）。
func cooldownWarnings(t *testing.T, output []byte) []map[string]any {
	t.Helper()
	var warnings []map[string]any
	for _, entry := range jsonLogEntries(t, output) {
		msg, _ := entry["msg"].(string)
		if entry["level"] == "WARN" && strings.Contains(msg, "冷却") {
			warnings = append(warnings, entry)
		}
	}
	return warnings
}

// cooldownHasWarn 报告日志中是否存在任何 Warn 级别的条目。
func cooldownHasWarn(t *testing.T, output []byte) bool {
	t.Helper()
	for _, entry := range jsonLogEntries(t, output) {
		if entry["level"] == "WARN" {
			return true
		}
	}
	return false
}

// cooldownFile 返回账号缓存目录下的文件路径；name 为不含扩展名的文件名，如 "cooldown"、"session"。
func cooldownFile(t *testing.T, p *Pool, id, name string) string {
	t.Helper()
	account := p.findAccount(id)
	if account == nil {
		t.Fatalf("账号 %q 不存在", id)
	}
	return account.service.cacheFile(name)
}

// cooldownRecordGone 报告持久化记录是否已经删除：文件不存在，或内容为空。
func cooldownRecordGone(t *testing.T, path string) bool {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	if err != nil {
		t.Fatalf("读取 %s: %v", path, err)
	}
	return len(strings.TrimSpace(string(data))) == 0
}

// cooldownWriteFile 以 0600 权限写入原始内容，目录不存在时创建。用于构造过期或损坏的 cooldown.json。
func cooldownWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// cooldownFailingClient 返回的客户端对 Authorization 头以 prefix 开头的请求直接返回网络错误，其余请求交给 up。
func cooldownFailingClient(up *mgmtUpstream, prefix string) *http.Client {
	return &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		if strings.HasPrefix(r.Header.Get("Authorization"), prefix) {
			return nil, errors.New("test network failure")
		}
		return up.client.Transport.RoundTrip(r)
	})}
}

// cooldownLogValueIs 报告日志条目的任一字段值的文本表示是否等于 want。
func cooldownLogValueIs(entry map[string]any, want string) bool {
	for _, value := range entry {
		if fmt.Sprint(value) == want {
			return true
		}
	}
	return false
}

// cooldownLogHasDeadline 报告日志条目中是否有一个字段是 RFC3339 时间（冷却截止时间）。
func cooldownLogHasDeadline(entry map[string]any) bool {
	for _, value := range entry {
		text, ok := value.(string)
		if !ok {
			continue
		}
		if _, err := time.Parse(time.RFC3339Nano, text); err == nil {
			return true
		}
	}
	return false
}

// cooldownLogNamesAccount 报告日志条目是否带有账号信息：account 下标为 index，或 user、id 为 user。
func cooldownLogNamesAccount(entry map[string]any, index float64, user string) bool {
	if entry["account"] == index {
		return true
	}
	return entry["user"] == user || entry["id"] == user
}

// countAccount 统计生成请求顺序中某个 OAuth token 出现的次数。
func countAccount(order []string, account string) int {
	n := 0
	for _, got := range order {
		if got == account {
			n++
		}
	}
	return n
}

// cooldownFields 读取文件并解析为字段映射；文件不存在或不是 JSON 对象时终止测试。
func cooldownFields(t *testing.T, path string) map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s: %v", path, err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		t.Fatalf("%s 不是 JSON 对象: %v; body=%s", path, err, data)
	}
	return fields
}

// cooldownEventually 轮询直到 cond 为真，最长等待 5 秒。进入冷却的副作用（清缓存、删绑定、写 cooldown.json）是异步的，
// 测试用它等待这些副作用最终完成；条件始终不成立时以 what 说明未完成的内容。
func cooldownEventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("等待超时：%s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// cooldownSettle 等待进入冷却的异步副作用全部结束（Pool.WaitCooldownEffects），用于“之后不存在某文件”这类否定断言。
// 调用前必须已释放所有会阻塞副作用的上游阻塞（hold、模型锁等），否则会一直等待；因此它总是放在释放之后调用。
func cooldownSettle(p *Pool) {
	p.WaitCooldownEffects()
}

// cooldownAwaitRecord 等待 cooldown.json 写入完成（进入冷却的写入是异步的）。
func cooldownAwaitRecord(t *testing.T, p *Pool, id string) {
	t.Helper()
	path := cooldownFile(t, p, id, "cooldown")
	cooldownEventually(t, "cooldown.json 写入", func() bool {
		_, err := os.Stat(path)
		return err == nil
	})
}

// cooldownHook 是 cooldownWrap 的钩子：handled 为 true 时直接返回 resp 与 err，否则请求交给测试上游。
type cooldownHook func(r *http.Request) (resp *http.Response, handled bool, err error)

// cooldownWrap 返回的客户端先经过 hook，未处理的请求交给测试上游 up。
func cooldownWrap(up *mgmtUpstream, hook cooldownHook) *http.Client {
	return &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		if resp, handled, err := hook(r); handled {
			return resp, err
		}
		return up.client.Transport.RoundTrip(r)
	})}
}

// cooldownHTTPResponse 构造测试用的上游响应。
func cooldownHTTPResponse(r *http.Request, status int, contentType string, body io.ReadCloser) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     http.Header{"Content-Type": {contentType}},
		Body:       body,
		Request:    r,
	}
}

// cooldownMemorySession 在会话锁内读取内存中的 session 令牌。清理在异步副作用中进行，读取必须加锁，避免数据竞争。
func cooldownMemorySession(a *poolAccount) string {
	_ = a.service.sessionMu.lock(context.Background())
	defer a.service.sessionMu.unlock()
	return a.service.session.Token
}

// cooldownMemoryModelsCleared 在模型锁内报告内存中的模型列表是否已清空（零值）。
func cooldownMemoryModelsCleared(a *poolAccount) bool {
	_ = a.service.modelsMu.lock(context.Background())
	defer a.service.modelsMu.unlock()
	return len(a.service.models.Data) == 0 && a.service.models.At.IsZero()
}

// cooldownSessionFileHasToken 报告磁盘 session 文件是否仍含令牌；文件不存在、为空或无令牌都视为已清空。
func cooldownSessionFileHasToken(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var cached cachedSession
	return json.Unmarshal(data, &cached) == nil && cached.Token != ""
}

// cooldownModelsFileHasData 报告磁盘模型列表是否仍有缓存；文件不存在、为空或为零值都视为已清空。
func cooldownModelsFileHasData(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var cached modelList
	return json.Unmarshal(data, &cached) == nil && (len(cached.Data) != 0 || !cached.At.IsZero())
}

// cooldownFinished 等待后台请求的 goroutine 结束（done 被关闭）；超时报告错误。
// 后台请求若在测试结束后仍在运行，可能在 TempDir 清理时写入缓存文件，因此每个后台 goroutine 都必须在测试内等待。
func cooldownFinished(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Errorf("%s 未结束", what)
	}
}
