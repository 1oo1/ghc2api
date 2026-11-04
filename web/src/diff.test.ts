// 行级 diff（diff.ts）的测试，覆盖的导出：
//   - splitLines(text)：按 "\n" 切分；以 "\n" 结尾时去掉最后的空串；"" → []。
//   - diffLines(before, after)：行号从 1 开始，equal 带 oldLine/newLine，del 只带 oldLine，add 只带 newLine；
//     先去公共前后缀，中间部分做 LCS（结果为最小编辑）；中间部分行数乘积 > 4_000_000 时整体先删后增；
//     同一处修改中删除行在新增行之前。
//   - foldDiff(lines, context = 3)：保留每处改动前后各 context 行 equal，其余连续 equal 折叠为 { type: "skip", count }；
//     没有改动时返回 []。
import { expect, test } from "vitest";
import { diffLines, foldDiff, splitLines, type DiffItem, type DiffLine } from "./diff";

const join = (lines: string[]) => lines.join("\n");

// ---------------------------------------------------------------- splitLines

test.each<[string, string, string[]]>([
  ["空串没有行", "", []],
  ["无结尾换行", "a\nb", ["a", "b"]],
  ["结尾换行不产生空行", "a\nb\n", ["a", "b"]],
  ["仅一个换行是一个空行", "\n", [""]],
  ["结尾两个换行保留一个空行", "a\n\n", ["a", ""]],
  ["中间空行保留", "a\n\nb", ["a", "", "b"]],
])("splitLines：%s", (_name, text, expected) => {
  expect(splitLines(text)).toEqual(expected);
});

// ---------------------------------------------------------------- diffLines

test("相同输入全部为 equal，行号两侧一致", () => {
  expect(diffLines("a\nb\nc", "a\nb\nc")).toStrictEqual([
    { type: "equal", text: "a", oldLine: 1, newLine: 1 },
    { type: "equal", text: "b", oldLine: 2, newLine: 2 },
    { type: "equal", text: "c", oldLine: 3, newLine: 3 },
  ]);
});

test("两侧均为空文本时没有差异行", () => {
  expect(diffLines("", "")).toEqual([]);
});

test("末尾换行不影响比较（按 splitLines 的行比较）", () => {
  expect(diffLines("a\nb\n", "a\nb").every((l) => l.type === "equal")).toBe(true);
});

test("纯新增：add 只带 newLine", () => {
  expect(diffLines("a\nc", "a\nb\nc")).toStrictEqual([
    { type: "equal", text: "a", oldLine: 1, newLine: 1 },
    { type: "add", text: "b", newLine: 2 },
    { type: "equal", text: "c", oldLine: 2, newLine: 3 },
  ]);
});

test("纯删除：del 只带 oldLine", () => {
  expect(diffLines("a\nb\nc", "a\nc")).toStrictEqual([
    { type: "equal", text: "a", oldLine: 1, newLine: 1 },
    { type: "del", text: "b", oldLine: 2 },
    { type: "equal", text: "c", oldLine: 3, newLine: 2 },
  ]);
});

test("中间修改：删除行排在新增行之前", () => {
  expect(diffLines("a\nb\nc", "a\nX\nc")).toStrictEqual([
    { type: "equal", text: "a", oldLine: 1, newLine: 1 },
    { type: "del", text: "b", oldLine: 2 },
    { type: "add", text: "X", newLine: 2 },
    { type: "equal", text: "c", oldLine: 3, newLine: 3 },
  ]);
});

test("多行修改：连续的删除行在连续的新增行之前", () => {
  expect(diffLines("a\nb1\nb2\nc", "a\nx1\nx2\nx3\nc")).toStrictEqual([
    { type: "equal", text: "a", oldLine: 1, newLine: 1 },
    { type: "del", text: "b1", oldLine: 2 },
    { type: "del", text: "b2", oldLine: 3 },
    { type: "add", text: "x1", newLine: 2 },
    { type: "add", text: "x2", newLine: 3 },
    { type: "add", text: "x3", newLine: 4 },
    { type: "equal", text: "c", oldLine: 4, newLine: 5 },
  ]);
});

test("空文本转非空文本：全部为 add", () => {
  expect(diffLines("", "a\nb")).toStrictEqual([
    { type: "add", text: "a", newLine: 1 },
    { type: "add", text: "b", newLine: 2 },
  ]);
});

test("非空文本转空文本：全部为 del", () => {
  expect(diffLines("a\nb", "")).toStrictEqual([
    { type: "del", text: "a", oldLine: 1 },
    { type: "del", text: "b", oldLine: 2 },
  ]);
});

test("完全不同的文本：先全删后全增", () => {
  expect(diffLines("a\nb", "x\ny").map((l) => l.type)).toEqual(["del", "del", "add", "add"]);
});

/** 由 diff 结果还原两侧文本行：旧侧 = equal + del，新侧 = equal + add。 */
function sides(lines: DiffLine[]) {
  return {
    before: lines.filter((l) => l.type !== "add").map((l) => l.text),
    after: lines.filter((l) => l.type !== "del").map((l) => l.text),
  };
}

/** 检查行号字段规则：行号自 1 起逐一递增，且 add/del 不带对侧行号。 */
function expectLineNumbers(lines: DiffLine[]) {
  let oldLine = 0;
  let newLine = 0;
  for (const l of lines) {
    if (l.type !== "add") oldLine += 1;
    if (l.type !== "del") newLine += 1;
    if (l.type === "equal") expect(l).toStrictEqual({ type: "equal", text: l.text, oldLine, newLine });
    if (l.type === "del") expect(l).toStrictEqual({ type: "del", text: l.text, oldLine });
    if (l.type === "add") expect(l).toStrictEqual({ type: "add", text: l.text, newLine });
  }
}

/** 动态规划求 LCS 长度，作为最小编辑的基准。 */
function lcsLength(a: string[], b: string[]): number {
  const dp = Array.from({ length: a.length + 1 }, () => new Array<number>(b.length + 1).fill(0));
  for (let i = 1; i <= a.length; i++) {
    for (let j = 1; j <= b.length; j++) {
      dp[i][j] = a[i - 1] === b[j - 1] ? dp[i - 1][j - 1] + 1 : Math.max(dp[i - 1][j], dp[i][j - 1]);
    }
  }
  return dp[a.length][b.length];
}

test("经典用例 ABCABBA / CBABAC：equal 行数等于 LCS 长度 4", () => {
  const before = join([..."ABCABBA"]);
  const after = join([..."CBABAC"]);
  const lines = diffLines(before, after);
  expect(lines.filter((l) => l.type === "equal")).toHaveLength(4);
  expect(lines.filter((l) => l.type === "del")).toHaveLength(3);
  expect(lines.filter((l) => l.type === "add")).toHaveLength(2);
  expect(sides(lines)).toEqual({ before: [..."ABCABBA"], after: [..."CBABAC"] });
  expectLineNumbers(lines);
});

test("随机输入：可还原两侧、行号正确、equal 数等于 LCS、同一处修改中删除先于新增", () => {
  // 固定种子的线性同余发生器，保证用例可复现。
  let seed = 12345;
  const rand = (n: number) => {
    seed = (seed * 1103515245 + 12345) % 2147483648;
    return seed % n;
  };
  for (let round = 0; round < 200; round++) {
    const a = Array.from({ length: rand(14) }, () => "xyz".charAt(rand(3)));
    const b = Array.from({ length: rand(14) }, () => "xyz".charAt(rand(3)));
    const lines = diffLines(a.join("\n"), b.join("\n"));
    expect(sides(lines)).toEqual({ before: a, after: b });
    expectLineNumbers(lines);
    expect(lines.filter((l) => l.type === "equal")).toHaveLength(lcsLength(a, b));
    // 相邻的非 equal 行中不能出现 add 紧跟 del。
    lines.forEach((l, i) => {
      if (i > 0 && l.type === "del") expect(lines[i - 1].type).not.toBe("add");
    });
  }
});

// ---------------------------------------------------------------- 超大中间区

/** 构造「公共头 + 中间区 + 公共尾」的两侧文本；中间区首尾各有一个互不相同的行，其余 shared 行两侧相同。 */
function bigInput(sharedCount: number, middleLength: number) {
  const shared = Array.from({ length: sharedCount }, (_, i) => `c${i}`);
  const fill = (head: string, tail: string) => {
    const mid = [head, ...shared, tail];
    expect(mid).toHaveLength(middleLength);
    return mid;
  };
  const oldMid = fill("old-first", "old-last");
  const newMid = fill("new-first", "new-last");
  return {
    before: join(["head", ...oldMid, "tail"]),
    after: join(["head", ...newMid, "tail"]),
  };
}

test("中间区行数乘积 > 4_000_000：不做 LCS，先全删再全增，并保留公共前后缀", () => {
  // 中间区 2001 × 2001 = 4_004_001 > 4_000_000，其中含 1999 个两侧相同的行，LCS 本可匹配它们。
  const n = 2001;
  const { before, after } = bigInput(n - 2, n);
  const lines = diffLines(before, after);

  expect(lines).toHaveLength(1 + n + n + 1);
  expect(lines[0]).toStrictEqual({ type: "equal", text: "head", oldLine: 1, newLine: 1 });
  expect(lines.slice(1, 1 + n).every((l) => l.type === "del")).toBe(true);
  expect(lines.slice(1 + n, 1 + 2 * n).every((l) => l.type === "add")).toBe(true);
  expect(lines.at(-1)).toStrictEqual({ type: "equal", text: "tail", oldLine: n + 2, newLine: n + 2 });
  expect(lines.filter((l) => l.type === "equal")).toHaveLength(2);
  expect(sides(lines)).toEqual({ before: splitLines(before), after: splitLines(after) });
  expectLineNumbers(lines);
});

test("中间区行数乘积恰为 4_000_000：仍做 LCS，保留相同行", () => {
  // 中间区 2000 × 2000 = 4_000_000，不超过阈值；共享的 1998 行 + 头尾 = 1998 + 2 个 equal。
  const n = 2000;
  const { before, after } = bigInput(n - 2, n);
  const lines = diffLines(before, after);

  expect(lines.filter((l) => l.type === "equal")).toHaveLength(n - 2 + 2);
  expect(lines.filter((l) => l.type === "del")).toHaveLength(2);
  expect(lines.filter((l) => l.type === "add")).toHaveLength(2);
  expect(sides(lines)).toEqual({ before: splitLines(before), after: splitLines(after) });
});

test("超大中间区的两侧行数不同时同样正确（新侧只含新增）", () => {
  // 旧侧中间区 2500 行、新侧 1700 行：乘积 4_250_000 > 4_000_000，且两侧不含公共行。
  const oldMid = Array.from({ length: 2500 }, (_, i) => `o${i}`);
  const newMid = Array.from({ length: 1700 }, (_, i) => `n${i}`);
  const lines = diffLines(join(["h", ...oldMid, "t"]), join(["h", ...newMid, "t"]));
  expect(lines.map((l) => l.type)).toEqual([
    "equal",
    ...new Array<string>(2500).fill("del"),
    ...new Array<string>(1700).fill("add"),
    "equal",
  ]);
});

// ---------------------------------------------------------------- foldDiff

/** 由形如 "==+-==" 的模式串构造 diff 行：= equal，+ add，- del（行号对折叠无影响，仅填 text）。 */
function pattern(p: string): DiffLine[] {
  return [...p].map((c, i) => ({ type: c === "=" ? "equal" : c === "+" ? "add" : "del", text: `l${i}` }));
}

/** 把折叠结果压缩成可读形式：equal/add/del 用 =/+/-，skip 用 "~count"。 */
function shape(items: DiffItem[]): string[] {
  return items.map((it) => (it.type === "skip" ? `~${it.count}` : { equal: "=", add: "+", del: "-" }[it.type]));
}

const rep = (c: string, n: number) => c.repeat(n);

test("没有任何改动时返回 []", () => {
  expect(foldDiff(pattern(rep("=", 20)))).toEqual([]);
  expect(foldDiff([])).toEqual([]);
});

test("默认 context=3：首尾长 equal 段折叠为 skip，count 为被省略行数", () => {
  const items = foldDiff(pattern(rep("=", 10) + "+" + rep("=", 10)));
  expect(shape(items)).toEqual(["~7", "=", "=", "=", "+", "=", "=", "=", "~7"]);
});

test("equal 段恰为 context 行时不折叠；多 1 行则折叠 1 行", () => {
  expect(shape(foldDiff(pattern("===+===")))).toEqual(["=", "=", "=", "+", "=", "=", "="]);
  expect(shape(foldDiff(pattern("====+====")))).toEqual(["~1", "=", "=", "=", "+", "=", "=", "=", "~1"]);
});

test("改动间距 <= 2*context 时不折叠，间距多 1 行则折叠 1 行", () => {
  expect(shape(foldDiff(pattern("+" + rep("=", 5) + "+")))).toEqual(["+", "=", "=", "=", "=", "=", "+"]);
  expect(shape(foldDiff(pattern("+" + rep("=", 6) + "+")))).toEqual(["+", "=", "=", "=", "=", "=", "=", "+"]);
  expect(shape(foldDiff(pattern("+" + rep("=", 7) + "+")))).toEqual(["+", "=", "=", "=", "~1", "=", "=", "=", "+"]);
});

test("改动间距较大时中间折叠为一个 skip", () => {
  expect(shape(foldDiff(pattern("-" + rep("=", 20) + "+")))).toEqual(["-", "=", "=", "=", "~14", "=", "=", "=", "+"]);
});

test("紧邻的 del/add 视为同一处改动，改动位于首尾时那一侧没有 skip", () => {
  expect(shape(foldDiff(pattern("-+" + rep("=", 5))))).toEqual(["-", "+", "=", "=", "=", "~2"]);
  expect(shape(foldDiff(pattern(rep("=", 5) + "-+")))).toEqual(["~2", "=", "=", "=", "-", "+"]);
});

test("没有 equal 行时原样返回，不产生 skip", () => {
  const lines = pattern("-+-+");
  expect(foldDiff(lines)).toEqual(lines);
});

test("自定义 context", () => {
  const lines = pattern(rep("=", 5) + "+" + rep("=", 5));
  expect(shape(foldDiff(lines, 1))).toEqual(["~4", "=", "+", "=", "~4"]);
  expect(shape(foldDiff(lines, 0))).toEqual(["~5", "+", "~5"]);
  expect(shape(foldDiff(lines, 5))).toEqual(["=", "=", "=", "=", "=", "+", "=", "=", "=", "=", "="]);
});

test("折叠保留原 DiffLine 内容（含行号）与顺序", () => {
  const lines = diffLines(join(["a", "b", "c", "d", "e", "f", "g", "h", "i"]), join(["a", "b", "c", "d", "X", "f", "g", "h", "i"]));
  const items = foldDiff(lines, 1);
  expect(items).toStrictEqual([
    { type: "skip", count: 3 },
    { type: "equal", text: "d", oldLine: 4, newLine: 4 },
    { type: "del", text: "e", oldLine: 5 },
    { type: "add", text: "X", newLine: 5 },
    { type: "equal", text: "f", oldLine: 6, newLine: 6 },
    { type: "skip", count: 3 },
  ]);
});
