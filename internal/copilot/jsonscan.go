package copilot

// 本文件是请求体的 JSON 辅助函数。请求体可能含有数百万个数组元素（例如 messages 中的数字），
// 因此这里逐个定位数组元素，不把整个数组解析成切片，也不为每个非对象元素分配内存。
// 调用方须保证输入已通过 json.Valid（请求体在解析前整体校验），这里不做语法检查。
// 但对非法输入，所有函数都保证在有限时间内返回且不 panic（结果不作保证），测试见 jsonscan_test.go。
//
// 元素边界用字节扫描而不用 json.Decoder.Decode 逐个读取：Decode 每个元素至少分配约 32 字节（-race 下约 120 字节），
// 800 万个元素的数组即需约 256 MB，远超内存预算。
// json.Decoder.Token 对每个数字也会分配字符串，同样不可取。

import (
	"encoding/json"
	"unicode/utf8"
)

// arrayBody 返回 JSON 数组 raw 中 '[' 之后的下标；raw 不是数组时返回 -1。
func arrayBody(raw []byte) int {
	i := skipJSONSpace(raw, 0)
	if i >= len(raw) || raw[i] != '[' {
		return -1
	}
	return i + 1
}

// eachObject 按顺序把 JSON 数组 raw 中的对象元素交给 fn，fn 返回 false 时停止遍历。
// 参数是 raw 中该对象的子切片，fn 不得保留它。数字、字符串、null、数组等非对象元素只按字节跳过，不做解析。
// raw 不是数组时什么也不做。输入非法时（如 [}]、[1,}）遍历在无法前进处停止，不会死循环，也不会越界。
func eachObject(raw json.RawMessage, fn func(object []byte) bool) {
	i := arrayBody(raw)
	if i < 0 {
		return
	}
	for {
		i = skipJSONSpace(raw, i)
		if i >= len(raw) || raw[i] == ']' {
			return
		}
		end := valueEnd(raw, i)
		if end <= i {
			return // 值的位置没有前进，只可能是非法输入，停止遍历以免死循环
		}
		if raw[i] == '{' && !fn(raw[i:end]) {
			return
		}
		i = skipJSONSpace(raw, end)
		if i < len(raw) && raw[i] == ',' {
			i++
		}
	}
}

// valueEnd 返回从下标 i 开始的一个 JSON 值之后的下标（不含之后的逗号或外层结束括号）。
// 容器按括号深度匹配，字符串按转义跳过；标量在遇到深度为 0 的逗号或外层结束括号时结束。
// 返回值不超过 len(s)；输入被截断时，未闭合的值结束于 len(s)。
func valueEnd(s []byte, i int) int {
	depth := 0
	for ; i < len(s); i++ {
		switch s[i] {
		case '"':
			i = jsonStringEnd(s, i)
		case '{', '[':
			depth++
		case '}', ']':
			if depth == 0 {
				return i // 外层数组的结束括号，前面是标量值
			}
			depth--
			if depth == 0 {
				return i + 1 // 容器值的结束
			}
		case ',':
			if depth == 0 {
				return i
			}
		}
	}
	return i
}

// skipJSONSpace 返回从下标 i 起跳过 JSON 空白（空格、制表符、回车、换行）后的下标，结果可能等于 len(s)。
func skipJSONSpace(s []byte, i int) int {
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\r' || s[i] == '\n') {
		i++
	}
	return i
}

// firstByte 返回 raw 去除首部 JSON 空白后的第一个字节；全为空白或为空时返回 0。
func firstByte(raw []byte) byte {
	if i := skipJSONSpace(raw, 0); i < len(raw) {
		return raw[i]
	}
	return 0
}

// jsonStringEnd 返回以 start 处引号开始的字符串字面量的结束引号下标。start 处须为引号。
// 字符串未闭合（输入被截断或非法）时返回 len(body)-1，即最后一个字节的下标，调用方据此切片时不会越界。
func jsonStringEnd(body []byte, start int) int {
	for i := start + 1; i < len(body); i++ {
		switch body[i] {
		case '\\':
			i++ // 跳过被转义的字符；\u 之后的四位十六进制数字都是普通字符，不会是引号或反斜杠
		case '"':
			return i
		}
	}
	return len(body) - 1
}

// decodeASCIIString 把 JSON 字符串字面量（含引号）解码到 buf 并返回结果，用于与 ASCII 目标比较，不分配内存。
// 以下情况返回 nil，效果等同于"不等于任何 ASCII 目标"：含非 ASCII 字符（包括解码为非 ASCII 的 \u 转义）；
// 使用了 \u 以外的转义（它们解码为控制字符、引号、反斜杠或斜杠，不可能出现在目标中）；结果超出 buf；
// 字面量不完整（少于两个字节、末尾的反斜杠、\u 后不足 4 个字符）。
// 调用方的目标（type、image 等）只含小写字母与下划线。输入不完整或非法时结果无意义，但不会越界。
func decodeASCIIString(literal, buf []byte) []byte {
	if len(literal) < 2 {
		return nil
	}
	s := literal[1 : len(literal)-1]
	n := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\\' {
			if i+6 > len(s) || s[i+1] != 'u' {
				return nil
			}
			r := hexValue(s[i+2 : i+6])
			if r >= utf8.RuneSelf {
				return nil
			}
			c = byte(r)
			i += 5
		}
		if c >= utf8.RuneSelf || n == len(buf) {
			return nil
		}
		buf[n] = c
		n++
	}
	return buf[:n]
}

// hexValue 把 4 位十六进制数字转为码点。调用方须保证长度为 4（decodeASCIIString 已检查）；
// 非十六进制字符只出现在非法输入中，此时结果无意义，但不会 panic。
func hexValue(digits []byte) rune {
	var r rune
	for _, c := range digits {
		switch {
		case c >= '0' && c <= '9':
			r = r<<4 | rune(c-'0')
		case c >= 'a' && c <= 'f':
			r = r<<4 | rune(c-'a'+10)
		default:
			r = r<<4 | rune(c-'A'+10)
		}
	}
	return r
}
