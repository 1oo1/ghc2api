// 配置编辑器纯函数（configEditor.ts）的测试，覆盖的导出：
//   - isDirty(state)：content !== original。
//   - canSave(state)：writable && !saving && 已修改 && validation 对应当前 content && 校验有效。
//   - insertIndent(value, start, end)：用两个空格替换 [start, end) 选区，cursor 为 start + 2。
//   - fieldList(fields)：用「、」连接字段名，空数组返回 ""。
//   - toEditorText(text)：把 CRLF 换成 LF 供编辑（textarea 会规范化为 LF），并记录原文是否使用 CRLF。
//   - fromEditorText(text, crlf)：保存前按原文的行尾风格还原。
import { expect, test } from "vitest";
import {
  canSave,
  fieldList,
  fromEditorText,
  insertIndent,
  isDirty,
  toEditorText,
  type ConfigEditorState,
} from "./configEditor";

/** 一个「可以保存」的基准状态，各用例在此基础上改动一个条件。 */
function savable(overrides: Partial<ConfigEditorState> = {}): ConfigEditorState {
  return {
    original: "a: 1\n",
    content: "a: 2\n",
    writable: true,
    saving: false,
    validation: { content: "a: 2\n", result: { valid: true, restart_required: [] } },
    ...overrides,
  };
}

test.each<[string, string, string, boolean]>([
  ["内容相同", "a: 1", "a: 1", false],
  ["内容不同", "a: 1", "a: 2", true],
  ["仅末尾换行不同也算已修改", "a: 1", "a: 1\n", true],
  ["都为空", "", "", false],
])("isDirty：%s", (_name, original, content, expected) => {
  expect(isDirty({ ...savable(), original, content })).toBe(expected);
});

test("canSave：全部条件满足时为 true", () => {
  expect(canSave(savable())).toBe(true);
});

test("canSave：有效校验带 restart_required 仍可保存", () => {
  expect(canSave(savable({ validation: { content: "a: 2\n", result: { valid: true, restart_required: ["listen"] } } }))).toBe(true);
});

test.each<[string, Partial<ConfigEditorState>]>([
  ["配置不可写", { writable: false }],
  ["正在保存", { saving: true }],
  ["内容未修改", { content: "a: 1\n", validation: { content: "a: 1\n", result: { valid: true, restart_required: [] } } }],
  ["校验结果对应的不是当前内容", { validation: { content: "a: 1\n", result: { valid: true, restart_required: [] } } }],
  ["校验无效", { validation: { content: "a: 2\n", result: { valid: false, error: "yaml: line 1: bad", line: 1, restart_required: [] } } }],
  ["没有校验结果", { validation: null }],
])("canSave：%s 时为 false", (_name, overrides) => {
  expect(canSave(savable(overrides))).toBe(false);
});

test.each<[string, string, number, number, { value: string; cursor: number }]>([
  ["无选区：在光标处插入", "abc", 1, 1, { value: "a  bc", cursor: 3 }],
  ["光标在开头", "abc", 0, 0, { value: "  abc", cursor: 2 }],
  ["光标在末尾", "abc", 3, 3, { value: "abc  ", cursor: 5 }],
  ["选区被两个空格替换", "abcdef", 1, 4, { value: "a  ef", cursor: 3 }],
  ["全选被替换", "abc", 0, 3, { value: "  ", cursor: 2 }],
  ["选区跨越换行", "a\nb\nc", 1, 4, { value: "a  c", cursor: 3 }],
  ["空文本", "", 0, 0, { value: "  ", cursor: 2 }],
])("insertIndent：%s", (_name, value, start, end, expected) => {
  expect(insertIndent(value, start, end)).toEqual(expected);
});

test.each<[string[], string]>([
  [[], ""],
  [["listen"], "listen"],
  [["listen", "data_path"], "listen、data_path"],
  [["a", "b", "c"], "a、b、c"],
])("fieldList(%j) = %j", (fields, expected) => {
  expect(fieldList(fields)).toBe(expected);
});

test.each<[string, string, string, boolean]>([
  ["LF 原样", "a: 1\nb: 2\n", "a: 1\nb: 2\n", false],
  ["CRLF 换成 LF", "a: 1\r\nb: 2\r\n", "a: 1\nb: 2\n", true],
  ["无换行", "a: 1", "a: 1", false],
  ["空文本", "", "", false],
  ["孤立的 \\r 保留", "a: 1\rb", "a: 1\rb", false],
])("toEditorText：%s", (_name, input, text, crlf) => {
  expect(toEditorText(input)).toEqual({ text, crlf });
});

test.each<[string, string, boolean, string]>([
  ["LF 文件保持 LF", "a: 1\nb: 2\n", false, "a: 1\nb: 2\n"],
  ["CRLF 文件还原 CRLF", "a: 1\nb: 2\n", true, "a: 1\r\nb: 2\r\n"],
  ["CRLF 文件无换行", "a: 1", true, "a: 1"],
])("fromEditorText：%s", (_name, input, crlf, expected) => {
  expect(fromEditorText(input, crlf)).toBe(expected);
});

test("CRLF 往返：未编辑时还原后与原文完全相同", () => {
  const original = "# 注释\r\nlisten: 127.0.0.1:8080\r\n";
  const { text, crlf } = toEditorText(original);
  expect(fromEditorText(text, crlf)).toBe(original);
});
