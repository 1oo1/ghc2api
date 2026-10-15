package copilot

// 本文件实现会话绑定的持久化：关闭服务时把未过期的绑定写入缓存根目录，启动时读回。
// 绑定文件只含会话键（已是哈希）、账号 ID 与截止时刻，不含请求内容。

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// sessionAffinityFileName 是绑定文件在缓存根目录下的文件名。
const sessionAffinityFileName = "session_affinity.json"

// affinityFileVersion 是绑定文件的格式版本；版本不符时整个文件视为无效。
const affinityFileVersion = 1

// affinityFile 是绑定文件的 JSON 结构。
type affinityFile struct {
	Version  int                 `json:"version"`
	Bindings []affinityFileEntry `json:"bindings"`
}

// affinityFileEntry 是绑定文件中的一条绑定。
type affinityFileEntry struct {
	Key     string    `json:"key"`
	Account string    `json:"account"`
	Expires time.Time `json:"expires"`
}

// save 把 now 时刻仍有效的绑定原子写入 path，文件权限为 0600。已过期（截止时刻及之后）的绑定不写入。
// 在锁内只复制数据，写文件在锁外进行。
func (t *affinityTable) save(path string, now time.Time) error {
	t.mu.Lock()
	file := affinityFile{Version: affinityFileVersion, Bindings: make([]affinityFileEntry, 0, len(t.bindings))}
	for key, binding := range t.bindings {
		if now.Before(binding.expires) {
			file.Bindings = append(file.Bindings, affinityFileEntry{
				Key: key, Account: binding.accountID, Expires: binding.expires,
			})
		}
	}
	t.mu.Unlock()
	return writeCacheFile(path, file)
}

// load 从 path 读取绑定并替换表的全部内容。文件不存在时表为空且返回 nil；文件无效时返回错误，表保持不变。
// 加载规则见 restoreBindings。
func (t *affinityTable) load(path string, now time.Time) error {
	bindings := make(map[string]affinityBinding)
	var file affinityFile
	switch err := readCacheFile(path, &file); {
	case errors.Is(err, os.ErrNotExist):
		// 尚无绑定文件（首次启动），表保持为空。
	case err != nil:
		return err
	case file.Version != affinityFileVersion:
		return fmt.Errorf("绑定文件版本 %d 不受支持", file.Version)
	default:
		bindings = restoreBindings(file.Bindings, now, t.ttl)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.bindings = bindings
	return nil
}

// restoreBindings 把文件中的绑定还原为表内容。丢弃 now 时刻已过期（截止时刻及之后）与 key 或账号 ID 为空的项；
// 每条有效期封顶于 now+ttl；条数超过 maxSessionBindings 时只保留截止时刻最晚的 maxSessionBindings 条。
func restoreBindings(entries []affinityFileEntry, now time.Time, ttl time.Duration) map[string]affinityBinding {
	ceiling := now.Add(ttl)
	bindings := make(map[string]affinityBinding, len(entries))
	for _, entry := range entries {
		if entry.Key == "" || entry.Account == "" || !now.Before(entry.Expires) {
			continue
		}
		expires := entry.Expires
		if expires.After(ceiling) {
			expires = ceiling
		}
		bindings[entry.Key] = affinityBinding{accountID: entry.Account, expires: expires}
	}
	if len(bindings) > maxSessionBindings {
		keepLatestBindings(bindings)
	}
	return bindings
}

// keepLatestBindings 删除多余的条目，只保留 maxSessionBindings 条：截止时刻越晚越优先保留；截止时刻并列时键字典序小者优先。
func keepLatestBindings(bindings map[string]affinityBinding) {
	keys := make([]string, 0, len(bindings))
	for key := range bindings {
		keys = append(keys, key)
	}
	// 排序后前面的是应保留的条目，末尾的是应删除的条目。
	slices.SortFunc(keys, func(a, b string) int {
		ea, eb := bindings[a].expires, bindings[b].expires
		switch {
		case ea.After(eb):
			return -1
		case ea.Before(eb):
			return 1
		default:
			return strings.Compare(a, b)
		}
	})
	for _, key := range keys[maxSessionBindings:] {
		delete(bindings, key)
	}
}

// SaveSessionAffinity 把未过期的会话绑定保存到 <CacheDir>/session_affinity.json。
// 未开启会话亲和或 CacheDir 为空时不做任何事。应在 HTTP 服务关闭之后调用；
// 若关闭超时被强制中断，仍在途的请求可能与保存并发，其绑定可能不会被保存。
func (p *Pool) SaveSessionAffinity() error {
	if p.affinity == nil || p.config.CacheDir == "" {
		return nil
	}
	return p.affinity.save(filepath.Join(p.config.CacheDir, sessionAffinityFileName), time.Now())
}

// loadSessionAffinity 在池创建时读取绑定文件。加载失败只输出 Warn，绑定表保持为空，池照常启动。
func (p *Pool) loadSessionAffinity() {
	if p.affinity == nil || p.config.CacheDir == "" {
		return
	}
	path := filepath.Join(p.config.CacheDir, sessionAffinityFileName)
	if err := p.affinity.load(path, time.Now()); err != nil {
		p.logger.Warn("会话亲和：绑定文件无法加载，已以空表启动", "error", err)
	}
}
