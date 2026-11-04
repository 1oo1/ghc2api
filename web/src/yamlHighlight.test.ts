// YAML 行高亮（yamlHighlight.ts）的测试，覆盖的导出：
//   - highlightYaml(text)：按 "\n" 切分行，返回每行的记号数组；每行记号 text 拼接严格等于该行原文，
//     相邻同类型记号合并，不产生空 text 记号。规则见 /tmp 规格 §4.2：键/冒号、引号字符串、注释、
//     boolean/null/number、序列项、文档标记、未闭合引号。
// 规格未规定行尾 "\r" 与行尾空白归属哪种记号，因此这类输入只断言「拼接还原」与结构不变量，不断言记号类型。
import { expect, test } from "vitest";
import { highlightYaml, type YamlToken, type YamlTokenType } from "./yamlHighlight";

const t = (type: YamlTokenType, text: string): YamlToken => ({ type, text });

/** 结构不变量：行数与原文一致、逐行拼接还原、无空记号、相邻记号类型不同。 */
function expectInvariants(text: string) {
  const lines = highlightYaml(text);
  const raw = text.split("\n");
  expect(lines).toHaveLength(raw.length);
  lines.forEach((tokens, i) => {
    expect(tokens.map((x) => x.text).join("")).toBe(raw[i]);
    tokens.forEach((token, j) => {
      expect(token.text).not.toBe("");
      if (j > 0) expect(token.type).not.toBe(tokens[j - 1].type);
    });
  });
}

const samples: [string, string][] = [
  ["典型配置", "# 配置\nlisten: \":8080\"\nhost: 'a''b'\nlog_level: info # 级别\nsession_affinity: true\nitems:\n  - a\n  - key: 1.5\n---\n"],
  ["CRLF 换行", "a: 1\r\nb: \"x\"\r\n# c\r\n"],
  ["空行与行尾空白", "a: 1   \n\n   \nb:\t\n  # 注释  \n"],
  ["未闭合引号与奇怪输入", "a: \"abc # d\nb: 'x\n:::\n- \n-\n#\n\"\n"],
  ["仅空白", "   "],
  ["中文与特殊字符", "名称: 值 # 说明\nurl: http://x:80/a?b=c#d\n"],
];

test.each(samples)("每行记号拼接还原原文且无空记号、相邻类型不同：%s", (_name, text) => {
  expectInvariants(text);
});

// ---------------------------------------------------------------- 行切分

test.each<[string, string, number]>([
  ["空字符串为 1 行", "", 1],
  ["单行无换行", "a", 1],
  ["两行", "a\nb", 2],
  ["以换行结尾多出一个空行", "a\n", 2],
  ["仅换行", "\n", 2],
  ["中间空行", "a\n\nb", 3],
])("行数：%s", (_name, text, count) => {
  expect(highlightYaml(text)).toHaveLength(count);
});

test("空字符串与末尾换行后的行没有记号", () => {
  expect(highlightYaml("")).toEqual([[]]);
  expect(highlightYaml("a\n")[1]).toEqual([]);
  expect(highlightYaml("a\n\nb")[1]).toEqual([]);
});

// ---------------------------------------------------------------- 逐行规则

const lineCases: [string, string, YamlToken[]][] = [
  // 键与冒号
  ["简单键值", "a: 1", [t("key", "a"), t("punct", ":"), t("plain", " "), t("number", "1")]],
  ["普通字符串值为 plain，前导空白与值合并", "name: alice", [t("key", "name"), t("punct", ":"), t("plain", " alice")]],
  ["键后无值", "a:", [t("key", "a"), t("punct", ":")]],
  ["缩进为 plain", "  a: b", [t("plain", "  "), t("key", "a"), t("punct", ":"), t("plain", " b")]],
  ["冒号后无空格不是键", "a:b", [t("plain", "a:b")]],
  ["URL 不是键", "http://x:80", [t("plain", "http://x:80")]],
  ["键取第一个冒号，值中的冒号不再处理", "a: b: c", [t("key", "a"), t("punct", ":"), t("plain", " b: c")]],
  ["值为 URL", "url: http://x:80", [t("key", "url"), t("punct", ":"), t("plain", " http://x:80")]],
  // 带引号的键
  ["双引号键", '"k": v', [t("key", '"k"'), t("punct", ":"), t("plain", " v")]],
  ["单引号键", "'k': v", [t("key", "'k'"), t("punct", ":"), t("plain", " v")]],
  ["引号内的冒号不分隔键", '"a: b": 1', [t("key", '"a: b"'), t("punct", ":"), t("plain", " "), t("number", "1")]],
  // 引号字符串
  ["双引号字符串", 'a: "x y"', [t("key", "a"), t("punct", ":"), t("plain", " "), t("string", '"x y"')]],
  ["双引号内的 \\\" 转义", 'a: "x\\"y"', [t("key", "a"), t("punct", ":"), t("plain", " "), t("string", '"x\\"y"')]],
  ["单引号字符串", "a: 'x y'", [t("key", "a"), t("punct", ":"), t("plain", " "), t("string", "'x y'")]],
  ["单引号内的 '' 转义", "a: 'it''s'", [t("key", "a"), t("punct", ":"), t("plain", " "), t("string", "'it''s'")]],
  ["引号字符串内的 # 不是注释", 'a: "x # y"', [t("key", "a"), t("punct", ":"), t("plain", " "), t("string", '"x # y"')]],
  ["引号字符串后的注释", 'a: "x"  # c', [t("key", "a"), t("punct", ":"), t("plain", " "), t("string", '"x"'), t("plain", "  "), t("comment", "# c")]],
  ["没有键的整行字符串", '"abc"', [t("string", '"abc"')]],
  // 未闭合引号
  ["未闭合双引号到行尾整体为 string，不再识别注释", 'a: "abc # d', [t("key", "a"), t("punct", ":"), t("plain", " "), t("string", '"abc # d')]],
  ["未闭合单引号", "a: 'abc", [t("key", "a"), t("punct", ":"), t("plain", " "), t("string", "'abc")]],
  // 注释
  ["整行注释", "# 说明: a", [t("comment", "# 说明: a")]],
  ["缩进的整行注释", "  # c", [t("plain", "  "), t("comment", "# c")]],
  ["值后注释", "a: 1 # c", [t("key", "a"), t("punct", ":"), t("plain", " "), t("number", "1"), t("plain", " "), t("comment", "# c")]],
  ["plain 值后注释，空白并入 plain", "a: b #c", [t("key", "a"), t("punct", ":"), t("plain", " b "), t("comment", "#c")]],
  ["# 前无空白不是注释", "a: b#c", [t("key", "a"), t("punct", ":"), t("plain", " b#c")]],
  ["值开头的 # 是注释", "a: # c", [t("key", "a"), t("punct", ":"), t("plain", " "), t("comment", "# c")]],
  ["boolean 后注释", "a: true # c", [t("key", "a"), t("punct", ":"), t("plain", " "), t("boolean", "true"), t("plain", " "), t("comment", "# c")]],
  // 序列项
  ["序列项", "- a", [t("punct", "-"), t("plain", " a")]],
  ["缩进的序列项", "  - a", [t("plain", "  "), t("punct", "-"), t("plain", " a")]],
  ["序列项内的键值", "- key: value", [t("punct", "-"), t("plain", " "), t("key", "key"), t("punct", ":"), t("plain", " value")]],
  ["序列项内的数字", "- 3", [t("punct", "-"), t("plain", " "), t("number", "3")]],
  ["序列项内的字符串", '- "q"', [t("punct", "-"), t("plain", " "), t("string", '"q"')]],
  ["行内只有 -", "-", [t("punct", "-")]],
  // 文档标记
  ["文档开始标记", "---", [t("punct", "---")]],
  ["缩进的文档标记", "  ---", [t("plain", "  "), t("punct", "---")]],
  ["文档结束标记", "...", [t("punct", "...")]],
  // 空行
  ["空行没有记号", "", []],
];

test.each(lineCases)("%s", (_name, input, expected) => {
  expect(highlightYaml(input)).toEqual([expected]);
});

// ---------------------------------------------------------------- 未加引号的标量

const scalarCases: [string, YamlTokenType][] = [
  ["true", "boolean"],
  ["false", "boolean"],
  ["True", "boolean"],
  ["FALSE", "boolean"],
  ["null", "null"],
  ["Null", "null"],
  ["NULL", "null"],
  ["~", "null"],
  ["0", "number"],
  ["42", "number"],
  ["1.5", "number"],
  ["-3", "number"],
  ["+2", "number"],
  [".5", "number"],
  ["1.", "number"],
  ["1e3", "number"],
  ["-1.5E-3", "number"],
  ["0x1F", "number"],
  ["2h", "plain"],
  ["120m", "plain"],
  ["1.2.3", "plain"],
  ["0x", "plain"],
  ["-", "plain"],
  ["yes", "plain"],
  ["truee", "plain"],
  ["nulls", "plain"],
  ["abc", "plain"],
];

test.each(scalarCases)("标量 %s 的类型为 %s", (value, type) => {
  // plain 值与前导空白合并为一个记号；其余类型的值是独立记号。
  const valueTokens = type === "plain" ? [t("plain", ` ${value}`)] : [t("plain", " "), t(type, value)];
  expect(highlightYaml(`a: ${value}`)).toEqual([[t("key", "a"), t("punct", ":"), ...valueTokens]]);
});

test.each(scalarCases)("标量 %s 后带注释时类型仍为 %s", (value, type) => {
  const tokens = highlightYaml(`a: ${value} # c`)[0];
  expect(tokens.at(-1)).toEqual(t("comment", "# c"));
  // 去掉键、冒号、注释与纯空白记号后，只剩值本身（plain 值可能带着两侧空白）。
  const valueTokens = tokens.filter((x) => !["key", "punct", "comment"].includes(x.type) && x.text.trim() !== "");
  expect(valueTokens.map((x) => [x.type, x.text.trim()])).toEqual([[type, value]]);
});
