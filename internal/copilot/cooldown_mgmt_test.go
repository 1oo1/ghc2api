package copilot

// 本文件覆盖管理接口：AccountView 的 cooldown_reason（仅冷却中输出）、
// PATCH /v0/management/accounts 的 clear_cooldown（可与 disabled、priority 组合；三者都缺或 clear_cooldown 为 false 时 400），
// 解除后账号立即可选、计数清零、删除持久化记录；禁用与启用不影响冷却状态。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// cooldownPatch 通过管理接口发送 PATCH，返回响应。
func cooldownPatch(p *Pool, body string) *httptest.ResponseRecorder {
	return mgmtCall(p, "PATCH", "/v0/management/accounts", body, mgmtKey)
}

// cooldownDecodeView 解析 PATCH 返回的账号视图；响应必须是 200。
func cooldownDecodeView(t *testing.T, w *httptest.ResponseRecorder) AccountView {
	t.Helper()
	var view AccountView
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &view) != nil {
		t.Fatalf("PATCH status=%d body=%s, want 200 与账号视图", w.Code, w.Body.String())
	}
	return view
}

// TestCooldownAccountViewReportsReason 账号列表中冷却账号带 cooldown_reason 与 cooldown_until；
// 未冷却账号两个字段都不输出（与 cooldown_until 一致，仅冷却中输出）。
func TestCooldownAccountViewReportsReason(t *testing.T) {
	f := newCooldownFixture(t, "a", "b")
	f.accounts[0].Priority = 100
	f.up.pool.failGeneration("oauth-a", poolFailure{http.StatusForbidden, cooldownBody(403)})
	p := f.start()
	cooldownChat(p) // a 冷却

	w := mgmtCall(p, "GET", "/v0/management/accounts", "", mgmtKey)
	var items []map[string]any
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &items) != nil {
		t.Fatalf("GET accounts status=%d body=%s", w.Code, w.Body.String())
	}
	seen := map[string]bool{}
	for _, item := range items {
		id, _ := item["id"].(string)
		seen[id] = true
		switch id {
		case "a":
			if item["cooldown_reason"] != "403" || item["cooldown_until"] == nil {
				t.Errorf("冷却账号 a 的视图 cooldown_reason=%v cooldown_until=%v, want 403 与截止时间", item["cooldown_reason"], item["cooldown_until"])
			}
		case "b":
			if _, ok := item["cooldown_reason"]; ok {
				t.Errorf("未冷却账号 b 不应输出 cooldown_reason: %v", item)
			}
			if _, ok := item["cooldown_until"]; ok {
				t.Errorf("未冷却账号 b 不应输出 cooldown_until: %v", item)
			}
		}
	}
	if !seen["a"] || !seen["b"] {
		t.Fatalf("账号列表缺少 a 或 b: %v", items)
	}
}

// TestPatchClearCooldownUnlocksAccountAndResetsCount PATCH clear_cooldown 立即解除冷却：返回的视图不再冷却，
// 持久化记录被删除（重启后仍不冷却），账号立即可选，计数清零（解除后两次失败不冷却，第 3 次才冷却）。
func TestPatchClearCooldownUnlocksAccountAndResetsCount(t *testing.T) {
	f := newCooldownFixture(t, "a")
	f.up.pool.failGeneration("oauth-a", poolFailure{http.StatusInternalServerError, cooldownBody(500)})
	p := f.start()
	for range 3 {
		cooldownChat(p)
	}
	if cooldownView(t, p, "a").CooldownUntil == nil {
		t.Fatal("前提不成立：3 次 500 后应冷却")
	}
	// 进入冷却的记录写入是异步的：等它写完再解除，否则迟到的写入会干扰“记录已删除”的断言。
	cooldownAwaitRecord(t, p, "a")

	view := cooldownDecodeView(t, cooldownPatch(p, `{"id":"a","clear_cooldown":true}`))
	if view.CooldownUntil != nil || view.CooldownReason != "" {
		t.Fatalf("解除后视图仍冷却: until=%v reason=%q", view.CooldownUntil, view.CooldownReason)
	}
	if !cooldownRecordGone(t, cooldownFile(t, p, "a", "cooldown")) {
		t.Fatal("解除冷却后 cooldown.json 应被删除（或为空）")
	}

	// 重启后仍不冷却：持久化记录已删除。
	restarted := f.start()
	if v := cooldownView(t, restarted, "a"); v.CooldownUntil != nil {
		t.Fatalf("解除后重启仍冷却至 %s", v.CooldownUntil)
	}

	// 当前进程中账号立即可选：请求由 a 处理（上游返回 500，而不是 503）。这是解除后的第 1 次失败。
	if w := cooldownChat(p); w.Code != http.StatusInternalServerError {
		t.Fatalf("解除后请求 status=%d body=%s, want 由 a 处理得到上游的 500", w.Code, w.Body.String())
	}
	// 计数清零：解除前的 3 次失败不计入，解除后第 2 次失败仍不冷却。
	cooldownChat(p)
	if cooldownView(t, p, "a").CooldownUntil != nil {
		t.Fatal("解除后第 2 次失败就冷却：计数未清零")
	}
	// 解除后第 3 次失败才冷却。
	cooldownChat(p)
	if cooldownView(t, p, "a").CooldownUntil == nil {
		t.Fatal("解除后第 3 次失败应冷却")
	}
}

// TestPatchClearCooldownCombinesWithOtherFields clear_cooldown 可与 priority、disabled 同时出现，三者各自生效。
func TestPatchClearCooldownCombinesWithOtherFields(t *testing.T) {
	for _, tc := range []struct {
		name  string
		body  string
		check func(AccountView) bool
		want  string
	}{
		{
			name:  "with-priority",
			body:  `{"id":"a","clear_cooldown":true,"priority":7}`,
			check: func(v AccountView) bool { return v.Priority == 7 && !v.Disabled },
			want:  "priority 7、未禁用",
		},
		{
			name:  "with-disabled",
			body:  `{"id":"a","clear_cooldown":true,"disabled":true}`,
			check: func(v AccountView) bool { return v.Disabled },
			want:  "已禁用",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCooldownFixture(t, "a")
			f.up.pool.failGeneration("oauth-a", poolFailure{http.StatusForbidden, cooldownBody(403)})
			p := f.start()
			cooldownChat(p)
			if cooldownView(t, p, "a").CooldownUntil == nil {
				t.Fatal("前提不成立：403 后账号应冷却")
			}

			view := cooldownDecodeView(t, cooldownPatch(p, tc.body))
			if view.CooldownUntil != nil || view.CooldownReason != "" {
				t.Fatalf("组合 PATCH 后仍冷却: until=%v reason=%q", view.CooldownUntil, view.CooldownReason)
			}
			if !tc.check(view) {
				t.Fatalf("组合 PATCH 后视图 = %+v, want %s", view, tc.want)
			}
		})
	}
}

// TestPatchClearCooldownBadRequests clear_cooldown 为 false 视为未提供；id 缺失，或 disabled、priority、clear_cooldown 都缺失时返回 400，
// 且冷却保持不变。
func TestPatchClearCooldownBadRequests(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "clear-false-only", body: `{"id":"a","clear_cooldown":false}`},
		{name: "nothing-else", body: `{"id":"a"}`},
		{name: "missing-id", body: `{"clear_cooldown":true}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCooldownFixture(t, "a")
			f.up.pool.failGeneration("oauth-a", poolFailure{http.StatusForbidden, cooldownBody(403)})
			p := f.start()
			cooldownChat(p)

			if w := cooldownPatch(p, tc.body); w.Code != http.StatusBadRequest {
				t.Fatalf("PATCH %s status=%d body=%s, want 400", tc.body, w.Code, w.Body.String())
			}
			if v := cooldownView(t, p, "a"); v.CooldownUntil == nil || v.CooldownReason != "403" {
				t.Fatalf("被拒绝的 PATCH 不应改变冷却: until=%v reason=%q", v.CooldownUntil, v.CooldownReason)
			}
		})
	}
}

// TestPatchClearCooldownUnknownAccountIsNotFound 解除未知账号的冷却返回 404。
func TestPatchClearCooldownUnknownAccountIsNotFound(t *testing.T) {
	f := newCooldownFixture(t, "a")
	p := f.start()
	if w := cooldownPatch(p, `{"id":"nobody","clear_cooldown":true}`); w.Code != http.StatusNotFound {
		t.Fatalf("未知账号 status=%d body=%s, want 404", w.Code, w.Body.String())
	}
}

// TestPatchDisabledAndPriorityKeepCooldown 禁用、启用与调整优先级不影响冷却状态与原因。
func TestPatchDisabledAndPriorityKeepCooldown(t *testing.T) {
	f := newCooldownFixture(t, "a")
	f.up.pool.failGeneration("oauth-a", poolFailure{http.StatusForbidden, cooldownBody(403)})
	p := f.start()
	cooldownChat(p)

	for _, body := range []string{
		`{"id":"a","disabled":true}`,
		`{"id":"a","disabled":false}`,
		`{"id":"a","priority":5}`,
	} {
		view := cooldownDecodeView(t, cooldownPatch(p, body))
		if view.CooldownUntil == nil || view.CooldownReason != "403" {
			t.Fatalf("PATCH %s 后冷却状态 until=%v reason=%q, want 保持 403 冷却", body, view.CooldownUntil, view.CooldownReason)
		}
	}
}

// TestPatchClearCooldownKeepsCooldownWhenRecordCannotBeDeleted 解除冷却时删除 cooldown.json 失败：
// 返回 500，账号保持冷却（截止时间与原因不变）；修复后再次解除成功。失败用非空目录替换记录文件来制造（删除非空目录会失败）。
func TestPatchClearCooldownKeepsCooldownWhenRecordCannotBeDeleted(t *testing.T) {
	f := newCooldownFixture(t, "a")
	f.up.pool.failGeneration("oauth-a", poolFailure{http.StatusUnauthorized, cooldownBody(401)})
	p := f.start()
	cooldownChat(p)
	cooldownAwaitRecord(t, p, "a")
	before := cooldownView(t, p, "a")
	if before.CooldownUntil == nil {
		t.Fatal("前提不成立：401 后账号应冷却")
	}

	path := cooldownFile(t, p, "a", "cooldown")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "keep"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	if w := cooldownPatch(p, `{"id":"a","clear_cooldown":true}`); w.Code != http.StatusInternalServerError {
		t.Fatalf("删除冷却记录失败时 PATCH status=%d body=%s, want 500", w.Code, w.Body.String())
	}
	after := cooldownView(t, p, "a")
	if after.CooldownUntil == nil || !after.CooldownUntil.Equal(*before.CooldownUntil) || after.CooldownReason != "401" {
		t.Fatalf("删除失败后冷却状态 until=%v reason=%q, want 保持 %v 与原因 401", after.CooldownUntil, after.CooldownReason, before.CooldownUntil)
	}
	if w := cooldownChat(p); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("删除失败后账号仍应冷却（503），实际 status=%d", w.Code)
	}

	// 修复后再次解除：成功，并清除内存冷却。
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	if view := cooldownDecodeView(t, cooldownPatch(p, `{"id":"a","clear_cooldown":true}`)); view.CooldownUntil != nil {
		t.Fatalf("修复后解除仍冷却至 %s", view.CooldownUntil)
	}
}

// TestPatchCredentialWriteFailureChangesNothing 凭证写入失败时，PATCH 返回 500，冷却、禁用状态与 cooldown.json 全部保持不变：
// 凭证的写入先于解除冷却，因此失败时不会出现“冷却已解除、凭证未更新”的半完成状态。
// 凭证文件无法写入用目录替换文件来制造（读取目录作为凭证会失败）。
func TestPatchCredentialWriteFailureChangesNothing(t *testing.T) {
	f := newCooldownFixture(t, "a")
	f.up.pool.failGeneration("oauth-a", poolFailure{http.StatusUnauthorized, cooldownBody(401)})
	p := f.start()
	cooldownChat(p)
	cooldownAwaitRecord(t, p, "a")
	before := cooldownView(t, p, "a")
	if before.CooldownUntil == nil {
		t.Fatal("前提不成立：401 后账号应冷却")
	}

	credPath := filepath.Join(f.config.CredentialsDir, "a.json")
	if err := os.Remove(credPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(credPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(credPath, "keep"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	if w := cooldownPatch(p, `{"id":"a","disabled":true,"clear_cooldown":true}`); w.Code != http.StatusInternalServerError {
		t.Fatalf("凭证写入失败时 PATCH status=%d body=%s, want 500", w.Code, w.Body.String())
	}
	after := cooldownView(t, p, "a")
	if after.CooldownUntil == nil || !after.CooldownUntil.Equal(*before.CooldownUntil) || after.CooldownReason != "401" {
		t.Fatalf("凭证写入失败后冷却状态 until=%v reason=%q, want 保持 %v 与原因 401", after.CooldownUntil, after.CooldownReason, before.CooldownUntil)
	}
	if after.Disabled {
		t.Fatal("凭证写入失败后禁用状态不应生效")
	}
	if _, err := os.Stat(cooldownFile(t, p, "a", "cooldown")); err != nil {
		t.Fatalf("凭证写入失败后 cooldown.json 应仍在: %v", err)
	}

	// 修复凭证后同一请求可以成功。
	if err := os.RemoveAll(credPath); err != nil {
		t.Fatal(err)
	}
	if err := SaveCredential(credPath, Credential{Token: "oauth-a", User: "a"}); err != nil {
		t.Fatal(err)
	}
	view := cooldownDecodeView(t, cooldownPatch(p, `{"id":"a","disabled":true,"clear_cooldown":true}`))
	if view.CooldownUntil != nil || !view.Disabled {
		t.Fatalf("修复后解除并禁用 view = %+v, want 未冷却且已禁用", view)
	}
}

// TestPatchLiftFailureAfterCredentialUpdateKeepsDisabled 凭证已更新、但解除冷却失败（cooldown.json 无法删除）时，
// PATCH 返回 500，错误信息说明“凭证已更新”；禁用状态已生效并写入凭证，冷却保持不变。
func TestPatchLiftFailureAfterCredentialUpdateKeepsDisabled(t *testing.T) {
	f := newCooldownFixture(t, "a")
	f.up.pool.failGeneration("oauth-a", poolFailure{http.StatusUnauthorized, cooldownBody(401)})
	p := f.start()
	cooldownChat(p)
	cooldownAwaitRecord(t, p, "a")

	recordPath := cooldownFile(t, p, "a", "cooldown")
	if err := os.Remove(recordPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(recordPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(recordPath, "keep"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	w := cooldownPatch(p, `{"id":"a","disabled":true,"clear_cooldown":true}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("解除失败时 PATCH status=%d body=%s, want 500", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "凭证已更新") {
		t.Fatalf("错误信息应说明凭证已更新: %s", w.Body.String())
	}
	view := cooldownView(t, p, "a")
	if !view.Disabled {
		t.Fatal("解除失败后禁用状态应已生效")
	}
	if view.CooldownUntil == nil || view.CooldownReason != "401" {
		t.Fatalf("解除失败后冷却应保持 until=%v reason=%q", view.CooldownUntil, view.CooldownReason)
	}
	cred, err := LoadCredential(filepath.Join(f.config.CredentialsDir, "a.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !cred.Disabled {
		t.Fatal("凭证文件应已记录 disabled")
	}
}

// TestPatchClearCooldownDoesNotWriteUnchangedCredential 只有 clear_cooldown，或 disabled、priority 与当前值相同时，PATCH 不写凭证：
// 凭证文件即使无法写入（用目录替换文件）也返回 200 并解除冷却。
func TestPatchClearCooldownDoesNotWriteUnchangedCredential(t *testing.T) {
	for _, body := range []string{
		`{"id":"a","clear_cooldown":true}`,
		`{"id":"a","disabled":false,"priority":0,"clear_cooldown":true}`,
	} {
		t.Run(body, func(t *testing.T) {
			f := newCooldownFixture(t, "a")
			f.up.pool.failGeneration("oauth-a", poolFailure{http.StatusUnauthorized, cooldownBody(401)})
			p := f.start()
			cooldownChat(p)
			cooldownAwaitRecord(t, p, "a")

			credPath := filepath.Join(f.config.CredentialsDir, "a.json")
			if err := os.Remove(credPath); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(credPath, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(credPath, "keep"), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}

			if view := cooldownDecodeView(t, cooldownPatch(p, body)); view.CooldownUntil != nil {
				t.Fatalf("解除后仍冷却至 %s", view.CooldownUntil)
			}
		})
	}
}
