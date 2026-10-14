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
