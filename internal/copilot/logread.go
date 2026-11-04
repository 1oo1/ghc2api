package copilot

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// readLogTail 返回日志文件及其滚动备份按时间顺序拼接（path.N … path.1, path）后的最后至多 maxBytes 字节，只含完整的行。
//
// 从最新文件向前读取，读够 maxBytes 即停止，不会把所有文件整体读入内存。
//   - 因 maxBytes 截掉了更早的内容时 truncated 为 true；保留部分的第一个字节不是行首时，丢弃到第一个 \n（含）为止的残行。
//   - 末尾最后一个 \n 之后（正在写入的半行）丢弃。
//   - 备份文件末尾缺少 \n 时，拼接处补一个 \n，避免与下一文件的首行粘连。
//   - 文件不存在则跳过；其他读取错误返回 err。
//
// maxBytes 必须大于 0，由调用方校验。
func readLogTail(path string, maxBytes int64) (data []byte, truncated bool, err error) {
	files, err := logFilesOldestFirst(path)
	if err != nil {
		return nil, false, err
	}

	var parts [][]byte // 按“最新在前”收集，最后反转拼接
	total := 0
	remaining := maxBytes
	partial := false // 保留部分的第一个字节前面还有同一行的内容
	for i := len(files) - 1; i >= 0; i-- {
		if remaining == 0 { // 预算用尽：只需知道更早的文件是否还有内容
			size, ok, err := statLogFile(files[i])
			if err != nil {
				return nil, false, err
			}
			if ok && size > 0 {
				truncated = true
				break
			}
			continue
		}
		part, cut, startsMidLine, err := readLogPart(files[i], remaining, len(parts) > 0)
		if err != nil {
			return nil, false, err
		}
		if len(part) == 0 { // 文件不存在、为空或已被截短
			continue
		}
		parts = append(parts, part)
		total += len(part)
		remaining -= int64(len(part))
		if cut {
			truncated, partial = true, startsMidLine
			break
		}
	}

	switch len(parts) {
	case 0:
	case 1:
		data = parts[0] // 单个文件直接使用，避免整体拷贝
	default:
		data = make([]byte, 0, total)
		for i := len(parts) - 1; i >= 0; i-- {
			data = append(data, parts[i]...)
		}
	}
	if partial {
		if idx := bytes.IndexByte(data, '\n'); idx >= 0 {
			data = data[idx+1:]
		} else {
			data = nil
		}
	}
	if idx := bytes.LastIndexByte(data, '\n'); idx >= 0 {
		data = data[:idx+1]
	} else {
		data = nil
	}
	return data, truncated, nil
}

// readLogPart 读取文件的最后至多 limit 字节。pad 为 true 时（后面还有更新的内容）文件末尾缺 \n 会补一个，补的字节计入 limit。
// cut 表示文件开头的内容被截掉；startsMidLine 表示截断点落在行中间（返回的第一个字节前一个字节不是 \n）。
// 先打开文件再用文件句柄取大小，因此与日志滚动（重命名）并发时读到的仍是同一个文件；
// 文件不存在时返回空 part；读取期间文件被截短时按实际读到的字节处理。
func readLogPart(path string, limit int64, pad bool) (part []byte, cut, startsMidLine bool, err error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, false, nil
	}
	if err != nil {
		return nil, false, false, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, false, false, err
	}
	size := info.Size()
	if !info.Mode().IsRegular() || size == 0 {
		return nil, false, false, nil
	}

	needNewline := false
	if pad {
		var last [1]byte
		n, err := file.ReadAt(last[:], size-1)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, false, false, err
		}
		needNewline = n == 1 && last[0] != '\n'
	}
	virtualSize := size
	if needNewline {
		virtualSize++
	}

	// 被截掉时多读一个字节（位于截断点之前）用来判断截断点是否在行首。
	want := virtualSize
	cut = virtualSize > limit
	if cut {
		want = limit + 1
	}
	fileWant := want
	if needNewline {
		fileWant--
	}
	buf := make([]byte, fileWant, want)
	n, err := file.ReadAt(buf, size-fileWant)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, false, false, err
	}
	buf = buf[:n]
	if needNewline && int64(n) == fileWant {
		buf = append(buf, '\n')
	}
	if cut && len(buf) > 0 {
		startsMidLine = buf[0] != '\n'
		buf = buf[1:]
	}
	return buf, cut, startsMidLine, nil
}

// statLogFile 返回普通文件的大小；文件不存在或不是普通文件时 ok 为 false。
func statLogFile(path string) (size int64, ok bool, err error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return info.Size(), info.Mode().IsRegular(), nil
}

// logFilesOldestFirst 返回 path 的滚动备份（path.N … path.1，N 越大越旧）及 path 本身，最旧在前。
// 备份必须是普通文件，后缀能被 strconv.Atoi 解析且不小于 1；目录不存在时只返回 path。
func logFilesOldestFirst(path string) ([]string, error) {
	dir, base := filepath.Dir(path), filepath.Base(path)+"."
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	type backup struct {
		index int
		path  string
	}
	var backups []backup
	for _, entry := range entries {
		suffix, found := strings.CutPrefix(entry.Name(), base)
		if !found || !entry.Type().IsRegular() {
			continue
		}
		if index, err := strconv.Atoi(suffix); err == nil && index >= 1 {
			backups = append(backups, backup{index, filepath.Join(dir, entry.Name())})
		}
	}
	sort.Slice(backups, func(i, j int) bool { return backups[i].index > backups[j].index })

	files := make([]string, 0, len(backups)+1)
	for _, b := range backups {
		files = append(files, b.path)
	}
	return append(files, path), nil
}
