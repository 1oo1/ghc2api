// JSON 高亮：把 JSON 文本切分为记号，供 CodeBlock 以文本节点渲染（不生成 HTML 字符串）。纯函数，无 React。

export type CodeToken = {
  type: "key" | "string" | "number" | "boolean" | "null" | "punct" | "text";
  text: string;
};

const NUMBER = /-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?/y;
const WHITESPACE = /\s/;
const PUNCT = "{}[],:";

/** 追加记号；相邻的 "text" 合并，避免空白与无法识别的字符产生大量碎片。 */
function push(tokens: CodeToken[], type: CodeToken["type"], text: string) {
  const last = tokens[tokens.length - 1];
  if (type === "text" && last?.type === "text") last.text += text;
  else tokens.push({ type, text });
}

/** 返回以 start 处引号开头的字符串字面量的结束位置（不含），未闭合时到文本末尾。 */
function stringEnd(text: string, start: number): number {
  for (let i = start + 1; i < text.length; i++) {
    if (text[i] === "\\") i++;
    else if (text[i] === '"') return i + 1;
  }
  return text.length;
}

/**
 * 把 JSON 文本切分为高亮记号，所有 token.text 依次拼接等于输入。
 * 紧跟（可有空白）冒号的字符串为 key；不要求输入是合法 JSON，无法识别的字符为 text，不会抛异常。
 */
export function tokenizeJson(text: string): CodeToken[] {
  const tokens: CodeToken[] = [];
  tokenizeInto(tokens, text, Infinity);
  return tokens;
}

/**
 * 把 text 的记号追加到 tokens。tokens 的长度超过 budget 时立即停止并返回 false（此时 tokens 内容不再有意义）；
 * 记号数只增不减，因此第一次超限就等价于“最终记号数超过 budget”。
 */
function tokenizeInto(tokens: CodeToken[], text: string, budget: number): boolean {
  let i = 0;
  while (i < text.length) {
    if (tokens.length > budget) return false;
    const ch = text[i];
    if (ch === '"') {
      const end = stringEnd(text, i);
      let next = end;
      while (next < text.length && WHITESPACE.test(text[next])) next++;
      push(tokens, text[next] === ":" ? "key" : "string", text.slice(i, end));
      i = end;
    } else if (PUNCT.includes(ch)) {
      push(tokens, "punct", ch);
      i++;
    } else if (ch === "-" || (ch >= "0" && ch <= "9")) {
      NUMBER.lastIndex = i;
      const match = NUMBER.exec(text);
      if (match) {
        push(tokens, "number", match[0]);
        i += match[0].length;
      } else {
        push(tokens, "text", ch);
        i++;
      }
    } else if (text.startsWith("true", i) || text.startsWith("false", i)) {
      const word = text.startsWith("true", i) ? "true" : "false";
      push(tokens, "boolean", word);
      i += word.length;
    } else if (text.startsWith("null", i)) {
      push(tokens, "null", "null");
      i += 4;
    } else {
      // 空白与无法识别的字符；按码点前进，避免拆开代理对。
      const char = String.fromCodePoint(text.codePointAt(i) as number);
      push(tokens, "text", char);
      i += char.length;
    }
  }
  return tokens.length <= budget;
}

export type FormattedBody = {
  format: "json" | "sse" | "text";
  tokens: CodeToken[];
  /** 是否按记号着色；format 为 "text"，或记号数超过 maxTokens 时为 false。 */
  highlighted: boolean;
};

/** 尝试把文本解析为 JSON；失败返回 undefined（值本身不会是 undefined）。 */
function tryParse(text: string): { value: unknown } | undefined {
  try {
    return { value: JSON.parse(text) };
  } catch {
    return undefined;
  }
}

/** SSE 行中 data 行的 JSON 载荷（去掉 "data:" 与开头一个可选空格后能解析时）；其他行为 undefined。 */
function ssePayload(line: string): { value: unknown } | undefined {
  return line.startsWith("data:")
    ? tryParse(line.slice(5).replace(/^ /, ""))
    : undefined;
}

/** 只拼接 SSE 逐行处理后的文本、不生成记号，用于记号数超限时的降级显示。 */
function formatSseText(lines: string[]): string {
  return lines
    .map((line) => {
      const payload = ssePayload(line);
      return payload ? "data: " + JSON.stringify(payload.value) : line;
    })
    .join("\n");
}

/**
 * 格式化正文（格式化没有大小上限）：
 * - 整体是合法 JSON：缩进 2 空格后高亮。
 * - 否则存在以 "data:" 开头的行：按 SSE 逐行处理，data 行的 JSON 重写为紧凑格式并高亮，其他行原样。
 * - 否则按纯文本（highlighted 为 false）。
 * 记号数超过 maxTokens 时不着色：tokens 为单个 text 记号，内容是格式化后的完整文本，format 不变；
 * 生成记号时一旦超限就立即停止，不会为超大正文构建完整的记号数组。
 */
export function formatBody(text: string, maxTokens = Infinity): FormattedBody {
  const json = tryParse(text);
  if (json) {
    const pretty = JSON.stringify(json.value, null, 2);
    const tokens: CodeToken[] = [];
    return tokenizeInto(tokens, pretty, maxTokens)
      ? { format: "json", tokens, highlighted: true }
      : plain("json", pretty);
  }
  const lines = text.split("\n");
  if (!lines.some((line) => line.startsWith("data:"))) {
    return {
      format: "text",
      tokens: text === "" ? [] : [{ type: "text", text }],
      highlighted: false,
    };
  }
  const tokens: CodeToken[] = [];
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i];
    if (i > 0) push(tokens, "text", "\n");
    const payload = ssePayload(line);
    if (payload) {
      push(tokens, "text", "data: ");
      if (!tokenizeInto(tokens, JSON.stringify(payload.value), maxTokens))
        return plain("sse", formatSseText(lines));
    } else if (line !== "") {
      push(tokens, "text", line);
    }
    if (tokens.length > maxTokens) return plain("sse", formatSseText(lines));
  }
  return { format: "sse", tokens, highlighted: true };
}

/** 超限降级：单个 text 记号，内容是格式化后的完整文本。 */
function plain(format: "json" | "sse", text: string): FormattedBody {
  return { format, tokens: [{ type: "text", text }], highlighted: false };
}
