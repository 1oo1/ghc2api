package copilot

// 本文件覆盖管理接口对账号优先级的处理：账号视图始终输出 priority；PATCH 可以单独或同时更新 priority 与 disabled，
// 校验失败与写文件失败时不改变任何状态；PATCH 之后的选择结果随之变化。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// priorityManaged 为每个账号写入凭证文件 <user>.json（字段与 PoolAccount 一致），并用这些账号创建托管池。
// 返回池与凭证目录。
func priorityManaged(t *testing.T, up *mgmtUpstream, accounts ...PoolAccount) (*Pool, string) {
	t.Helper()
	dir := t.TempDir()
	for _, a := range accounts {
		credential := Credential{Token: a.Token, Host: a.Host, User: a.User, Priority: a.Priority, Disabled: a.Disabled}
		if err := SaveCredential(filepath.Join(dir, a.User+".json"), credential); err != nil {
			t.Fatal(err)
		}
		up.setLogin(a.Token, a.User)
	}
	p, err := NewManagedPool(mgmtPoolConfig(t, up, dir, 0), accounts)
	if err != nil {
		t.Fatal(err)
	}
	return p, dir
}

// mgmtPatchPriority 通过 PATCH 设置账号优先级，并要求返回 200。
func mgmtPatchPriority(t *testing.T, p *Pool, user string, priority int) {
	t.Helper()
	body := fmt.Sprintf(`{"id":%q,"priority":%d}`, user, priority)
	if w := mgmtCall(p, "PATCH", "/v0/management/accounts", body, mgmtKey); w.Code != http.StatusOK {
		t.Fatalf("PATCH %s priority=%d status=%d body=%s", user, priority, w.Code, w.Body.String())
	}
}

// decodeAccountView 解析单个账号视图的响应。
func decodeAccountView(t *testing.T, w *httptest.ResponseRecorder) AccountView {
	t.Helper()
	var view AccountView
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatalf("账号视图不是合法 JSON: %v; body=%s", err, w.Body.String())
	}
	return view
}

// assertViewState 断言账号在管理接口中的 disabled 与 priority。
func assertViewState(t *testing.T, p *Pool, user string, disabled bool, priority int) {
	t.Helper()
	view := mgmtAccountByID(t, mgmtAccounts(t, p), user)
	if view.Disabled != disabled || view.Priority != priority {
		t.Fatalf("%s 的视图 disabled=%v priority=%d, want disabled=%v priority=%d",
			user, view.Disabled, view.Priority, disabled, priority)
	}
}

// assertCredentialState 读取凭证文件，断言其中的 disabled 与 priority。
func assertCredentialState(t *testing.T, dir, user string, disabled bool, priority int) {
	t.Helper()
	c, err := LoadCredential(filepath.Join(dir, user+".json"))
	if err != nil || c.Disabled != disabled || c.Priority != priority {
		t.Fatalf("凭证文件 %s = %+v, %v; want disabled=%v priority=%d", user, c, err, disabled, priority)
	}
}

// readFileBytes 读取文件原始内容，用于断言失败的请求没有写入文件。
func readFileBytes(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestManagementAccountViewAlwaysOutputsPriority 账号视图始终输出 priority 字段，包括 0 与 -1；
// 初始优先级由 NewManagedPool 传入。
func TestManagementAccountViewAlwaysOutputsPriority(t *testing.T) {
	up := newMgmtUpstream(t)
	p, _ := priorityManaged(t, up, priorityAccount("alice", 0), priorityAccount("bob", -1), priorityAccount("carol", 100))
	w := mgmtCall(p, "GET", "/v0/management/accounts", "", mgmtKey)
	if w.Code != http.StatusOK {
		t.Fatalf("GET accounts status=%d body=%s", w.Code, w.Body.String())
	}
	var raw []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("账号列表不是合法 JSON: %v", err)
	}
	if len(raw) != 3 {
		t.Fatalf("账号数 = %d, want 3", len(raw))
	}
	for _, view := range raw {
		if _, ok := view["priority"]; !ok {
			t.Fatalf("账号视图缺少 priority 字段: %v", view)
		}
	}
	want := map[string]int{"alice": 0, "bob": -1, "carol": 100}
	for id, priority := range want {
		assertViewState(t, p, id, false, priority)
	}
}

// TestManagementPatchPriorityUpdatesViewAndFile 只提供 priority 时更新内存与凭证文件，
// 凭证文件中的其他字段（token、host、user）保持不变，并返回更新后的账号视图。
func TestManagementPatchPriorityUpdatesViewAndFile(t *testing.T) {
	up := newMgmtUpstream(t)
	alice := priorityAccount("alice", 0)
	alice.Host = "acme.ghe.com"
	p, dir := priorityManaged(t, up, alice)

	w := mgmtCall(p, "PATCH", "/v0/management/accounts", `{"id":"alice","priority":42}`, mgmtKey)
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH status=%d body=%s", w.Code, w.Body.String())
	}
	view := decodeAccountView(t, w)
	if view.ID != "alice" || view.Priority != 42 || view.Disabled {
		t.Fatalf("PATCH 返回的视图 = %+v, want ID alice、priority 42 且未禁用", view)
	}
	assertViewState(t, p, "alice", false, 42)
	c, err := LoadCredential(filepath.Join(dir, "alice.json"))
	if err != nil || c.Priority != 42 || c.Token != "oauth-alice" || c.User != "alice" || c.Host != "acme.ghe.com" {
		t.Fatalf("凭证文件 = %+v, %v; want priority 42 且 token、user、host 保持不变", c, err)
	}
}

// TestManagementPatchPriorityBoundaries 优先级的边界值 -1 与 100 可以设置，并写入凭证文件。
func TestManagementPatchPriorityBoundaries(t *testing.T) {
	up := newMgmtUpstream(t)
	p, dir := priorityManaged(t, up, priorityAccount("alice", 0))
	for _, priority := range []int{MinPriority, MaxPriority} {
		body := fmt.Sprintf(`{"id":"alice","priority":%d}`, priority)
		w := mgmtCall(p, "PATCH", "/v0/management/accounts", body, mgmtKey)
		if w.Code != http.StatusOK || decodeAccountView(t, w).Priority != priority {
			t.Fatalf("设置 priority=%d status=%d body=%s", priority, w.Code, w.Body.String())
		}
		assertCredentialState(t, dir, "alice", false, priority)
	}
}

// TestManagementPatchPriorityKeepsDisabledFlag 只改 priority 时 disabled 保持不变，只改 disabled 时 priority 保持不变，
// 两者同时提供时都更新。内存与凭证文件保持一致。
func TestManagementPatchPriorityKeepsDisabledFlag(t *testing.T) {
	up := newMgmtUpstream(t)
	alice := priorityAccount("alice", 7)
	alice.Disabled = true
	p, dir := priorityManaged(t, up, alice)

	// 只改 priority：禁用状态保持 true。
	if w := mgmtCall(p, "PATCH", "/v0/management/accounts", `{"id":"alice","priority":9}`, mgmtKey); w.Code != http.StatusOK {
		t.Fatalf("只改 priority status=%d body=%s", w.Code, w.Body.String())
	}
	assertViewState(t, p, "alice", true, 9)
	assertCredentialState(t, dir, "alice", true, 9)

	// 只改 disabled：优先级保持 9。
	if w := mgmtCall(p, "PATCH", "/v0/management/accounts", `{"id":"alice","disabled":false}`, mgmtKey); w.Code != http.StatusOK {
		t.Fatalf("只改 disabled status=%d body=%s", w.Code, w.Body.String())
	}
	assertViewState(t, p, "alice", false, 9)
	assertCredentialState(t, dir, "alice", false, 9)

	// 同时提供：两者都更新。
	if w := mgmtCall(p, "PATCH", "/v0/management/accounts", `{"id":"alice","disabled":true,"priority":-1}`, mgmtKey); w.Code != http.StatusOK {
		t.Fatalf("同时提供两个字段 status=%d body=%s", w.Code, w.Body.String())
	}
	assertViewState(t, p, "alice", true, -1)
	assertCredentialState(t, dir, "alice", true, -1)
}

// TestManagementPatchRequiresIDAndAtLeastOneField 缺少 id，或 disabled 与 priority 都没有提供时返回 400，且不改变任何状态、不写文件。
func TestManagementPatchRequiresIDAndAtLeastOneField(t *testing.T) {
	for _, body := range []string{`{"disabled":true}`, `{"priority":5}`, `{"id":"alice"}`, `{}`} {
		t.Run(body, func(t *testing.T) {
			up := newMgmtUpstream(t)
			p, dir := priorityManaged(t, up, priorityAccount("alice", 3))
			before := readFileBytes(t, filepath.Join(dir, "alice.json"))
			if w := mgmtCall(p, "PATCH", "/v0/management/accounts", body, mgmtKey); w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
			}
			assertViewState(t, p, "alice", false, 3)
			if after := readFileBytes(t, filepath.Join(dir, "alice.json")); !bytes.Equal(before, after) {
				t.Fatalf("失败的请求改写了凭证文件:\n%s\n->\n%s", before, after)
			}
		})
	}
}

// TestManagementPatchRejectsInvalidPriority priority 超出 [-1, 100]、不是整数或不是数字时返回 400，
// 且不改变任何状态、不写文件；同一请求中合法的 disabled 也不生效。
func TestManagementPatchRejectsInvalidPriority(t *testing.T) {
	for _, body := range []string{
		`{"id":"alice","priority":-2}`,
		`{"id":"alice","priority":101}`,
		`{"id":"alice","priority":1.5}`,
		`{"id":"alice","priority":"5"}`,
		`{"id":"alice","disabled":true,"priority":101}`,
	} {
		t.Run(body, func(t *testing.T) {
			up := newMgmtUpstream(t)
			p, dir := priorityManaged(t, up, priorityAccount("alice", 3))
			before := readFileBytes(t, filepath.Join(dir, "alice.json"))
			if w := mgmtCall(p, "PATCH", "/v0/management/accounts", body, mgmtKey); w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
			}
			assertViewState(t, p, "alice", false, 3)
			if after := readFileBytes(t, filepath.Join(dir, "alice.json")); !bytes.Equal(before, after) {
				t.Fatalf("非法优先级改写了凭证文件:\n%s\n->\n%s", before, after)
			}
		})
	}
}

// TestManagementPatchPriorityUnknownIDIsNotFound 未知账号 ID 返回 404，已有账号的状态与凭证文件不受影响。
func TestManagementPatchPriorityUnknownIDIsNotFound(t *testing.T) {
	up := newMgmtUpstream(t)
	p, dir := priorityManaged(t, up, priorityAccount("alice", 3))
	before := readFileBytes(t, filepath.Join(dir, "alice.json"))
	if w := mgmtCall(p, "PATCH", "/v0/management/accounts", `{"id":"nobody","priority":5}`, mgmtKey); w.Code != http.StatusNotFound {
		t.Fatalf("未知 ID status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
	assertViewState(t, p, "alice", false, 3)
	if after := readFileBytes(t, filepath.Join(dir, "alice.json")); !bytes.Equal(before, after) {
		t.Fatal("未知 ID 的请求改写了已有账号的凭证文件")
	}
}

// TestManagementPatchPriorityWriteFailureKeepsState 凭证写入失败时返回 500，内存中的优先级保持不变。
func TestManagementPatchPriorityWriteFailureKeepsState(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 可以写入只读目录，无法模拟写入失败")
	}
	up := newMgmtUpstream(t)
	p, dir := priorityManaged(t, up, priorityAccount("alice", 3))
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	if w := mgmtCall(p, "PATCH", "/v0/management/accounts", `{"id":"alice","priority":50}`, mgmtKey); w.Code != http.StatusInternalServerError {
		t.Fatalf("凭证写入失败时 status = %d, want 500; body=%s", w.Code, w.Body.String())
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	assertViewState(t, p, "alice", false, 3)
	assertCredentialState(t, dir, "alice", false, 3)
}

// TestManagementPatchPriorityChangesNextSelection PATCH 修改优先级后，后续选择立即按新优先级进行。
func TestManagementPatchPriorityChangesNextSelection(t *testing.T) {
	up := newMgmtUpstream(t)
	p, _ := priorityManaged(t, up, priorityAccount("alice", 0), priorityAccount("bob", 0))
	if got := userOf(p.choose("", nil)); got != "alice" {
		t.Fatalf("初始 choose = %s, want alice", got)
	}

	mgmtPatchPriority(t, p, "bob", 5)
	if got := userOf(p.choose("", nil)); got != "bob" {
		t.Fatalf("bob 提高优先级后 choose = %s, want bob", got)
	}
	// 经由 HTTP 发起的请求同样使用新的优先级。
	if w := poolRequest(p, "POST", "/v1/chat/completions", poolChatBody); w.Code != http.StatusOK {
		t.Fatalf("生成请求 status=%d body=%s", w.Code, w.Body.String())
	}
	assertGenerationOrder(t, up.pool, "oauth-bob")

	mgmtPatchPriority(t, p, "bob", -1)
	if got := userOf(p.choose("", nil)); got != "alice" {
		t.Fatalf("bob 降低优先级后 choose = %s, want alice", got)
	}
}
