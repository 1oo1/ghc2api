// 配置编辑器的纯函数：状态判断、Tab 缩进与字段名拼接。
import type { ConfigValidation } from "./api";

export type ConfigEditorState = {
  /** 上次加载或保存成功的内容。 */
  original: string;
  /** 编辑中的内容。 */
  content: string;
  writable: boolean;
  saving: boolean;
  /** 最近一次校验对应的内容与结果；没有时为 null。 */
  validation: { content: string; result: ConfigValidation } | null;
};

export function isDirty(s: ConfigEditorState): boolean {
  return s.content !== s.original;
}

/** 可写、未在保存、有修改，且当前内容已通过服务端校验时才能保存。 */
export function canSave(s: ConfigEditorState): boolean {
  return (
    s.writable &&
    !s.saving &&
    isDirty(s) &&
    s.validation?.content === s.content &&
    s.validation.result.valid
  );
}

/** Tab 键：用两个空格替换 [start, end) 选区，cursor 为插入后的光标位置。 */
export function insertIndent(
  value: string,
  start: number,
  end: number,
): { value: string; cursor: number } {
  return { value: value.slice(0, start) + "  " + value.slice(end), cursor: start + 2 };
}

/** 字段名列表，用「、」连接。 */
export function fieldList(fields: string[]): string {
  return fields.join("、");
}

/** 把 CRLF 换成 LF 供编辑（textarea 会规范化为 LF），并记录原文是否使用 CRLF；孤立的 "\r" 保留。 */
export function toEditorText(text: string): { text: string; crlf: boolean } {
  return { text: text.replaceAll("\r\n", "\n"), crlf: text.includes("\r\n") };
}

/** 保存前按原文的行尾风格还原：crlf 为 true 时把 LF 换成 CRLF。 */
export function fromEditorText(text: string, crlf: boolean): string {
  return crlf ? text.replaceAll("\n", "\r\n") : text;
}
