package copilot

// 本文件覆盖 (*Service).modelAvailability(id string) (listed, known bool)：账号选号时用它判断账号是否有某个模型。
//   - 基于内存中最近一次加载的模型列表（不论是否过期），不得阻塞（选号时持有 Pool.mu，不能等 modelsMu）；
//   - 从未加载过、或已被 forgetModels（冷却）清空：known = false；
//   - listed 当且仅当 id 在可见列表（Data）或隐藏 chat 模型（Endpoints）中，且账号当前未拒绝它（isRefused 为 false）；
//   - 列表在首次生成请求校验或查询模型列表时自然加载（含从磁盘缓存加载），不为路由额外访问上游。

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// availabilityModels 是 modelAvailability 测试使用的 /models 条目：
// m-visible 可见，m-hidden 隐藏的 chat 模型（只有路径记录），m-terms 待接受条款（可见），m-embed 不是 chat 模型。
var availabilityModels = []string{
	modelEntry("m-visible", "m-visible"),
	`{"id":"m-hidden","model_picker_enabled":false,"capabilities":{"type":"chat"},"supported_endpoints":["/chat/completions"]}`,
	`{"id":"m-terms","model_picker_enabled":true,"capabilities":{"type":"chat"},"policy":{"state":"disabled","terms":"Accept terms"}}`,
	`{"id":"m-embed","model_picker_enabled":true,"capabilities":{"type":"embeddings"}}`,
}

// availabilityService 创建只有一个账号 solo 的 Service，其 /models 为 availabilityModels。
func availabilityService(t *testing.T, config Config) (*Service, *modelsUpstream) {
	t.Helper()
	return availabilityServiceFor(t, config, availabilityModels...)
}

// availabilityServiceFor 与 availabilityService 相同，但 /models 为 entries。
func availabilityServiceFor(t *testing.T, config Config, entries ...string) (*Service, *modelsUpstream) {
	t.Helper()
	up := newModelsUpstream(t)
	up.setModels("solo", entries...)
	config.APIKey, config.Token, config.Client = testAPIKey, "oauth-solo", up.client()
	return testService(t, config), up
}

// expectAvailability 断言 modelAvailability 对 id 的结果。
func expectAvailability(t *testing.T, s *Service, stage, id string, wantListed, wantKnown bool) {
	t.Helper()
	if listed, known := s.modelAvailability(id); listed != wantListed || known != wantKnown {
		t.Errorf("%s：modelAvailability(%q) = (listed=%v, known=%v), want (listed=%v, known=%v)",
			stage, id, listed, known, wantListed, wantKnown)
	}
}

func loadModels(t *testing.T, s *Service) {
	t.Helper()
	if w := testRequest(s, http.MethodGet, "/v1/models", ""); w.Code != http.StatusOK {
		t.Fatalf("加载模型列表 status=%d body=%s", w.Code, w.Body.String())
	}
}

// TestModelAvailability 未加载时 known 为 false；加载后可见模型、隐藏 chat 模型、待接受条款的模型 listed 且 known，
// 非 chat 模型与不在列表中的模型 known 但不 listed；账号拒绝的模型不 listed（恢复后又 listed）；
// forgetModels 清空后 known 回到 false，重新加载后恢复。
func TestModelAvailability(t *testing.T) {
	s, up := availabilityService(t, Config{})
	ids := []string{"m-visible", "m-hidden", "m-terms", "m-embed", "m-none"}

	for _, id := range ids {
		expectAvailability(t, s, "加载前", id, false, false)
	}

	loadModels(t, s)
	for id, listed := range map[string]bool{"m-visible": true, "m-hidden": true, "m-terms": true, "m-embed": false, "m-none": false} {
		expectAvailability(t, s, "加载后", id, listed, true)
	}

	// 账号拒绝的模型（可见与隐藏）不 listed，其他模型不受影响；拒绝被清除后恢复。
	s.refuse("m-visible")
	s.refuse("m-hidden")
	expectAvailability(t, s, "拒绝后", "m-visible", false, true)
	expectAvailability(t, s, "拒绝后", "m-hidden", false, true)
	expectAvailability(t, s, "拒绝后", "m-terms", true, true)
	s.unrefuse("m-visible", s.refusalVersion("m-visible"))
	s.unrefuse("m-hidden", s.refusalVersion("m-hidden"))
	expectAvailability(t, s, "拒绝清除后", "m-visible", true, true)
	expectAvailability(t, s, "拒绝清除后", "m-hidden", true, true)

	// 冷却清空模型列表（内存与磁盘）。
	if err := s.forgetModels(func() bool { return true }); err != nil {
		t.Fatalf("forgetModels: %v", err)
	}
	for _, id := range ids {
		expectAvailability(t, s, "清空后", id, false, false)
	}

	loadModels(t, s)
	expectAvailability(t, s, "重新加载后", "m-visible", true, true)
	expectAvailability(t, s, "重新加载后", "m-none", false, true)
	if got := up.modelsCalls("solo"); got != 2 {
		t.Errorf("/models 上游请求 %d 次, want 2（首次加载与清空后的重新加载）", got)
	}
}

// TestModelAvailabilityKeepsExpiredList 列表过期后 modelAvailability 仍按最近一次加载的列表回答，且不访问上游。
func TestModelAvailabilityKeepsExpiredList(t *testing.T) {
	const ttl = 20 * time.Millisecond
	s, up := availabilityService(t, Config{ModelsCacheTTL: ttl})
	loadModels(t, s)
	time.Sleep(ttl + 40*time.Millisecond)

	expectAvailability(t, s, "缓存过期后", "m-visible", true, true)
	expectAvailability(t, s, "缓存过期后", "m-none", false, true)
	if got := up.modelsCalls("solo"); got != 1 {
		t.Errorf("/models 上游请求 %d 次, want 1（modelAvailability 不访问上游）", got)
	}
}

// TestModelAvailabilityDoesNotBlockOnModelsMutex modelsMu 被持有（例如正在刷新模型列表）时 modelAvailability 立即返回：
// 选号时持有 Pool.mu，不能等待模型列表的刷新。
func TestModelAvailabilityDoesNotBlockOnModelsMutex(t *testing.T) {
	s, _ := availabilityService(t, Config{})
	loadModels(t, s)
	if err := s.modelsMu.lock(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.modelsMu.unlock()

	type answer struct{ listed, known bool }
	done := make(chan answer, 1)
	go func() {
		listed, known := s.modelAvailability("m-visible")
		done <- answer{listed, known}
	}()
	select {
	case got := <-done:
		if !got.listed || !got.known {
			t.Fatalf("modelAvailability = %+v, want listed 且 known", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("modelsMu 被持有时 modelAvailability 被阻塞")
	}
}

// TestModelAvailabilityFollowsDiskCache 从磁盘缓存加载模型列表（另一个 Service 实例已刷新过）同样使 known 为 true，且不访问上游。
func TestModelAvailabilityFollowsDiskCache(t *testing.T) {
	dir := t.TempDir()
	first, up := availabilityService(t, Config{CacheDir: dir, ModelsCacheTTL: time.Hour})
	loadModels(t, first)

	second := testService(t, Config{APIKey: testAPIKey, Token: "oauth-solo", Client: up.client(), CacheDir: dir, ModelsCacheTTL: time.Hour})
	expectAvailability(t, second, "加载前", "m-visible", false, false)
	loadModels(t, second)
	expectAvailability(t, second, "从磁盘缓存加载后", "m-visible", true, true)
	expectAvailability(t, second, "从磁盘缓存加载后", "m-none", false, true)
	if got := up.modelsCalls("solo"); got != 1 {
		t.Errorf("/models 上游请求 %d 次, want 1（第二个实例用磁盘缓存）", got)
	}
}

// TestModelAvailabilityResetWhenAccountCools 账号进入冷却（401）后其模型列表被清空：known 回到 false；其他账号不受影响。
func TestModelAvailabilityResetWhenAccountCools(t *testing.T) {
	up := newModelsUpstream(t)
	up.setModels("alice", modelEntries("m-all")...)
	up.setModels("bob", modelEntries("m-all")...)
	p := newModelsPool(t, up, 0, priorityAccount("alice", 0), priorityAccount("bob", 0))
	warmModels(t, p)
	alice, bob := p.findAccount("alice").service, p.findAccount("bob").service
	expectAvailability(t, alice, "冷却前", "m-all", true, true)

	up.pool.failGeneration("oauth-alice", poolFailure{http.StatusUnauthorized, cooldownBody(http.StatusUnauthorized)})
	if w := modelsChat(p, "m-all", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s, want alice 返回 401", w.Code, w.Body.String())
	}
	p.WaitCooldownEffects()
	expectAvailability(t, alice, "冷却后", "m-all", false, false)
	expectAvailability(t, bob, "冷却后", "m-all", true, true)
}

// policyModels 是策略相关的 /models 条目。策略明确不是 enabled 且没有条款的 chat 模型不进可见列表（Data），
// 只剩 Endpoints 里的路径记录；它们不应被算作账号有该模型（listed 为 false），否则请求仍会落到该账号得到 400/403。
// 隐藏但策略 enabled（或没有策略）的模型、待接受条款的模型照常 listed。
var policyModels = []string{
	modelEntry("m-visible", "m-visible"),
	// 策略禁用（无条款）：隐藏的、picker 启用的、state 不是 disabled 的、条款为空串的、内部 id 的、Experimental 的，都不 listed。
	`{"id":"m-policy-hidden","model_picker_enabled":false,"capabilities":{"type":"chat"},"supported_endpoints":["/chat/completions"],"policy":{"state":"disabled"}}`,
	`{"id":"m-policy-picker","model_picker_enabled":true,"capabilities":{"type":"chat"},"supported_endpoints":["/chat/completions"],"policy":{"state":"disabled","terms":""}}`,
	`{"id":"m-policy-unconfigured","model_picker_enabled":false,"capabilities":{"type":"chat"},"supported_endpoints":["/chat/completions"],"policy":{"state":"unconfigured"}}`,
	`{"id":"gpt-9-secondary","model_picker_enabled":true,"capabilities":{"type":"chat"},"supported_endpoints":["/chat/completions"],"policy":{"state":"disabled"}}`,
	`{"id":"m-policy-experimental","vendor":"Experimental","model_picker_enabled":false,"model_picker_category":"powerful","capabilities":{"type":"chat"},"supported_endpoints":["/chat/completions"],"policy":{"state":"disabled"}}`,
	// 隐藏但可用：策略 enabled、没有策略、内部 id 但策略 enabled，都 listed。
	`{"id":"m-hidden-enabled","model_picker_enabled":false,"capabilities":{"type":"chat"},"supported_endpoints":["/chat/completions"],"policy":{"state":"enabled"}}`,
	`{"id":"m-hidden-nopolicy","model_picker_enabled":false,"capabilities":{"type":"chat"},"supported_endpoints":["/chat/completions"]}`,
	`{"id":"gpt-9-tertiary","model_picker_enabled":true,"capabilities":{"type":"chat"},"supported_endpoints":["/chat/completions"],"policy":{"state":"enabled"}}`,
	// 待接受条款：可见（Waiting），照常 listed。
	`{"id":"m-terms","model_picker_enabled":true,"capabilities":{"type":"chat"},"policy":{"state":"disabled","terms":"Accept terms"}}`,
	// 非 chat 模型从不记录。
	`{"id":"m-embed-disabled","model_picker_enabled":true,"capabilities":{"type":"embeddings"},"policy":{"state":"disabled"}}`,
}

// policyExpectations 是 policyModels 加载后各 id 的 modelAvailability 期望（known 都为 true）。
var policyExpectations = map[string]bool{
	"m-visible": true, "m-hidden-enabled": true, "m-hidden-nopolicy": true, "gpt-9-tertiary": true, "m-terms": true,
	"m-policy-hidden": false, "m-policy-picker": false, "m-policy-unconfigured": false, "gpt-9-secondary": false,
	"m-policy-experimental": false, "m-embed-disabled": false, "m-none": false,
}

// policyDisabledIDs 是 policyModels 中应记入 disabled 的 chat 模型。
var policyDisabledIDs = []string{"m-policy-hidden", "m-policy-picker", "m-policy-unconfigured", "gpt-9-secondary", "m-policy-experimental"}

func expectPolicyAvailability(t *testing.T, s *Service, stage string) {
	t.Helper()
	for id, listed := range policyExpectations {
		expectAvailability(t, s, stage, id, listed, true)
	}
}

// TestModelAvailabilityPolicyDisabledHiddenModels 策略禁用（state 不是 enabled 且没有条款）的 chat 模型不算 listed
// （不论是否隐藏、内部 id、Experimental、picker 启用），但 known；隐藏但 enabled（或没有策略）的模型与待接受条款的模型仍 listed。
func TestModelAvailabilityPolicyDisabledHiddenModels(t *testing.T) {
	s, _ := availabilityServiceFor(t, Config{}, policyModels...)
	expectAvailability(t, s, "加载前", "m-policy-hidden", false, false)
	loadModels(t, s)
	expectPolicyAvailability(t, s, "加载后")
}

// policyModelsCacheFile 读取账号磁盘缓存 models.json，解析成通用对象。
func policyModelsCacheFile(t *testing.T, s *Service) (string, map[string]any) {
	t.Helper()
	path := s.cacheFile("models")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取模型缓存: %v", err)
	}
	var cache map[string]any
	if err := json.Unmarshal(data, &cache); err != nil {
		t.Fatalf("模型缓存不是合法 JSON: %v", err)
	}
	return path, cache
}

// TestModelAvailabilityDisabledModelsPersistToDiskCache 被策略禁用的模型记入磁盘缓存的 disabled 字段（id 到 true 的对象），
// 另一个 Service 实例从磁盘缓存加载后判断相同，且不访问上游。
func TestModelAvailabilityDisabledModelsPersistToDiskCache(t *testing.T) {
	dir := t.TempDir()
	first, up := availabilityServiceFor(t, Config{CacheDir: dir, ModelsCacheTTL: time.Hour}, policyModels...)
	loadModels(t, first)

	_, cache := policyModelsCacheFile(t, first)
	disabled, _ := cache["disabled"].(map[string]any)
	if len(disabled) != len(policyDisabledIDs) {
		t.Fatalf("缓存的 disabled = %v, want 恰好含 %v", cache["disabled"], policyDisabledIDs)
	}
	for _, id := range policyDisabledIDs {
		if disabled[id] != true {
			t.Errorf("缓存的 disabled 缺少 %s: %v", id, disabled)
		}
	}

	second := testService(t, Config{APIKey: testAPIKey, Token: "oauth-solo", Client: up.client(), CacheDir: dir, ModelsCacheTTL: time.Hour})
	expectAvailability(t, second, "加载前", "m-policy-hidden", false, false)
	loadModels(t, second)
	expectPolicyAvailability(t, second, "从磁盘缓存加载后")
	if got := up.modelsCalls("solo"); got != 1 {
		t.Errorf("/models 上游请求 %d 次, want 1（第二个实例用磁盘缓存）", got)
	}
}

// TestModelAvailabilityLegacyDiskCacheWithoutDisabledField 旧版本留下的磁盘缓存没有 disabled 字段：视为空，加载照常，
// 此时隐藏 chat 模型（含策略禁用的）都按旧行为 listed，不访问上游。
func TestModelAvailabilityLegacyDiskCacheWithoutDisabledField(t *testing.T) {
	dir := t.TempDir()
	first, up := availabilityServiceFor(t, Config{CacheDir: dir, ModelsCacheTTL: time.Hour}, policyModels...)
	loadModels(t, first)
	path, cache := policyModelsCacheFile(t, first)
	delete(cache, "disabled")
	data, err := json.Marshal(cache)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	second := testService(t, Config{APIKey: testAPIKey, Token: "oauth-solo", Client: up.client(), CacheDir: dir, ModelsCacheTTL: time.Hour})
	loadModels(t, second)
	for _, id := range append([]string{"m-visible", "m-hidden-enabled", "m-terms"}, policyDisabledIDs...) {
		expectAvailability(t, second, "旧缓存加载后", id, true, true)
	}
	expectAvailability(t, second, "旧缓存加载后", "m-embed-disabled", false, true)
	expectAvailability(t, second, "旧缓存加载后", "m-none", false, true)
	if got := up.modelsCalls("solo"); got != 1 {
		t.Errorf("/models 上游请求 %d 次, want 1（旧缓存仍有效）", got)
	}
}

// TestPolicyDisabledHiddenModelCanStillBeSpecified 生成前的路径校验不变：策略禁用的隐藏模型仍可手动指定，
// 按 /models 声明的路径放行（交给上游决定），不支持的路径仍 400；modelAvailability 不影响单账号 Service 的校验。
func TestPolicyDisabledHiddenModelCanStillBeSpecified(t *testing.T) {
	s, up := availabilityServiceFor(t, Config{}, policyModels...)
	if w := testRequest(s, http.MethodPost, "/v1/chat/completions", endpointTestBody("/v1/chat/completions", "m-policy-hidden")); w.Code != http.StatusOK {
		t.Fatalf("手动指定策略禁用的隐藏模型 status=%d body=%s, want 200（仍转发）", w.Code, w.Body.String())
	}
	assertGenerationOrder(t, up.pool, "oauth-solo")

	w := testRequest(s, http.MethodPost, "/v1/responses", endpointTestBody("/v1/responses", "m-policy-hidden"))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "不支持 API 路径") {
		t.Fatalf("不支持的路径 status=%d body=%s, want 400 不支持 API 路径", w.Code, w.Body.String())
	}
	assertGenerationOrder(t, up.pool, "oauth-solo") // 没有新的生成请求
}
