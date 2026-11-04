package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// runIDPattern 是 run_id 的格式：8 字节随机数的 16 位小写十六进制。
var runIDPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)

// TestNewRunIDFormatAndUniqueness newRunID 返回 16 位小写十六进制字符串，多次调用得到不同的值。
func TestNewRunIDFormatAndUniqueness(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		id, err := newRunID()
		if err != nil {
			t.Fatalf("newRunID() error = %v", err)
		}
		if !runIDPattern.MatchString(id) {
			t.Fatalf("newRunID() = %q, want 16 位小写十六进制", id)
		}
		if seen[id] {
			t.Fatalf("newRunID() 重复返回 %q", id)
		}
		seen[id] = true
	}
}

// readLogRecords 读取日志文件并把每一行解析为 JSON 对象。
func readLogRecords(t *testing.T, path string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var records []map[string]any
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	for scanner.Scan() {
		var record map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatalf("日志行不是 JSON: %v; line=%s", err, scanner.Text())
		}
		records = append(records, record)
	}
	return records
}

// TestNewConfiguredLoggerStampsRunID newConfiguredLogger 返回的根日志器写入日志文件的每条记录都带 run_id
// （16 位小写十六进制）；同一 logger 及其 With 派生日志器的记录值相同，无论级别。
func TestNewConfiguredLoggerStampsRunID(t *testing.T) {
	logsDir := t.TempDir()
	logger, file, err := newConfiguredLogger(appConfig{
		LogsPath: logsDir, LogLevel: "debug", LogMaxSizeMB: 1, LogMaxBackups: 1, LogFileOnly: true,
	}, &bytes.Buffer{}, new(slog.LevelVar))
	if err != nil {
		t.Fatalf("newConfiguredLogger() error = %v", err)
	}
	logger.Debug("d")
	logger.Info("i")
	logger.Warn("w")
	logger.Error("e")
	logger.With("request_id", 7).Info("derived")
	logger.With("a", 1).With("b", 2).Info("derived twice")
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	records := readLogRecords(t, filepath.Join(logsDir, logFileName))
	if len(records) != 6 {
		t.Fatalf("日志记录数 = %d, want 6", len(records))
	}
	first, _ := records[0]["run_id"].(string)
	if !runIDPattern.MatchString(first) {
		t.Fatalf("第一条记录的 run_id = %#v, want 16 位小写十六进制", records[0]["run_id"])
	}
	for i, record := range records {
		if got, _ := record["run_id"].(string); got != first {
			t.Fatalf("第 %d 条记录的 run_id = %#v, want %q（同一进程内相同）", i, record["run_id"], first)
		}
	}
}

// TestNewConfiguredLoggerRunIDDiffersPerCall 两次调用 newConfiguredLogger 得到不同的 run_id（模拟两次进程启动）。
func TestNewConfiguredLoggerRunIDDiffersPerCall(t *testing.T) {
	ids := make([]string, 0, 2)
	for i := 0; i < 2; i++ {
		logsDir := t.TempDir()
		logger, file, err := newConfiguredLogger(appConfig{
			LogsPath: logsDir, LogLevel: "info", LogMaxSizeMB: 1, LogMaxBackups: 1, LogFileOnly: true,
		}, &bytes.Buffer{}, new(slog.LevelVar))
		if err != nil {
			t.Fatalf("newConfiguredLogger() error = %v", err)
		}
		logger.Info("probe")
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		records := readLogRecords(t, filepath.Join(logsDir, logFileName))
		if len(records) != 1 {
			t.Fatalf("日志记录数 = %d, want 1", len(records))
		}
		id, _ := records[0]["run_id"].(string)
		if !runIDPattern.MatchString(id) {
			t.Fatalf("run_id = %#v", records[0]["run_id"])
		}
		ids = append(ids, id)
	}
	if ids[0] == ids[1] {
		t.Fatalf("两次调用的 run_id 相同: %q", ids[0])
	}
}
