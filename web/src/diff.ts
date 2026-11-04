// 行级 diff：公共前后缀 + LCS，用于保存配置前的差异预览。

export type DiffLine = {
  type: "equal" | "add" | "del";
  text: string;
  /** equal 与 del 带旧文本行号（从 1 起）。 */
  oldLine?: number;
  /** equal 与 add 带新文本行号（从 1 起）。 */
  newLine?: number;
};
export type DiffItem = DiffLine | { type: "skip"; count: number };

/** 中间区（去掉公共前后缀后）旧行数 × 新行数超过该值时不做 LCS，避免占用过多内存与时间。 */
const MAX_LCS_CELLS = 4_000_000;

/** 按行切分；结尾的换行不产生空行，空文本没有行。 */
export function splitLines(text: string): string[] {
  if (text === "") return [];
  const lines = text.split("\n");
  if (lines[lines.length - 1] === "") lines.pop();
  return lines;
}

type Op = { type: "equal" | "add" | "del"; text: string };

/** 中间区的最小编辑序列（LCS）。 */
function lcsOps(a: string[], b: string[]): Op[] {
  const n = a.length;
  const m = b.length;
  const width = m + 1;
  // table[i * width + j] = a[i:] 与 b[j:] 的 LCS 长度。
  const table = new Uint32Array((n + 1) * width);
  for (let i = n - 1; i >= 0; i--) {
    for (let j = m - 1; j >= 0; j--) {
      table[i * width + j] =
        a[i] === b[j]
          ? table[(i + 1) * width + j + 1] + 1
          : Math.max(table[(i + 1) * width + j], table[i * width + j + 1]);
    }
  }
  const ops: Op[] = [];
  let i = 0;
  let j = 0;
  while (i < n && j < m) {
    if (a[i] === b[j]) {
      ops.push({ type: "equal", text: a[i] });
      i++;
      j++;
    } else if (table[(i + 1) * width + j] >= table[i * width + j + 1]) {
      ops.push({ type: "del", text: a[i++] });
    } else {
      ops.push({ type: "add", text: b[j++] });
    }
  }
  while (i < n) ops.push({ type: "del", text: a[i++] });
  while (j < m) ops.push({ type: "add", text: b[j++] });
  return ops;
}

/** 把每一处连续改动整理成「先删后增」（LCS 回溯可能交错出现）。 */
function deletesFirst(ops: Op[]): Op[] {
  const result: Op[] = [];
  for (let i = 0; i < ops.length; ) {
    if (ops[i].type === "equal") {
      result.push(ops[i++]);
      continue;
    }
    const dels: Op[] = [];
    const adds: Op[] = [];
    for (; i < ops.length && ops[i].type !== "equal"; i++) {
      (ops[i].type === "del" ? dels : adds).push(ops[i]);
    }
    result.push(...dels, ...adds);
  }
  return result;
}

/** 逐行比较两段文本（按 splitLines 切分，因此仅结尾换行不同视为相同）。 */
export function diffLines(before: string, after: string): DiffLine[] {
  const a = splitLines(before);
  const b = splitLines(after);
  let prefix = 0;
  while (prefix < a.length && prefix < b.length && a[prefix] === b[prefix]) prefix++;
  let suffix = 0;
  while (
    suffix < a.length - prefix &&
    suffix < b.length - prefix &&
    a[a.length - 1 - suffix] === b[b.length - 1 - suffix]
  ) {
    suffix++;
  }
  const oldMid = a.slice(prefix, a.length - suffix);
  const newMid = b.slice(prefix, b.length - suffix);

  const middle: Op[] =
    oldMid.length * newMid.length > MAX_LCS_CELLS
      ? [
          ...oldMid.map((text): Op => ({ type: "del", text })),
          ...newMid.map((text): Op => ({ type: "add", text })),
        ]
      : deletesFirst(lcsOps(oldMid, newMid));
  const ops: Op[] = [
    ...a.slice(0, prefix).map((text): Op => ({ type: "equal", text })),
    ...middle,
    ...a.slice(a.length - suffix).map((text): Op => ({ type: "equal", text })),
  ];

  let oldLine = 0;
  let newLine = 0;
  return ops.map((op): DiffLine => {
    if (op.type === "equal") return { type: "equal", text: op.text, oldLine: ++oldLine, newLine: ++newLine };
    if (op.type === "del") return { type: "del", text: op.text, oldLine: ++oldLine };
    return { type: "add", text: op.text, newLine: ++newLine };
  });
}

/** 只保留每处改动前后各 context 行未变内容，其余连续未变行折叠为一个 skip；没有改动时返回 []。 */
export function foldDiff(lines: DiffLine[], context = 3): DiffItem[] {
  const keep = new Array<boolean>(lines.length).fill(false);
  let changed = false;
  lines.forEach((line, i) => {
    if (line.type === "equal") return;
    changed = true;
    const from = Math.max(0, i - context);
    const to = Math.min(lines.length - 1, i + context);
    for (let k = from; k <= to; k++) keep[k] = true;
  });
  if (!changed) return [];

  const items: DiffItem[] = [];
  let skipped = 0;
  lines.forEach((line, i) => {
    if (keep[i]) {
      if (skipped > 0) items.push({ type: "skip", count: skipped });
      skipped = 0;
      items.push(line);
    } else {
      skipped++;
    }
  });
  if (skipped > 0) items.push({ type: "skip", count: skipped });
  return items;
}
