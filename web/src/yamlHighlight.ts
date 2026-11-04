// YAML 行高亮：逐行、无跨行状态的轻量着色，只用于编辑器展示，不做解析（校验由服务端完成）。
// 每行记号的 text 依次拼接等于该行原文，相邻同类型记号合并，不产生空记号。

export type YamlTokenType =
  | "comment"
  | "key"
  | "string"
  | "number"
  | "boolean"
  | "null"
  | "punct"
  | "plain";
export type YamlToken = { type: YamlTokenType; text: string };

const BOOLEAN = /^(true|false|True|False|TRUE|FALSE)$/;
const NULL = /^(null|Null|NULL|~)$/;
const NUMBER = /^([-+]?(\d+(\.\d*)?|\.\d+)([eE][-+]?\d+)?|0x[0-9a-fA-F]+)$/;

function scalarType(text: string): YamlTokenType {
  if (BOOLEAN.test(text)) return "boolean";
  if (NULL.test(text)) return "null";
  if (NUMBER.test(text)) return "number";
  return "plain";
}

const isBlank = (c: string | undefined) => c === undefined || c === " " || c === "\t";

/** s[start] 为引号时，返回闭合引号之后的位置；未闭合返回 -1。双引号支持 \ 转义，单引号用 '' 转义。 */
function quoteEnd(s: string, start: number): number {
  const quote = s[start];
  for (let i = start + 1; i < s.length; i++) {
    if (quote === '"' && s[i] === "\\") i++;
    else if (s[i] === quote) {
      if (quote === "'" && s[i + 1] === "'") i++;
      else return i + 1;
    }
  }
  return -1;
}

/** 键的结束位置（冒号所在下标）；该行没有键时返回 -1。 */
function keyColon(s: string): number {
  if (s[0] === '"' || s[0] === "'") {
    const end = quoteEnd(s, 0);
    return end !== -1 && s[end] === ":" && (end + 1 === s.length || isBlank(s[end + 1])) ? end : -1;
  }
  for (let i = 0; i < s.length; i++) {
    // 空白后的 # 开始注释，其后的冒号不再是键分隔符。
    if (s[i] === "#" && i > 0 && isBlank(s[i - 1])) return -1;
    if (s[i] === ":" && (i + 1 === s.length || isBlank(s[i + 1]))) return i;
  }
  return -1;
}

function highlightLine(line: string): YamlToken[] {
  const tokens: YamlToken[] = [];
  const push = (type: YamlTokenType, text: string) => {
    if (text === "") return;
    const last = tokens[tokens.length - 1];
    if (last && last.type === type) last.text += text;
    else tokens.push({ type, text });
  };
  /** 切出开头的空白记为 plain，返回其后的剩余部分。 */
  const skipSpace = (s: string) => {
    const rest = s.trimStart();
    push("plain", s.slice(0, s.length - rest.length));
    return rest;
  };

  let rest = skipSpace(line);
  if (rest.startsWith("#")) {
    push("comment", rest);
    return tokens;
  }
  if (rest === "---" || rest === "...") {
    push("punct", rest);
    return tokens;
  }
  // 序列项前缀 "- "（可连续出现）。
  while (rest[0] === "-" && (rest.length === 1 || /\s/.test(rest[1]))) {
    push("punct", "-");
    rest = skipSpace(rest.slice(1));
  }

  const colon = keyColon(rest);
  if (colon !== -1) {
    const key = rest.slice(0, colon);
    const name = key.trimEnd();
    push("key", name);
    push("plain", key.slice(name.length));
    push("punct", ":");
    rest = skipSpace(rest.slice(colon + 1));
  }
  highlightValue(rest, push);
  return tokens;
}

/** 值部分（已去掉前导空白）：引号字符串、未加引号的标量与行尾注释。 */
function highlightValue(rest: string, push: (type: YamlTokenType, text: string) => void) {
  let scalar = rest;
  let tail = "";
  if (rest[0] === '"' || rest[0] === "'") {
    const end = quoteEnd(rest, 0);
    if (end === -1) {
      push("string", rest);
      return;
    }
    push("string", rest.slice(0, end));
    tail = rest.slice(end);
  } else {
    // 注释从值开头或空白之后的 # 开始。
    for (let i = 0; i < rest.length; i++) {
      if (rest[i] === "#" && (i === 0 || isBlank(rest[i - 1]))) {
        scalar = rest.slice(0, i);
        tail = rest.slice(i);
        break;
      }
    }
    const body = scalar.trimEnd();
    push(scalarType(body), body);
    push("plain", scalar.slice(body.length));
  }
  // 引号字符串之后：空白、注释或其他内容（非法 YAML，按 plain 处理）。
  const comment = tail.search(/#/);
  if (comment !== -1 && tail.slice(0, comment).trim() === "") {
    push("plain", tail.slice(0, comment));
    push("comment", tail.slice(comment));
  } else {
    push("plain", tail);
  }
}

/** 按行高亮 YAML；行按 "\n" 切分，返回每行的记号数组。 */
export function highlightYaml(text: string): YamlToken[][] {
  return text.split("\n").map(highlightLine);
}
