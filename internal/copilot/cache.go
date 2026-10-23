package copilot

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

const maxCacheFileSize = 8 << 20

func (s *Service) cacheFile(name string) string {
	if s.cacheDir == "" {
		return ""
	}
	return filepath.Join(s.cacheDir, name+".json")
}

// writeCache 把缓存写入账号的缓存目录。账号已删除时直接跳过，保证删除后不会重新创建缓存目录。
// 读锁与 removeCache 的写锁互斥：删除完成前的写入先完成，删除之后的写入都会被跳过。
func (s *Service) writeCache(name string, value any) error {
	s.cacheMu.RLock()
	defer s.cacheMu.RUnlock()
	path := s.cacheFile(name)
	if path == "" || s.cacheRemoved {
		return nil
	}
	return writeCacheFile(path, value)
}

// removeCache 标记账号已删除并删除其缓存目录。之后 writeCache 不再写入。
func (s *Service) removeCache() error {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	s.cacheRemoved = true
	if s.cacheDir == "" {
		return nil
	}
	return os.RemoveAll(s.cacheDir)
}

// removeCacheFile 删除账号缓存目录中的一个缓存文件；文件不存在视为成功。账号已删除或没有缓存目录时不做任何事。
// 与 writeCache 共用读锁，因此不会与 removeCache 交错。
func (s *Service) removeCacheFile(name string) error {
	s.cacheMu.RLock()
	defer s.cacheMu.RUnlock()
	path := s.cacheFile(name)
	if path == "" || s.cacheRemoved {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func readCacheFile(path string, value any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	data, err := readLimited(file, maxCacheFileSize)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}

func writeCacheFile(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(data) > maxCacheFileSize {
		return errors.New("本地缓存超过大小限制")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".cache-*")
	if err != nil {
		return err
	}
	tempPath := file.Name()
	defer os.Remove(tempPath)
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}
