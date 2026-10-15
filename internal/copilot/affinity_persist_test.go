package copilot

// 本文件覆盖绑定表的持久化：save 只写入未过期的绑定；load 丢弃过期与无效项、把有效期封顶为 加载时刻+当前 TTL，
// 超过 maxSessionBindings 时保留截止时刻最晚的条目。所有“当前时刻”都由测试显式传入，不依赖真实时钟。

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// affinityPersistBase 是持久化测试的固定基准时刻。
var affinityPersistBase = time.Date(2026, time.October, 15, 12, 0, 0, 0, time.UTC)

// affinityPersistPath 返回临时目录中的绑定文件路径，文件尚未创建。
func affinityPersistPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "session_affinity.json")
}

// affinityPersistSave 以 now 把绑定表保存到 path；保存失败时终止测试。
func affinityPersistSave(t *testing.T, table *affinityTable, path string, now time.Time) {
	t.Helper()
	if err := table.save(path, now); err != nil {
		t.Fatalf("save() error = %v", err)
	}
}

// affinityPersistLoad 创建 TTL 为 ttl 的新表，并以 now 加载 path；加载失败时终止测试。
func affinityPersistLoad(t *testing.T, path string, ttl time.Duration, now time.Time) *affinityTable {
	t.Helper()
	table := newAffinityTable(ttl)
	if err := table.load(path, now); err != nil {
		t.Fatalf("load() error = %v", err)
	}
	return table
}

// assertSameAffinityLookups 断言两张表在每个探测时刻、对每个键的查询结果（账号 ID 与是否命中）都一致。
func assertSameAffinityLookups(t *testing.T, want, got *affinityTable, keys []string, probes ...time.Time) {
	t.Helper()
	for _, key := range keys {
		for _, at := range probes {
			wantID, wantOK := want.lookup(key, at)
			gotID, gotOK := got.lookup(key, at)
			if wantID != gotID || wantOK != gotOK {
				t.Fatalf("键 %q 在 %s 的查询 = %q, %v; want %q, %v", key, at.Format(time.RFC3339), gotID, gotOK, wantID, wantOK)
			}
		}
	}
}

// TestSessionBindingLimitIs500 会话绑定表的上限为 500 条。
func TestSessionBindingLimitIs500(t *testing.T) {
	if maxSessionBindings != 500 {
		t.Fatalf("maxSessionBindings = %d, want 500", maxSessionBindings)
	}
}

// TestAffinityPersistRoundTrip 未过期的绑定保存后加载到新表，条数与每个键在各时刻的查询结果都与原表一致。
func TestAffinityPersistRoundTrip(t *testing.T) {
	const ttl = time.Hour
	table := newAffinityTable(ttl)
	keys := []string{"session-0", "session-1", "session-2", "session-3", "session-4"}
	for i, key := range keys {
		account := "alice"
		if i%2 == 1 {
			account = "bob"
		}
		// 第 i 条的截止时刻为 基准+i 分钟+TTL，各不相同。
		table.bind(key, account, affinityPersistBase.Add(time.Duration(i)*time.Minute))
	}
	path := affinityPersistPath(t)
	now := affinityPersistBase.Add(10 * time.Minute)
	affinityPersistSave(t, table, path, now)

	loaded := affinityPersistLoad(t, path, ttl, now)
	if got := loaded.count(); got != len(keys) {
		t.Fatalf("加载后 count() = %d, want %d", got, len(keys))
	}
	probes := []time.Time{
		now,
		affinityPersistBase.Add(61 * time.Minute),
		affinityPersistBase.Add(63 * time.Minute),
		affinityPersistBase.Add(64 * time.Minute),
	}
	assertSameAffinityLookups(t, table, loaded, keys, probes...)
}

// TestAffinityPersistSkipsExpiredBindings 保存时已过期（截止时刻及之后）的绑定不写入文件，加载后不可查询。
func TestAffinityPersistSkipsExpiredBindings(t *testing.T) {
	const ttl = time.Hour
	table := newAffinityTable(ttl)
	table.bind("long-expired", "alice", affinityPersistBase.Add(-2*time.Hour)) // 截止于基准前 1 小时
	table.bind("on-deadline", "alice", affinityPersistBase.Add(-ttl))          // 截止时刻恰为基准，已过期
	table.bind("live", "bob", affinityPersistBase)                             // 截止于基准后 1 小时
	path := affinityPersistPath(t)
	affinityPersistSave(t, table, path, affinityPersistBase)

	loaded := affinityPersistLoad(t, path, ttl, affinityPersistBase)
	if got := loaded.count(); got != 1 {
		t.Fatalf("加载后 count() = %d, want 1（只有 live）", got)
	}
	if id, ok := loaded.lookup("live", affinityPersistBase); !ok || id != "bob" {
		t.Fatalf("未过期的绑定 lookup = %q, %v; want bob, true", id, ok)
	}
	// 若保存时写入了已过期的绑定，在更早的时刻加载时它们仍然有效，条数会超过 1。
	early := affinityPersistBase.Add(-3 * time.Hour)
	if got := affinityPersistLoad(t, path, ttl, early).count(); got != 1 {
		t.Fatalf("保存时不应写入已过期的绑定：在 %s 加载得到 %d 条, want 1", early.Format(time.RFC3339), got)
	}
}

// TestAffinityPersistLoadDropsBindingsAtDeadline 加载时截止时刻本身即视为过期：截止前 1 纳秒仍有效，截止时刻起失效。
func TestAffinityPersistLoadDropsBindingsAtDeadline(t *testing.T) {
	const ttl = time.Hour
	table := newAffinityTable(ttl)
	table.bind("edge", "alice", affinityPersistBase) // 截止于 基准+1 小时
	path := affinityPersistPath(t)
	affinityPersistSave(t, table, path, affinityPersistBase)

	deadline := affinityPersistBase.Add(ttl)
	justBefore := affinityPersistLoad(t, path, ttl, deadline.Add(-time.Nanosecond))
	if id, ok := justBefore.lookup("edge", deadline.Add(-time.Nanosecond)); !ok || id != "alice" {
		t.Fatalf("截止前 1 纳秒加载的 lookup = %q, %v; want alice, true", id, ok)
	}
	atDeadline := affinityPersistLoad(t, path, ttl, deadline)
	if got := atDeadline.count(); got != 0 {
		t.Fatalf("截止时刻加载后 count() = %d, want 0", got)
	}
	if _, ok := atDeadline.lookup("edge", deadline); ok {
		t.Fatal("截止时刻的绑定仍然有效")
	}
}

// TestAffinityPersistLoadCapsExpiryAtCurrentTTL 加载后的有效期为 min(文件中的截止时刻, 加载时刻+当前 TTL)：
// 由 TTL 较长的表保存、用 TTL 较短的表加载时，有效期按新 TTL 收紧；新 TTL 较长时，仍不超过文件中的截止时刻。
func TestAffinityPersistLoadCapsExpiryAtCurrentTTL(t *testing.T) {
	old := newAffinityTable(2 * time.Hour)
	old.bind("session", "alice", affinityPersistBase) // 截止于 基准+2 小时
	path := affinityPersistPath(t)
	affinityPersistSave(t, old, path, affinityPersistBase)

	loadedAt := affinityPersistBase.Add(10 * time.Minute)
	shorter := affinityPersistLoad(t, path, 30*time.Minute, loadedAt) // 有效期应封顶于 加载时刻+30 分钟，即 基准+40 分钟
	if id, ok := shorter.lookup("session", affinityPersistBase.Add(39*time.Minute)); !ok || id != "alice" {
		t.Fatalf("封顶前的时刻 lookup = %q, %v; want alice, true", id, ok)
	}
	if _, ok := shorter.lookup("session", affinityPersistBase.Add(40*time.Minute)); ok {
		t.Fatal("有效期未按加载时刻+当前 TTL 封顶")
	}

	longer := affinityPersistLoad(t, path, 2*time.Hour, loadedAt) // 新 TTL 更长时，仍以文件中的截止时刻为准
	if _, ok := longer.lookup("session", affinityPersistBase.Add(2*time.Hour-time.Minute)); !ok {
		t.Fatal("新 TTL 较长时，文件中的截止前绑定应有效")
	}
	if _, ok := longer.lookup("session", affinityPersistBase.Add(2*time.Hour)); ok {
		t.Fatal("新 TTL 较长时，有效期超过了文件中的截止时刻")
	}
}

// TestAffinityPersistMissingFileIsEmpty 文件不存在时 load 返回 nil、表为空，且不会创建该文件。
func TestAffinityPersistMissingFileIsEmpty(t *testing.T) {
	path := affinityPersistPath(t)
	table := newAffinityTable(time.Hour)
	if err := table.load(path, affinityPersistBase); err != nil {
		t.Fatalf("文件不存在时 load() error = %v, want nil", err)
	}
	if got := table.count(); got != 0 {
		t.Fatalf("文件不存在时 count() = %d, want 0", got)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("load 不应创建绑定文件，stat error = %v", err)
	}
}

// TestAffinityPersistCorruptFileFailsWithEmptyTable 文件不是合法 JSON 时 load 返回非 nil 错误，且表保持为空。
func TestAffinityPersistCorruptFileFailsWithEmptyTable(t *testing.T) {
	for _, tc := range []struct{ name, contents string }{
		{name: "not-json", contents: "not json"},
		{name: "truncated", contents: `{"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := affinityPersistPath(t)
			if err := os.WriteFile(path, []byte(tc.contents), 0o600); err != nil {
				t.Fatal(err)
			}
			table := newAffinityTable(time.Hour)
			if err := table.load(path, affinityPersistBase); err == nil {
				t.Fatal("损坏的绑定文件 load() 未返回错误")
			}
			if got := table.count(); got != 0 {
				t.Fatalf("损坏文件加载失败后 count() = %d, want 0", got)
			}
			if _, ok := table.lookup("anything", affinityPersistBase); ok {
				t.Fatal("损坏文件加载失败后表中仍有绑定")
			}
		})
	}
}

// TestAffinityPersistFileIsPrivate 绑定文件权限为 0600（非 Windows），与其他缓存文件一致。
func TestAffinityPersistFileIsPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 不支持 Unix 文件权限")
	}
	table := newAffinityTable(time.Hour)
	table.bind("session", "alice", affinityPersistBase)
	path := affinityPersistPath(t)
	affinityPersistSave(t, table, path, affinityPersistBase)

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat 绑定文件: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("绑定文件权限 = %04o, want 0600", got)
	}
}

// TestAffinityPersistOverwritesAtomically 重复保存会覆盖旧内容，且目录中不留下临时文件。
func TestAffinityPersistOverwritesAtomically(t *testing.T) {
	path := affinityPersistPath(t)
	first := newAffinityTable(time.Hour)
	first.bind("first", "alice", affinityPersistBase)
	affinityPersistSave(t, first, path, affinityPersistBase)

	second := newAffinityTable(time.Hour)
	second.bind("second", "bob", affinityPersistBase)
	affinityPersistSave(t, second, path, affinityPersistBase)

	loaded := affinityPersistLoad(t, path, time.Hour, affinityPersistBase)
	if got := loaded.count(); got != 1 {
		t.Fatalf("覆盖保存后 count() = %d, want 1", got)
	}
	if _, ok := loaded.lookup("first", affinityPersistBase); ok {
		t.Fatal("覆盖保存后仍能查到旧绑定")
	}
	if id, ok := loaded.lookup("second", affinityPersistBase); !ok || id != "bob" {
		t.Fatalf("覆盖保存后的 lookup = %q, %v; want bob, true", id, ok)
	}

	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(path) {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("保存后目录内容 = %v, want 仅 %s（不应留下临时文件）", names, filepath.Base(path))
	}
}

// TestAffinityPersistSaveFailureReturnsError 父路径不是目录时 save 返回错误。
func TestAffinityPersistSaveFailureReturnsError(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	table := newAffinityTable(time.Hour)
	table.bind("session", "alice", affinityPersistBase)
	if err := table.save(filepath.Join(blocker, "session_affinity.json"), affinityPersistBase); err == nil {
		t.Fatal("父路径不是目录时 save() 未返回错误")
	}
}

// TestAffinityPersistDropsBlankKeyOrAccount 空键或空账号 ID 的绑定不会被加载，其他绑定正常保留。
func TestAffinityPersistDropsBlankKeyOrAccount(t *testing.T) {
	table := newAffinityTable(time.Hour)
	expires := affinityPersistBase.Add(time.Hour)
	table.bindings[""] = affinityBinding{accountID: "alice", expires: expires}
	table.bindings["blank-account"] = affinityBinding{accountID: "", expires: expires}
	table.bindings["valid"] = affinityBinding{accountID: "alice", expires: expires}
	path := affinityPersistPath(t)
	affinityPersistSave(t, table, path, affinityPersistBase)

	loaded := affinityPersistLoad(t, path, time.Hour, affinityPersistBase)
	if got := loaded.count(); got != 1 {
		t.Fatalf("加载后 count() = %d, want 1（只有 valid）", got)
	}
	if id, ok := loaded.lookup("valid", affinityPersistBase); !ok || id != "alice" {
		t.Fatalf("有效绑定 lookup = %q, %v; want alice, true", id, ok)
	}
	if _, ok := loaded.lookup("blank-account", affinityPersistBase); ok {
		t.Fatal("空账号 ID 的绑定被加载")
	}
}

// TestAffinityPersistRoundTripAtCapacity 恰好 maxSessionBindings 条时，保存与加载都不丢弃任何绑定。
func TestAffinityPersistRoundTripAtCapacity(t *testing.T) {
	const ttl = 24 * time.Hour
	table := newAffinityTable(ttl)
	keys := make([]string, 0, maxSessionBindings)
	for i := range maxSessionBindings {
		key := fmt.Sprintf("key-%04d", i)
		keys = append(keys, key)
		table.bind(key, "alice", affinityPersistBase.Add(time.Duration(i)*time.Second))
	}
	path := affinityPersistPath(t)
	affinityPersistSave(t, table, path, affinityPersistBase)

	loaded := affinityPersistLoad(t, path, ttl, affinityPersistBase)
	if got := loaded.count(); got != maxSessionBindings {
		t.Fatalf("恰好上限时加载后 count() = %d, want %d", got, maxSessionBindings)
	}
	for _, key := range keys {
		if _, ok := loaded.lookup(key, affinityPersistBase); !ok {
			t.Fatalf("恰好上限时绑定 %q 被丢弃", key)
		}
	}
}

// TestAffinityPersistLoadKeepsLatestExpiringWhenOverLimit 超过 maxSessionBindings 条时，加载只保留截止时刻最晚的 maxSessionBindings 条。
func TestAffinityPersistLoadKeepsLatestExpiringWhenOverLimit(t *testing.T) {
	const dropped = 10
	const total = maxSessionBindings + dropped
	const ttl = 24 * time.Hour
	table := newAffinityTable(ttl)
	// 直接写入 bindings 以构造超过上限的状态（绕过 bind 的淘汰）：key-0000 截止最早，key-(total-1) 截止最晚。
	for i := range total {
		table.bindings[fmt.Sprintf("key-%04d", i)] = affinityBinding{
			accountID: "alice", expires: affinityPersistBase.Add(time.Duration(i+1) * time.Minute),
		}
	}
	path := affinityPersistPath(t)
	affinityPersistSave(t, table, path, affinityPersistBase)

	loaded := affinityPersistLoad(t, path, ttl, affinityPersistBase)
	if got := loaded.count(); got != maxSessionBindings {
		t.Fatalf("超限加载后 count() = %d, want %d", got, maxSessionBindings)
	}
	for i := range total {
		key := fmt.Sprintf("key-%04d", i)
		_, ok := loaded.lookup(key, affinityPersistBase)
		if want := i >= dropped; ok != want {
			t.Fatalf("%s 的保留状态 = %v, want %v（应保留截止时刻最晚的 %d 条）", key, ok, want, maxSessionBindings)
		}
	}
}

// TestAffinityPersistLoadTieBreaksByKeyWhenOverLimit 超限时若最早截止的条目并列，按键字典序取小者优先保留。
func TestAffinityPersistLoadTieBreaksByKeyWhenOverLimit(t *testing.T) {
	const ttl = 24 * time.Hour
	table := newAffinityTable(ttl)
	shared := affinityPersistBase.Add(time.Hour)
	// 两条并列最早截止，共 501 条，因此只能保留 500 条中的一条：应保留 tie-a，丢弃 tie-b。
	table.bindings["tie-b"] = affinityBinding{accountID: "alice", expires: shared}
	table.bindings["tie-a"] = affinityBinding{accountID: "alice", expires: shared}
	for i := range maxSessionBindings - 1 {
		table.bindings[fmt.Sprintf("key-%04d", i)] = affinityBinding{
			accountID: "alice", expires: affinityPersistBase.Add(2*time.Hour + time.Duration(i)*time.Second),
		}
	}
	path := affinityPersistPath(t)
	affinityPersistSave(t, table, path, affinityPersistBase)

	loaded := affinityPersistLoad(t, path, ttl, affinityPersistBase)
	if got := loaded.count(); got != maxSessionBindings {
		t.Fatalf("超限加载后 count() = %d, want %d", got, maxSessionBindings)
	}
	if _, ok := loaded.lookup("tie-a", affinityPersistBase); !ok {
		t.Fatal("截止时刻并列时，键字典序较小的 tie-a 应被保留")
	}
	if _, ok := loaded.lookup("tie-b", affinityPersistBase); ok {
		t.Fatal("截止时刻并列时，键字典序较大的 tie-b 应被丢弃")
	}
}
