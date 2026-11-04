// JSON 高亮与正文格式化（highlight.ts）的测试，覆盖的导出：
//   - tokenizeJson(text)：把 JSON 文本切分为高亮记号，所有记号拼接必须还原输入，不得抛异常。
//   - formatBody(text, maxTokens = Infinity)：JSON 美化、SSE 逐行处理、纯文本；返回 { format, tokens, highlighted }。
//     格式化没有字符数上限；记号数超过 maxTokens 时 highlighted 为 false，tokens 为单个 text 记号（格式化后的完整文本）。
// 规格没有规定相邻的空白记号是否合并，因此断言前先用 merged() 把相邻的 "text" 记号合并。
import { expect, test } from "vitest";
import { formatBody, tokenizeJson, type CodeToken } from "./highlight";

const joined = (tokens: CodeToken[]) => tokens.map((t) => t.text).join("");

/** 合并相邻的 "text" 记号，使断言不依赖空白被切成几个记号。 */
function merged(tokens: CodeToken[]): CodeToken[] {
  const result: CodeToken[] = [];
  for (const token of tokens) {
    const last = result[result.length - 1];
    if (last && last.type === "text" && token.type === "text") {
      result[result.length - 1] = { type: "text", text: last.text + token.text };
    } else {
      result.push({ ...token });
    }
  }
  return result;
}

const tok = (type: CodeToken["type"], text: string): CodeToken => ({ type, text });

// ---------------------------------------------------------------- tokenizeJson

test("tokenizeJson 空串没有记号", () => {
  expect(tokenizeJson("")).toEqual([]);
});

test("tokenizeJson 的记号拼接后等于输入", () => {
  const inputs = [
    '{"a":1,"b":[true,false,null],"c":{"d":"e"}}',
    JSON.stringify({ k: "v\n\"q\"", n: -1.5e-3, arr: [1, 2, { x: null }] }, null, 2),
    '{\n  "a" : 1 ,\n\t"b":\r\n 2\n}',
    "  [ ]  ",
    '"\\u00e9 中文 \\\\"',
    "not json at all",
    '{"unterminated": "abc',
    "}{][,:",
    "tru nul fals @#$ 12abc",
  ];
  for (const input of inputs) {
    expect(joined(tokenizeJson(input)), input).toBe(input);
  }
});

test("tokenizeJson 区分 key 与 string，标点与空白", () => {
  expect(merged(tokenizeJson('{"a": "b"}'))).toEqual([
    tok("punct", "{"),
    tok("key", '"a"'),
    tok("punct", ":"),
    tok("text", " "),
    tok("string", '"b"'),
    tok("punct", "}"),
  ]);
});

test("tokenizeJson 允许 key 与冒号之间有空白", () => {
  expect(merged(tokenizeJson('{"a" : 1}'))).toEqual([
    tok("punct", "{"),
    tok("key", '"a"'),
    tok("text", " "),
    tok("punct", ":"),
    tok("text", " "),
    tok("number", "1"),
    tok("punct", "}"),
  ]);
});

test("tokenizeJson 数组中的字符串是 string，逗号与方括号是 punct", () => {
  expect(merged(tokenizeJson('["a","b"]'))).toEqual([
    tok("punct", "["),
    tok("string", '"a"'),
    tok("punct", ","),
    tok("string", '"b"'),
    tok("punct", "]"),
  ]);
});

test("tokenizeJson 转义字符串是一个记号", () => {
  expect(tokenizeJson('"a\\"b"')).toEqual([tok("string", '"a\\"b"')]);
  expect(tokenizeJson('"a\\\\"')).toEqual([tok("string", '"a\\\\"')]); // 以转义反斜杠结尾
  expect(tokenizeJson('"line\\nbreak \\u00e9"')).toEqual([tok("string", '"line\\nbreak \\u00e9"')]);
});

test("tokenizeJson 含转义的 key 仍是 key", () => {
  expect(merged(tokenizeJson('{"a\\"b":1}'))).toEqual([
    tok("punct", "{"),
    tok("key", '"a\\"b"'),
    tok("punct", ":"),
    tok("number", "1"),
    tok("punct", "}"),
  ]);
});

test("tokenizeJson 字符串值内的冒号和花括号不会被拆成记号", () => {
  expect(merged(tokenizeJson('{"u":"http://x/{y}"}'))).toEqual([
    tok("punct", "{"),
    tok("key", '"u"'),
    tok("punct", ":"),
    tok("string", '"http://x/{y}"'),
    tok("punct", "}"),
  ]);
});

test("tokenizeJson 识别整数、负数、小数与指数", () => {
  for (const n of ["0", "7", "-1", "3.14", "-0.5", "1e10", "2E5", "-2.5E+3", "1e-7"]) {
    expect(tokenizeJson(n), n).toEqual([tok("number", n)]);
  }
  expect(merged(tokenizeJson("[1,-2,3.5]"))).toEqual([
    tok("punct", "["),
    tok("number", "1"),
    tok("punct", ","),
    tok("number", "-2"),
    tok("punct", ","),
    tok("number", "3.5"),
    tok("punct", "]"),
  ]);
});

test("tokenizeJson 识别 true、false、null", () => {
  expect(tokenizeJson("true")).toEqual([tok("boolean", "true")]);
  expect(tokenizeJson("false")).toEqual([tok("boolean", "false")]);
  expect(tokenizeJson("null")).toEqual([tok("null", "null")]);
  expect(merged(tokenizeJson("[true,false,null]"))).toEqual([
    tok("punct", "["),
    tok("boolean", "true"),
    tok("punct", ","),
    tok("boolean", "false"),
    tok("punct", ","),
    tok("null", "null"),
    tok("punct", "]"),
  ]);
});

test("tokenizeJson 空白与换行为 text 记号", () => {
  const tokens = tokenizeJson('{\n  "a": 1\n}');
  expect(merged(tokens)).toEqual([
    tok("punct", "{"),
    tok("text", "\n  "),
    tok("key", '"a"'),
    tok("punct", ":"),
    tok("text", " "),
    tok("number", "1"),
    tok("text", "\n"),
    tok("punct", "}"),
  ]);
});

test("tokenizeJson 无法识别的字符为 text", () => {
  const tokens = merged(tokenizeJson("@#"));
  expect(tokens).toEqual([tok("text", "@#")]);
});

test("tokenizeJson 遇到非法输入不抛异常", () => {
  for (const input of ['"abc', '{"a":', "[1,", "\\", '"\\', "-", "1e", '{"a" "b"}', "\u0000\u0001", "😀 emoji"]) {
    expect(() => tokenizeJson(input), input).not.toThrow();
    expect(joined(tokenizeJson(input)), input).toBe(input);
  }
});

// ---------------------------------------------------------------- formatBody: json

test("formatBody 对 JSON 对象美化为 2 空格缩进", () => {
  const raw = '{"a":1,"b":[1,2],"c":{"d":"e"}}';
  const result = formatBody(raw);
  expect(result.format).toBe("json");
  const pretty = JSON.stringify(JSON.parse(raw), null, 2);
  expect(joined(result.tokens)).toBe(pretty);
  expect(merged(result.tokens)).toEqual(merged(tokenizeJson(pretty)));
});

test("formatBody 对已缩进或带多余空白的 JSON 重新美化", () => {
  const result = formatBody('  { "a" :   1 }\n');
  expect(result.format).toBe("json");
  expect(joined(result.tokens)).toBe('{\n  "a": 1\n}');
});

test("formatBody 的 JSON 标量也是 json 格式", () => {
  for (const [raw, pretty] of [
    ["123", "123"],
    ['"str"', '"str"'],
    ["null", "null"],
    ["true", "true"],
    ["[]", "[]"],
  ] as const) {
    const result = formatBody(raw);
    expect(result.format, raw).toBe("json");
    expect(joined(result.tokens), raw).toBe(pretty);
  }
});

// ---------------------------------------------------------------- formatBody: sse

test("formatBody 对 SSE 逐行处理：data 行的 JSON 紧凑输出并高亮，其余行原样为 text", () => {
  const raw = 'event: message\ndata: {"a":1}\n\ndata: [DONE]\n';
  const result = formatBody(raw);
  expect(result.format).toBe("sse");
  expect(joined(result.tokens)).toBe(raw);
  expect(merged(result.tokens)).toEqual([
    tok("text", "event: message\ndata: "),
    tok("punct", "{"),
    tok("key", '"a"'),
    tok("punct", ":"),
    tok("number", "1"),
    tok("punct", "}"),
    tok("text", "\n\ndata: [DONE]\n"),
  ]);
});

test("formatBody 的 SSE data 行 JSON 重写为紧凑格式，且总是以 \"data: \" 开头", () => {
  const result = formatBody('data: {"a": 1,  "b": [1, 2]}\ndata:{"c":true}');
  expect(result.format).toBe("sse");
  expect(joined(result.tokens)).toBe('data: {"a":1,"b":[1,2]}\ndata: {"c":true}');
  // 紧凑 JSON 部分按 tokenizeJson(JSON.stringify(value)) 高亮
  const types = merged(result.tokens).map((t) => t.type);
  expect(types).toContain("key");
  expect(types).toContain("boolean");
});

test("formatBody 的 SSE data 行内容是 JSON 标量时也高亮", () => {
  const result = formatBody("data: 123\ndata: null");
  expect(result.format).toBe("sse");
  expect(merged(result.tokens)).toEqual([
    tok("text", "data: "),
    tok("number", "123"),
    tok("text", "\ndata: "),
    tok("null", "null"),
  ]);
});

test("formatBody 的 SSE 非 JSON 的 data 行（如 [DONE]）与 event 行整行为 text", () => {
  const result = formatBody("event: ping\ndata: [DONE]\n: comment\n");
  expect(result.format).toBe("sse");
  expect(result.tokens.every((t) => t.type === "text")).toBe(true);
  expect(joined(result.tokens)).toBe("event: ping\ndata: [DONE]\n: comment\n");
});

test("formatBody 的 SSE 保留换行与空行，空行不产生记号", () => {
  const raw = "data: [DONE]\n\n\ndata: [DONE]";
  const result = formatBody(raw);
  expect(result.format).toBe("sse");
  expect(joined(result.tokens)).toBe(raw);
  expect(result.tokens.every((t) => t.text !== "")).toBe(true);
});

test("formatBody 仅在存在以 data: 开头的行时才判为 sse", () => {
  expect(formatBody("event: x\nid: 1").format).toBe("text");
  expect(formatBody("my data: x").format).toBe("text"); // data: 不在行首
  expect(formatBody(" data: x").format).toBe("text"); // 行首有空格
});

// ---------------------------------------------------------------- formatBody: text

test("formatBody 对纯文本返回单个 text 记号并保留换行", () => {
  const raw = "hello\nworld  ";
  expect(formatBody(raw)).toEqual({ format: "text", tokens: [tok("text", raw)], highlighted: false });
});

test("formatBody 对空串返回 text 格式与空记号数组", () => {
  expect(formatBody("")).toEqual({ format: "text", tokens: [], highlighted: false });
});

test("formatBody 对不是合法 JSON 的类 JSON 文本按纯文本处理", () => {
  const raw = '{"a":1,}';
  expect(formatBody(raw)).toEqual({ format: "text", tokens: [tok("text", raw)], highlighted: false });
});

// ---------------------------------------------------------------- formatBody: highlighted 与 maxTokens

test("formatBody 默认（不限制记号数）时 json 与 sse 的 highlighted 为 true", () => {
  expect(formatBody('{"a":1}').highlighted).toBe(true);
  expect(formatBody("123").highlighted).toBe(true);
  expect(formatBody('data: {"a":1}\ndata: [DONE]').highlighted).toBe(true);
});

test("formatBody 的 text 格式 highlighted 恒为 false，无论长度与 maxTokens", () => {
  expect(formatBody("hello").highlighted).toBe(false);
  expect(formatBody("x".repeat(100_000)).highlighted).toBe(false);
  expect(formatBody("hello", 0).highlighted).toBe(false);
  expect(formatBody("hello", Infinity).highlighted).toBe(false);
  expect(formatBody("").highlighted).toBe(false);
  expect(formatBody("", 0)).toEqual({ format: "text", tokens: [], highlighted: false });
});

test("formatBody 的 text 格式不受 maxTokens 影响，仍是单个 text 记号", () => {
  expect(formatBody("hello\nworld", 1)).toEqual({
    format: "text",
    tokens: [tok("text", "hello\nworld")],
    highlighted: false,
  });
});

test("formatBody 的 json 记号数恰为 maxTokens 时仍着色，多于 maxTokens 才降级", () => {
  const raw = '{"a":[1,2,{"b":null}],"c":"d"}';
  const pretty = JSON.stringify(JSON.parse(raw), null, 2);
  const count = tokenizeJson(pretty).length;

  const within = formatBody(raw, count);
  expect(within.format).toBe("json");
  expect(within.highlighted).toBe(true);
  expect(joined(within.tokens)).toBe(pretty);
  expect(within.tokens.length).toBeGreaterThan(1);

  const over = formatBody(raw, count - 1);
  expect(over.format).toBe("json");
  expect(over.highlighted).toBe(false);
  expect(over.tokens).toEqual([tok("text", pretty)]);
});

test("formatBody 的 json 超限时返回单个 text 记号，内容是美化后的完整文本", () => {
  const raw = '{"a":1,"b":[true,false]}';
  const result = formatBody(raw, 0);
  expect(result).toEqual({
    format: "json",
    tokens: [tok("text", JSON.stringify(JSON.parse(raw), null, 2))],
    highlighted: false,
  });
});

test("formatBody 的 sse 超限时返回单个 text 记号，内容是逐行处理后的完整文本，format 仍为 sse", () => {
  const raw = 'event: x\ndata: { "a": 1 }\n\ndata: [DONE]\n';
  const result = formatBody(raw, 1);
  expect(result).toEqual({
    format: "sse",
    tokens: [tok("text", 'event: x\ndata: {"a":1}\n\ndata: [DONE]\n')],
    highlighted: false,
  });
});

test("formatBody 超限降级前后文本一致（不变式：tokens 拼接 == 格式化后的文本）", () => {
  const raw = '{"k":["v",1,null]}';
  expect(joined(formatBody(raw, 0).tokens)).toBe(joined(formatBody(raw).tokens));
  const sse = 'data: {"a":[1,2]}\ndata: x';
  expect(joined(formatBody(sse, 1).tokens)).toBe(joined(formatBody(sse).tokens));
});

test("formatBody 对超过 1 MB 的 JSON 仍然格式化（没有字符数上限）", () => {
  const items = Array.from({ length: 30_000 }, (_, i) => ({ id: i, text: "xxxxxxxxxxxxxxxxxxxxxxxx" }));
  const raw = JSON.stringify({ items });
  expect(raw.length).toBeGreaterThan(1_000_000);
  const pretty = JSON.stringify(JSON.parse(raw), null, 2);

  const full = formatBody(raw);
  expect(full.format).toBe("json");
  expect(full.highlighted).toBe(true);
  expect(joined(full.tokens)).toBe(pretty);

  // 限制着色记号数时仍给出完整的美化文本，只是不着色
  const limited = formatBody(raw, 50_000);
  expect(limited.format).toBe("json");
  expect(limited.highlighted).toBe(false);
  expect(limited.tokens).toEqual([tok("text", pretty)]);
});
