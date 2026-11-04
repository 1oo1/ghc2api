package copilot

import (
	"os"
	"path/filepath"
	"testing"
)

// writeLogFile 写入测试用的日志文件内容（原样，不补换行）。
func writeLogFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// assertLogTail 调用 readLogTail 并断言返回的数据与 truncated 标记。
func assertLogTail(t *testing.T, path string, maxBytes int64, wantData string, wantTruncated bool) {
	t.Helper()
	data, truncated, err := readLogTail(path, maxBytes)
	if err != nil {
		t.Fatalf("readLogTail(maxBytes=%d) error = %v", maxBytes, err)
	}
	if string(data) != wantData || truncated != wantTruncated {
		t.Fatalf("readLogTail(maxBytes=%d) = (%q, %v), want (%q, %v)", maxBytes, data, truncated, wantData, wantTruncated)
	}
}

// TestReadLogTailSingleFile 只有当前日志文件时原样返回全部内容，未截断。
func TestReadLogTailSingleFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ghc2api.log")
	writeLogFile(t, path, "aaa\nbbb\nccc\n")
	assertLogTail(t, path, 1<<20, "aaa\nbbb\nccc\n", false)
}

// TestReadLogTailExactSizeIsNotTruncated maxBytes 恰好等于总长度时没有截掉任何内容，truncated=false。
func TestReadLogTailExactSizeIsNotTruncated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ghc2api.log")
	writeLogFile(t, path, "aaa\nbbb\n")
	assertLogTail(t, path, 8, "aaa\nbbb\n", false)
}

// TestReadLogTailConcatenatesBackupsOldestFirst 备份按数字降序（最旧在前）拼接，最后是当前文件；
// 数字按数值而非字典序比较（.10 比 .2 更旧）。
func TestReadLogTailConcatenatesBackupsOldestFirst(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ghc2api.log")
	writeLogFile(t, path+".10", "oldest\n")
	writeLogFile(t, path+".2", "older\n")
	writeLogFile(t, path+".1", "old\n")
	writeLogFile(t, path, "current\n")
	assertLogTail(t, path, 1<<20, "oldest\nolder\nold\ncurrent\n", false)
}

// TestReadLogTailIgnoresInvalidSuffixes 非法后缀（非数字、0、负数、多余后缀）、其他文件名前缀与目录都被忽略。
func TestReadLogTailIgnoresInvalidSuffixes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ghc2api.log")
	writeLogFile(t, path+".abc", "abc\n")
	writeLogFile(t, path+".0", "zero\n")
	writeLogFile(t, path+".-1", "negative\n")
	writeLogFile(t, path+".1.bak", "bak\n")
	writeLogFile(t, filepath.Join(dir, "other.log.1"), "other\n")
	if err := os.Mkdir(path+".5", 0o700); err != nil { // 同名模式的目录不是普通文件
		t.Fatal(err)
	}
	writeLogFile(t, path+".1", "backup\n")
	writeLogFile(t, path, "current\n")
	assertLogTail(t, path, 1<<20, "backup\ncurrent\n", false)
}

// TestReadLogTailSkipsMissingCurrentFile 当前文件不存在时跳过它，只返回备份。
func TestReadLogTailSkipsMissingCurrentFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ghc2api.log")
	writeLogFile(t, path+".1", "backup\n")
	assertLogTail(t, path, 1<<20, "backup\n", false)
}

// TestReadLogTailAllMissingReturnsEmpty 所有文件都不存在时返回空数据、truncated=false、无错误。
func TestReadLogTailAllMissingReturnsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ghc2api.log")
	assertLogTail(t, path, 1<<20, "", false)
}

// TestReadLogTailTruncationDropsLeadingPartialLine 截断点落在行中间时丢弃首部残行，truncated=true。
func TestReadLogTailTruncationDropsLeadingPartialLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ghc2api.log")
	writeLogFile(t, path, "aaa\nbbb\nccc\n") // 字节 6 是 "bbb" 中的第 3 个 b
	assertLogTail(t, path, 6, "ccc\n", true)
}

// TestReadLogTailTruncationAtLineStartKeepsLine 截断点恰好在行首（前一个字节是 \n）时不丢行，truncated 仍为 true。
func TestReadLogTailTruncationAtLineStartKeepsLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ghc2api.log")
	writeLogFile(t, path, "aaa\nbbb\nccc\n")
	assertLogTail(t, path, 8, "bbb\nccc\n", true)
}

// TestReadLogTailTruncationStartingAtNewlineByte 保留部分的第一个字节本身就是 \n（它前一个字节不是 \n）时，
// 该 \n 是上一行的结尾，被当作残行丢弃。
func TestReadLogTailTruncationStartingAtNewlineByte(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ghc2api.log")
	writeLogFile(t, path, "aaa\nbbb\nccc\n")
	assertLogTail(t, path, 5, "ccc\n", true) // 保留 "\nccc\n"，第一个字节是 bbb 行的换行
}

// TestReadLogTailTruncationLeavesNothing 保留部分内只有一个残行且没有换行可丢到时，结果为空，truncated=true。
func TestReadLogTailTruncationLeavesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ghc2api.log")
	writeLogFile(t, path, "aaaaaaaaaa\n")
	assertLogTail(t, path, 4, "", true)
}

// TestReadLogTailDropsTrailingPartialLine 末尾最后一个 \n 之后（正在写入的半行）被丢弃；未截掉内容时 truncated=false。
func TestReadLogTailDropsTrailingPartialLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ghc2api.log")
	writeLogFile(t, path, "aaa\nbbb\nccc")
	assertLogTail(t, path, 1<<20, "aaa\nbbb\n", false)
}

// TestReadLogTailOnlyPartialLineReturnsEmpty 整个文件只有半行时结果为空。
func TestReadLogTailOnlyPartialLineReturnsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ghc2api.log")
	writeLogFile(t, path, "no-newline-yet")
	assertLogTail(t, path, 1<<20, "", false)
}

// TestReadLogTailPadsBackupWithoutTrailingNewline 备份文件末尾缺 \n 时拼接补一个换行，避免与下一文件首行粘连。
func TestReadLogTailPadsBackupWithoutTrailingNewline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ghc2api.log")
	writeLogFile(t, path+".1", "x1\nx2")
	writeLogFile(t, path, "y1\n")
	assertLogTail(t, path, 1<<20, "x1\nx2\ny1\n", false)
}

// TestReadLogTailTruncationSpansFiles 截断跨越多个文件：从最新文件向前读够 maxBytes 为止。
// 三个文件各 6 字节：.2 = "a1\na2\n"，.1 = "b1\nb2\n"，当前 = "c1\nc2\n"，拼接后共 18 字节。
func TestReadLogTailTruncationSpansFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ghc2api.log")
	writeLogFile(t, path+".2", "a1\na2\n")
	writeLogFile(t, path+".1", "b1\nb2\n")
	writeLogFile(t, path, "c1\nc2\n")

	for _, tc := range []struct {
		maxBytes int64
		want     string
		trunc    bool
	}{
		{18, "a1\na2\nb1\nb2\nc1\nc2\n", false},
		{15, "a2\nb1\nb2\nc1\nc2\n", true}, // 起点在 a2 行首
		{14, "b1\nb2\nc1\nc2\n", true},     // 起点在 a2 行中间：丢弃残行
		{12, "b1\nb2\nc1\nc2\n", true},     // 起点恰好在 b1 行首（文件边界）
		{9, "b2\nc1\nc2\n", true},          // 起点在 b2 行首，跨过文件边界
		{8, "c1\nc2\n", true},              // 起点在 b2 行中间
	} {
		assertLogTail(t, path, tc.maxBytes, tc.want, tc.trunc)
	}
}

// TestReadLogTailStopsReadingOnceEnoughBytes 读够 maxBytes 后不再读取更旧的文件：
// 更旧的备份即使不可读也不会导致错误（证明没有把所有文件整体读入）。
func TestReadLogTailStopsReadingOnceEnoughBytes(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 不受文件权限限制")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "ghc2api.log")
	writeLogFile(t, path+".1", "old\n")
	if err := os.Chmod(path+".1", 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path+".1", 0o600) })
	writeLogFile(t, path, "c1\nc2\nc3\n")
	assertLogTail(t, path, 6, "c2\nc3\n", true)
}

// TestReadLogTailReturnsReadError 除“文件不存在”外的读取错误（如权限不足）返回 err。
func TestReadLogTailReturnsReadError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 不受文件权限限制")
	}
	path := filepath.Join(t.TempDir(), "ghc2api.log")
	writeLogFile(t, path, "aaa\n")
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	if _, _, err := readLogTail(path, 1<<20); err == nil {
		t.Fatal("文件不可读时 readLogTail 应返回错误")
	}
}
