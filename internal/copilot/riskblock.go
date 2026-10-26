package copilot

// 本文件实现风控记录：生成请求的 401/403 使账号进入冷却时，记录该请求的会话键与状态码；
// 之后同一会话键的请求在选账号之前直接返回 500，消息中附记录的原始状态码（见 Pool.forward），不再访问上游。
// 记录有效期固定为 riskBlockTTL（命中不续期），新增时立即写入 <CacheDir>/risk_blocks.json，池创建时读回。

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// riskBlockTTL 是一条风控记录的有效期。
const riskBlockTTL = 48 * time.Hour

// riskBlockMessage 是命中风控记录时返回给客户端的错误信息，其后附原始状态码。
const riskBlockMessage = "别再重试了，已经触发风控了"

// riskBlockFileName 是风控记录在缓存根目录下的文件名。
const riskBlockFileName = "risk_blocks.json"

// riskFileVersion 是风控记录文件的格式版本；版本不符时整个文件视为无效。
const riskFileVersion = 1

// riskTable 保存会话键到状态码的风控记录。所有方法并发安全。
// mu 同时覆盖写文件，使并发新增的保存按顺序落盘；记录只在 401/403 触发冷却时新增，频率很低。
type riskTable struct {
	mu     sync.Mutex
	blocks map[string]riskBlock
}

// riskBlock 是一条风控记录：触发冷却的状态码与截止时间（到达即失效）。
type riskBlock struct {
	status  int
	expires time.Time
}

// riskFile 是风控记录文件的 JSON 结构。
type riskFile struct {
	Version int             `json:"version"`
	Blocks  []riskFileEntry `json:"blocks"`
}

// riskFileEntry 是风控记录文件中的一条记录。
type riskFileEntry struct {
	Key     string    `json:"key"`
	Status  int       `json:"status"`
	Expires time.Time `json:"expires"`
}

// newRiskTable 创建空的风控记录表。
func newRiskTable() *riskTable {
	return &riskTable{blocks: make(map[string]riskBlock)}
}

// add 新建或覆盖 key 的记录，截止于 now+riskBlockTTL。
func (t *riskTable) add(key string, status int, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.blocks[key] = riskBlock{status: status, expires: now.Add(riskBlockTTL)}
}

// lookup 返回 key 记录的状态码；截止时刻及之后视为不存在。命中不续期。
func (t *riskTable) lookup(key string, now time.Time) (int, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	block, ok := t.blocks[key]
	if !ok || !now.Before(block.expires) {
		return 0, false
	}
	return block.status, true
}

// save 把 now 时刻仍有效的记录原子写入 path（权限 0600），并从内存中清理已过期的记录。
func (t *riskTable) save(path string, now time.Time) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	file := riskFile{Version: riskFileVersion, Blocks: make([]riskFileEntry, 0, len(t.blocks))}
	for key, block := range t.blocks {
		if !now.Before(block.expires) {
			delete(t.blocks, key)
			continue
		}
		file.Blocks = append(file.Blocks, riskFileEntry{Key: key, Status: block.status, Expires: block.expires})
	}
	return writeCacheFile(path, file)
}

// load 从 path 读取记录并替换表的全部内容。文件不存在时表为空且返回 nil；文件无效或版本不符时返回错误，表保持不变。
// 丢弃 now 时刻已过期的项、key 为空的项与状态码不是 401/403 的项；截止时刻封顶于 now+riskBlockTTL。
func (t *riskTable) load(path string, now time.Time) error {
	blocks := make(map[string]riskBlock)
	var file riskFile
	switch err := readCacheFile(path, &file); {
	case errors.Is(err, os.ErrNotExist):
		// 尚无记录文件，表保持为空。
	case err != nil:
		return err
	case file.Version != riskFileVersion:
		return fmt.Errorf("风控记录文件版本 %d 不受支持", file.Version)
	default:
		ceiling := now.Add(riskBlockTTL)
		for _, entry := range file.Blocks {
			if entry.Key == "" || !isRiskStatus(entry.Status) || !now.Before(entry.Expires) {
				continue
			}
			expires := entry.Expires
			if expires.After(ceiling) {
				expires = ceiling
			}
			blocks[entry.Key] = riskBlock{status: entry.Status, expires: expires}
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.blocks = blocks
	return nil
}

// isRiskStatus 报告状态码是否为会记录风控的 401 或 403。
func isRiskStatus(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusForbidden
}

// riskBlocked 在会话键命中风控记录时写出 500 与 riskBlockMessage（附记录的原始状态码）并返回 true；未命中或未启用时返回 false。
func (p *Pool) riskBlocked(ctx context.Context, w http.ResponseWriter, key string) bool {
	if p.risk == nil || key == "" {
		return false
	}
	status, ok := p.risk.lookup(key, time.Now())
	if !ok {
		return false
	}
	p.log(ctx, slog.LevelWarn, "风控：会话已被拦截", "session", key[:12], "status", status)
	writeError(w, http.StatusInternalServerError, fmt.Sprintf("%s（原始状态码 %d）", riskBlockMessage, status))
	return true
}

// recordRisk 在账号因 401/403 进入冷却后记录会话键与状态码，并立即写入记录文件（CacheDir 为空时只在内存）。
// cooled 为本次是否使账号进入冷却；未进入冷却、状态码不是 401/403、键为空或未启用时不做任何事。
func (p *Pool) recordRisk(ctx context.Context, key string, status int, cooled bool) {
	if !cooled || p.risk == nil || key == "" || !isRiskStatus(status) {
		return
	}
	now := time.Now()
	p.risk.add(key, status, now)
	p.log(ctx, slog.LevelWarn, "风控：记录会话", "session", key[:12], "status", status, "until", now.Add(riskBlockTTL).Format(time.RFC3339))
	if p.config.CacheDir == "" {
		return
	}
	if err := p.risk.save(filepath.Join(p.config.CacheDir, riskBlockFileName), now); err != nil {
		p.log(ctx, slog.LevelWarn, "风控：写入记录文件失败", "error", err)
	}
}

// loadRiskBlocks 在池创建时读取风控记录文件。加载失败只输出 Warn，记录表保持为空，池照常启动。
func (p *Pool) loadRiskBlocks() {
	if p.risk == nil || p.config.CacheDir == "" {
		return
	}
	if err := p.risk.load(filepath.Join(p.config.CacheDir, riskBlockFileName), time.Now()); err != nil {
		p.logger.Warn("风控：记录文件无法加载，已以空表启动", "error", err)
	}
}
