package main

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// rotatingLogWriter 按大小滚动日志文件，并保留配置数量的备份。
type rotatingLogWriter struct {
	mu         sync.Mutex
	path       string
	maxSize    int64
	maxBackups int
	file       *os.File
	size       int64
	closed     bool
	failure    error
}

func newConfiguredLogger(config appConfig, stderr io.Writer) (*slog.Logger, *rotatingLogWriter, error) {
	logFile, err := newRotatingLogWriter(
		filepath.Join(config.LogsPath, "ghc2api.log"),
		config.LogMaxSizeMB*(1<<20),
		config.LogMaxBackups,
	)
	if err != nil {
		return nil, nil, err
	}

	logger, err := newLogger(config.LogLevel, logOutputWriter{file: logFile, stderr: stderr})
	if err != nil {
		_ = logFile.Close()
		return nil, nil, err
	}
	return logger, logFile, nil
}

type logOutputWriter struct {
	file   io.Writer
	stderr io.Writer
}

// Write 分别写入日志文件和标准错误，避免一个目标失败时丢失另一个目标的输出。
func (w logOutputWriter) Write(data []byte) (int, error) {
	written := false
	var writeErrors []error
	for _, output := range []io.Writer{w.file, w.stderr} {
		if output == nil {
			continue
		}
		n, err := output.Write(data)
		if n == len(data) {
			written = true
		}
		if err == nil && n != len(data) {
			err = io.ErrShortWrite
		}
		if err != nil {
			writeErrors = append(writeErrors, err)
		}
	}
	if len(writeErrors) > 0 {
		if written {
			return len(data), errors.Join(writeErrors...)
		}
		return 0, errors.Join(writeErrors...)
	}
	if w.file == nil && w.stderr == nil {
		return 0, errors.New("没有可用的日志输出")
	}
	return len(data), nil
}

func newRotatingLogWriter(path string, maxSize int64, maxBackups int) (*rotatingLogWriter, error) {
	if path == "" {
		return nil, errors.New("日志文件路径不能为空")
	}
	if maxSize <= 0 {
		return nil, errors.New("日志单文件大小必须大于 0")
	}
	if maxBackups < 0 || maxBackups > maxLogBackups {
		return nil, fmt.Errorf("日志备份数必须是 0 到 %d 之间的整数", maxLogBackups)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("无法创建日志目录 %q: %w", dir, err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("无法检查日志目录 %q: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("日志路径 %q 不是目录", dir)
	}

	writer := &rotatingLogWriter{
		path: path, maxSize: maxSize, maxBackups: maxBackups,
	}
	if err := writer.pruneBackups(); err != nil {
		return nil, err
	}
	if err := writer.openCurrent(); err != nil {
		return nil, err
	}
	if writer.size > writer.maxSize {
		if err := writer.rotate(); err != nil {
			_ = writer.Close()
			return nil, fmt.Errorf("无法滚动超出上限的日志文件 %q: %w", path, err)
		}
	}
	return writer, nil
}

func (w *rotatingLogWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, os.ErrClosed
	}
	if w.failure != nil {
		return 0, w.failure
	}
	if w.file == nil {
		if err := w.openCurrent(); err != nil {
			return 0, err
		}
	}
	if int64(len(data)) > w.maxSize {
		return 0, fmt.Errorf("单条日志记录大小 %d 超过单文件上限 %d", len(data), w.maxSize)
	}
	if w.size > 0 && w.size+int64(len(data)) > w.maxSize {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	startSize := w.size
	n, err := w.file.Write(data)
	if err == nil && n == len(data) {
		w.size += int64(n)
		return n, nil
	}
	if err == nil {
		err = io.ErrShortWrite
	}
	rollbackErr := w.file.Truncate(startSize)
	if rollbackErr == nil {
		w.size = startSize
	} else {
		w.failure = errors.Join(err, rollbackErr)
	}
	if w.file != nil {
		closeErr := w.file.Close()
		w.file = nil
		return n, errors.Join(err, rollbackErr, closeErr)
	}
	return n, errors.Join(err, rollbackErr)
}

func (w *rotatingLogWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	if w.file == nil {
		return w.failure
	}
	syncErr := w.file.Sync()
	closeErr := w.file.Close()
	w.file = nil
	return errors.Join(w.failure, syncErr, closeErr)
}

func (w *rotatingLogWriter) openCurrent() error {
	if w.closed || w.failure != nil {
		return os.ErrClosed
	}
	file, err := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("无法打开日志文件 %q: %w", w.path, err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return fmt.Errorf("无法限制日志文件 %q 的权限: %w", w.path, err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return fmt.Errorf("无法检查日志文件 %q: %w", w.path, err)
	}
	size, err := truncateIncompleteLogTail(file, info.Size())
	if err != nil {
		_ = file.Close()
		return fmt.Errorf("无法修复日志文件 %q 的截断尾记录: %w", w.path, err)
	}
	w.file = file
	w.size = size
	return nil
}

func (w *rotatingLogWriter) rotate() error {
	if w.file != nil {
		err := w.file.Close()
		w.file = nil
		if err != nil {
			return w.reopenAfterFailure(err)
		}
	}

	if w.maxBackups == 0 {
		if err := removeIfExists(w.path); err != nil {
			return w.reopenAfterFailure(err)
		}
	} else {
		if err := removeIfExists(w.backupPath(w.maxBackups)); err != nil {
			return w.reopenAfterFailure(err)
		}
		for index := w.maxBackups - 1; index >= 1; index-- {
			if err := renameIfExists(w.backupPath(index), w.backupPath(index+1)); err != nil {
				return w.reopenAfterFailure(err)
			}
		}
		if err := renameIfExists(w.path, w.backupPath(1)); err != nil {
			return w.reopenAfterFailure(err)
		}
	}
	return w.openCurrent()
}

func (w *rotatingLogWriter) backupPath(index int) string {
	return fmt.Sprintf("%s.%d", w.path, index)
}

func (w *rotatingLogWriter) pruneBackups() error {
	dir := filepath.Dir(w.path)
	prefix := filepath.Base(w.path) + "."
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("无法读取日志目录 %q: %w", dir, err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		index, err := strconv.Atoi(strings.TrimPrefix(name, prefix))
		if err != nil || index < 1 || index <= w.maxBackups {
			continue
		}
		path := filepath.Join(dir, name)
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("无法清理超出保留数量的日志备份 %q: %w", path, err)
		}
	}
	return nil
}

func truncateIncompleteLogTail(file *os.File, size int64) (int64, error) {
	if size == 0 {
		return 0, nil
	}
	var lastByte [1]byte
	if _, err := file.ReadAt(lastByte[:], size-1); err != nil {
		return size, err
	}
	if lastByte[0] == '\n' {
		return size, nil
	}

	const scanSize = 4096
	buffer := make([]byte, scanSize)
	for end := size; end > 0; {
		start := max(int64(0), end-scanSize)
		count := int(end - start)
		n, err := file.ReadAt(buffer[:count], start)
		if err != nil && !errors.Is(err, io.EOF) {
			return size, err
		}
		for index := n - 1; index >= 0; index-- {
			if buffer[index] == '\n' {
				validSize := start + int64(index) + 1
				if err := file.Truncate(validSize); err != nil {
					return size, err
				}
				return validSize, nil
			}
		}
		end = start
	}
	if err := file.Truncate(0); err != nil {
		return size, err
	}
	return 0, nil
}

func (w *rotatingLogWriter) reopenAfterFailure(cause error) error {
	if err := w.openCurrent(); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

func removeIfExists(path string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func renameIfExists(oldPath, newPath string) error {
	err := os.Rename(oldPath, newPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
