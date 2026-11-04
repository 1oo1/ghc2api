// 日志解析与聚合（logs.ts）的测试，覆盖的导出：
//   - parseLogText(text)：按行解析 NDJSON，返回 { entries, invalid }，计算 epoch（遇到 msg === "配置加载完成" 先加 1），
//     并在 fields.run_id 为非空字符串时设置 runId。
//   - groupLogs(entries)：有 runId 时按 run_id + request_id 分组（不看 epoch），否则按 epoch + request_id 分组；排序、合并正文块、汇总 method/path/status/耗时/最高级别。
//   - statusTone / levelTone / filterGroups。
// 分组相关测试直接构造 LogEntry，避免依赖 parseLogText 的实现。
import { expect, test } from "vitest";
import {
  filterGroups,
  groupLogs,
  levelTone,
  parseLogText,
  statusTone,
  type LogEntry,
  type LogGroup,
} from "./logs";

const BASE = Date.UTC(2026, 10, 3, 2, 0, 0); // 2026-11-03T02:00:00Z

/** 生成基准时间之后 offsetMs 毫秒的 ISO 时间字符串。 */
const timeAt = (offsetMs: number) => new Date(BASE + offsetMs).toISOString();

/** 序列化一条日志行。 */
const jsonLine = (fields: Record<string, unknown>) => JSON.stringify(fields);

type EntryOptions = { requestId?: number; epoch?: number; level?: string; omitRequestIdField?: boolean; runId?: string };

/** 直接构造 LogEntry：fields 含 time/level/msg/request_id 与额外字段。 */
function entry(
  line: number,
  offsetMs: number,
  msg: string,
  extra: Record<string, unknown> = {},
  options: EntryOptions = {},
): LogEntry {
  const { requestId, epoch = 0, level = "INFO", omitRequestIdField = false, runId } = options;
  const time = timeAt(offsetMs);
  const fields: Record<string, unknown> = { time, level, msg };
  if (requestId !== undefined && !omitRequestIdField) fields.request_id = requestId;
  if (runId !== undefined) fields.run_id = runId;
  Object.assign(fields, extra);
  const result: LogEntry = { line, time, ts: Date.parse(time), level, msg, epoch, fields };
  if (requestId !== undefined) result.requestId = requestId;
  if (runId !== undefined) result.runId = runId;
  return result;
}

/** 一个请求下的正文块记录。 */
function chunk(
  line: number,
  offsetMs: number,
  kind: "请求" | "响应",
  chunkIndex: unknown,
  body: unknown,
  extra: Record<string, unknown> = {},
  options: EntryOptions = { requestId: 1 },
): LogEntry {
  const base: Record<string, unknown> = { method: "POST", url: "https://api.example/v1/chat", chunk_index: chunkIndex, body };
  return entry(line, offsetMs, `上游 API ${kind}正文块`, { ...base, ...extra }, options);
}

const only = (groups: LogGroup[]): LogGroup => {
  expect(groups).toHaveLength(1);
  return groups[0];
};

// ---------------------------------------------------------------- parseLogText

test("parseLogText 空文本没有记录也没有无效行", () => {
  expect(parseLogText("")).toEqual({ entries: [], invalid: 0 });
});

test("parseLogText 解析字段并保留完整 fields", () => {
  const time = "2026-11-03T10:00:00.123456789+08:00";
  const raw = { time, level: "INFO", msg: "收到客户端请求", request_id: 12, method: "POST", nested: { a: 1 } };
  const { entries, invalid } = parseLogText(jsonLine(raw));
  expect(invalid).toBe(0);
  expect(entries).toHaveLength(1);
  const [e] = entries;
  expect(e.line).toBe(0);
  expect(e.time).toBe(time);
  expect(e.ts).toBe(Date.parse(time));
  expect(e.level).toBe("INFO");
  expect(e.msg).toBe("收到客户端请求");
  expect(e.requestId).toBe(12);
  expect(e.epoch).toBe(0);
  expect(e.fields).toEqual(raw);
});

test("parseLogText 空白行跳过且不计入 invalid，行号计入空行", () => {
  const a = jsonLine({ time: timeAt(0), level: "INFO", msg: "a" });
  const b = jsonLine({ time: timeAt(1), level: "INFO", msg: "b" });
  const { entries, invalid } = parseLogText(`\n${a}\n   \n\t\n${b}\n`);
  expect(invalid).toBe(0);
  expect(entries.map((e) => [e.msg, e.line])).toEqual([
    ["a", 1],
    ["b", 4],
  ]);
});

test("parseLogText 容忍 \\r\\n 换行", () => {
  const a = jsonLine({ time: timeAt(0), level: "INFO", msg: "a" });
  const b = jsonLine({ time: timeAt(1), level: "WARN", msg: "b" });
  const { entries, invalid } = parseLogText(`${a}\r\n${b}\r\n`);
  expect(invalid).toBe(0);
  expect(entries.map((e) => [e.msg, e.level, e.line])).toEqual([
    ["a", "INFO", 0],
    ["b", "WARN", 1],
  ]);
});

test("parseLogText 无效行计入 invalid 且不产生记录，行号仍递增", () => {
  const time = timeAt(0);
  const ok = (msg: string) => jsonLine({ time, level: "INFO", msg });
  const invalidLines = [
    "not json at all",
    "{broken",
    "[1,2,3]",
    "42",
    '"just a string"',
    "null",
    "true",
    jsonLine({ time, level: "INFO" }), // 缺 msg
    jsonLine({ time, level: "INFO", msg: 5 }), // msg 不是字符串
    jsonLine({ level: "INFO", msg: "no time" }), // 缺 time
    jsonLine({ time: 123, level: "INFO", msg: "time number" }), // time 不是字符串
    jsonLine({ time: "yesterday", level: "INFO", msg: "bad time" }), // Date.parse 为 NaN
  ];
  const text = [ok("first"), ...invalidLines, ok("last")].join("\n");
  const { entries, invalid } = parseLogText(text);
  expect(invalid).toBe(invalidLines.length);
  expect(entries.map((e) => [e.msg, e.line])).toEqual([
    ["first", 0],
    ["last", invalidLines.length + 1],
  ]);
});

test("parseLogText 的 level 缺失或不是字符串时取空串，其余原样保留", () => {
  const time = timeAt(0);
  const text = [
    jsonLine({ time, msg: "no level" }),
    jsonLine({ time, level: 3, msg: "numeric level" }),
    jsonLine({ time, level: "WARN", msg: "warn" }),
    jsonLine({ time, level: "warn", msg: "lowercase" }),
  ].join("\n");
  const { entries, invalid } = parseLogText(text);
  expect(invalid).toBe(0);
  expect(entries.map((e) => e.level)).toEqual(["", "", "WARN", "warn"]);
});

test("parseLogText 仅当 request_id 是 number 时设置 requestId", () => {
  const time = timeAt(0);
  const text = [
    jsonLine({ time, level: "INFO", msg: "number", request_id: 7 }),
    jsonLine({ time, level: "INFO", msg: "string", request_id: "7" }),
    jsonLine({ time, level: "INFO", msg: "null", request_id: null }),
    jsonLine({ time, level: "INFO", msg: "object", request_id: { id: 7 } }),
    jsonLine({ time, level: "INFO", msg: "absent" }),
  ].join("\n");
  const { entries } = parseLogText(text);
  expect(entries.map((e) => e.requestId)).toEqual([7, undefined, undefined, undefined, undefined]);
  // 非 number 的 request_id 仍保留在 fields 中
  expect(entries[1].fields.request_id).toBe("7");
});

test("parseLogText 的 epoch 从 0 开始，遇到“配置加载完成”先加 1（该记录属于新序号）", () => {
  const rec = (offset: number, msg: string) => jsonLine({ time: timeAt(offset), level: "INFO", msg });
  const text = [
    rec(0, "旧进程日志"),
    rec(1, "配置加载完成"),
    rec(2, "收到客户端请求"),
    rec(3, "配置加载完成"),
    rec(4, "收到客户端请求"),
  ].join("\n");
  const { entries } = parseLogText(text);
  expect(entries.map((e) => e.epoch)).toEqual([0, 1, 1, 2, 2]);
});

test("parseLogText 第一条就是“配置加载完成”时它的 epoch 为 1", () => {
  const text = [
    jsonLine({ time: timeAt(0), level: "INFO", msg: "配置加载完成" }),
    jsonLine({ time: timeAt(1), level: "INFO", msg: "other" }),
  ].join("\n");
  const { entries } = parseLogText(text);
  expect(entries.map((e) => e.epoch)).toEqual([1, 1]);
});

test("parseLogText 的无效行不影响 epoch，无效的“配置加载完成”不产生新序号", () => {
  const text = [
    jsonLine({ time: timeAt(0), level: "INFO", msg: "a" }),
    jsonLine({ time: "bad", level: "INFO", msg: "配置加载完成" }), // time 无效 -> 无效行
    jsonLine({ time: timeAt(1), level: "INFO", msg: "b" }),
  ].join("\n");
  const { entries, invalid } = parseLogText(text);
  expect(invalid).toBe(1);
  expect(entries.map((e) => e.epoch)).toEqual([0, 0]);
});

test("parseLogText 仅当 fields.run_id 是非空字符串时设置 runId", () => {
  const time = timeAt(0);
  const text = [
    jsonLine({ time, level: "INFO", msg: "ok", run_id: "0123456789abcdef" }),
    jsonLine({ time, level: "INFO", msg: "empty", run_id: "" }),
    jsonLine({ time, level: "INFO", msg: "number", run_id: 5 }),
    jsonLine({ time, level: "INFO", msg: "null", run_id: null }),
    jsonLine({ time, level: "INFO", msg: "object", run_id: { a: 1 } }),
    jsonLine({ time, level: "INFO", msg: "absent" }),
  ].join("\n");
  const { entries, invalid } = parseLogText(text);
  expect(invalid).toBe(0);
  expect(entries.map((e) => e.runId)).toEqual(["0123456789abcdef", undefined, undefined, undefined, undefined, undefined]);
  expect(entries[2].fields.run_id).toBe(5); // 非字符串的 run_id 仍保留在 fields 中
});

test("parseLogText 带 run_id 时 epoch 规则不变（兼容旧日志）", () => {
  const rec = (offset: number, msg: string) =>
    jsonLine({ time: timeAt(offset), level: "INFO", msg, run_id: "aaaaaaaaaaaaaaaa" });
  const { entries } = parseLogText([rec(0, "a"), rec(1, "配置加载完成"), rec(2, "b")].join("\n"));
  expect(entries.map((e) => [e.epoch, e.runId])).toEqual([
    [0, "aaaaaaaaaaaaaaaa"],
    [1, "aaaaaaaaaaaaaaaa"],
    [1, "aaaaaaaaaaaaaaaa"],
  ]);
});

// ---------------------------------------------------------------- groupLogs: 分组与排序

test("groupLogs 空输入返回空数组", () => {
  expect(groupLogs([])).toEqual([]);
});

test("groupLogs 按 epoch + request_id 分组，key 为 `${epoch}:${requestId}`", () => {
  const groups = groupLogs([
    entry(0, 0, "a", {}, { requestId: 1, epoch: 0 }),
    entry(1, 1, "b", {}, { requestId: 1, epoch: 0 }),
    entry(2, 2, "c", {}, { requestId: 2, epoch: 0 }),
  ]);
  expect(groups.map((g) => [g.key, g.requestId, g.epoch, g.items.length]).sort()).toEqual([
    ["0:1", 1, 0, 2],
    ["0:2", 2, 0, 1],
  ]);
});

test("groupLogs 不同 epoch 的相同 request_id 分成不同的组", () => {
  const groups = groupLogs([
    entry(0, 0, "a", {}, { requestId: 1, epoch: 0 }),
    entry(1, 10, "b", {}, { requestId: 1, epoch: 1 }),
  ]);
  expect(groups.map((g) => g.key).sort()).toEqual(["0:1", "1:1"]);
  for (const g of groups) expect(g.items).toHaveLength(1);
});

test("groupLogs 没有 request_id 的记录各自成组，key 为 line:N", () => {
  const groups = groupLogs([entry(3, 0, "x"), entry(5, 1, "y"), entry(8, 2, "z")]);
  expect(groups.map((g) => g.key).sort()).toEqual(["line:3", "line:5", "line:8"]);
  for (const g of groups) {
    expect(g.requestId).toBeUndefined();
    expect(g.items).toHaveLength(1);
  }
});

test("groupLogs 按 start 降序排列组（最新在前）", () => {
  const groups = groupLogs([
    entry(0, 100, "old", {}, { requestId: 1 }),
    entry(1, 300, "newest", {}, { requestId: 3 }),
    entry(2, 200, "mid", {}, { requestId: 2 }),
  ]);
  expect(groups.map((g) => g.requestId)).toEqual([3, 2, 1]);
});

test("groupLogs 组的 start 取组内最早记录而非首次出现的记录", () => {
  // 组 1 的记录跨度 [100, 500]，组 2 单条 300：按 start 降序应为 组2(300) 在前、组1(100) 在后
  const groups = groupLogs([
    entry(0, 500, "late", {}, { requestId: 1 }),
    entry(1, 100, "early", {}, { requestId: 1 }),
    entry(2, 300, "other", {}, { requestId: 2 }),
  ]);
  expect(groups.map((g) => [g.requestId, g.start, g.end])).toEqual([
    [2, BASE + 300, BASE + 300],
    [1, BASE + 100, BASE + 500],
  ]);
});

test("groupLogs 的 start 相同时按组内最小 line 降序", () => {
  const groups = groupLogs([
    entry(2, 100, "a", {}, { requestId: 1 }),
    entry(9, 100, "b", {}, { requestId: 2 }),
    entry(5, 100, "c", {}, { requestId: 3 }),
    entry(7, 100, "c2", {}, { requestId: 3 }), // 组 3 的最小 line 是 5
  ]);
  expect(groups.map((g) => g.requestId)).toEqual([2, 3, 1]);
});

test("groupLogs 组内记录按 (ts, line) 升序，与输入顺序无关", () => {
  const groups = groupLogs([
    entry(4, 200, "c", {}, { requestId: 1 }),
    entry(2, 100, "b2", {}, { requestId: 1 }),
    entry(1, 100, "b1", {}, { requestId: 1 }),
    entry(0, 300, "d", {}, { requestId: 1 }),
  ]);
  const g = only(groups);
  expect(g.items.map((i) => i.entry.msg)).toEqual(["b1", "b2", "c", "d"]);
  expect(g.start).toBe(BASE + 100);
  expect(g.end).toBe(BASE + 300);
});

test("groupLogs 普通记录的 item 没有 body、chunks、incomplete", () => {
  const g = only(groupLogs([entry(0, 0, "收到客户端请求", {}, { requestId: 1 })]));
  expect(g.items[0].entry.msg).toBe("收到客户端请求");
  expect(g.items[0].body).toBeUndefined();
  expect(g.items[0].chunks).toBeUndefined();
  expect(g.items[0].incomplete).toBeUndefined();
});

// ---------------------------------------------------------------- groupLogs: run_id 分组

test("groupLogs 有 runId 时分组键为 run:${runId}:${requestId}，并设置 group.runId", () => {
  const g = only(
    groupLogs([
      entry(0, 0, "a", {}, { requestId: 12, epoch: 3, runId: "aaaaaaaaaaaaaaaa" }),
      entry(1, 1, "b", {}, { requestId: 12, epoch: 3, runId: "aaaaaaaaaaaaaaaa" }),
    ]),
  );
  expect(g.key).toBe("run:aaaaaaaaaaaaaaaa:12");
  expect(g.runId).toBe("aaaaaaaaaaaaaaaa");
  expect(g.requestId).toBe(12);
  expect(g.items).toHaveLength(2);
});

test("groupLogs 没有 runId 时 group.runId 缺失，键保持 `${epoch}:${requestId}`", () => {
  const g = only(groupLogs([entry(0, 0, "a", {}, { requestId: 12, epoch: 3 })]));
  expect(g.key).toBe("3:12");
  expect(g.runId).toBeUndefined();
});

test("groupLogs 同一 run_id 下即使 epoch 不同（中间出现“配置加载完成”）仍属同一组", () => {
  const g = only(
    groupLogs([
      entry(0, 0, "收到客户端请求", {}, { requestId: 1, epoch: 0, runId: "aaaaaaaaaaaaaaaa" }),
      entry(1, 1, "配置加载完成", {}, { requestId: 1, epoch: 1, runId: "aaaaaaaaaaaaaaaa" }),
      entry(2, 2, "客户端请求处理完成", { status: 200 }, { requestId: 1, epoch: 1, runId: "aaaaaaaaaaaaaaaa" }),
    ]),
  );
  expect(g.key).toBe("run:aaaaaaaaaaaaaaaa:1");
  expect(g.items.map((i) => i.entry.msg)).toEqual(["收到客户端请求", "配置加载完成", "客户端请求处理完成"]);
  expect(g.status).toBe(200);
});

test("groupLogs 不同 run_id 即使 epoch 与 request_id 都相同也分开", () => {
  const groups = groupLogs([
    entry(0, 0, "a", {}, { requestId: 1, epoch: 0, runId: "aaaaaaaaaaaaaaaa" }),
    entry(1, 1, "b", {}, { requestId: 1, epoch: 0, runId: "bbbbbbbbbbbbbbbb" }),
  ]);
  expect(groups.map((g) => g.key).sort()).toEqual(["run:aaaaaaaaaaaaaaaa:1", "run:bbbbbbbbbbbbbbbb:1"]);
  for (const g of groups) expect(g.items).toHaveLength(1);
});

test("groupLogs 带 run_id 与不带 run_id 的同 request_id 记录分开（旧日志不与新日志合并）", () => {
  const groups = groupLogs([
    entry(0, 0, "old", {}, { requestId: 1, epoch: 0 }),
    entry(1, 1, "new", {}, { requestId: 1, epoch: 0, runId: "aaaaaaaaaaaaaaaa" }),
  ]);
  expect(groups.map((g) => g.key).sort()).toEqual(["0:1", "run:aaaaaaaaaaaaaaaa:1"]);
});

test("groupLogs 无 request_id 的记录即使带 run_id 也各自成组，key 为 line:N", () => {
  const groups = groupLogs([
    entry(3, 0, "x", {}, { runId: "aaaaaaaaaaaaaaaa" }),
    entry(5, 1, "y", {}, { runId: "aaaaaaaaaaaaaaaa" }),
  ]);
  expect(groups.map((g) => g.key).sort()).toEqual(["line:3", "line:5"]);
  for (const g of groups) expect(g.requestId).toBeUndefined();
});

test("groupLogs 按 run_id 分组时，组仍按 start 降序、组内按 (ts, line) 升序，正文块照常合并", () => {
  const groups = groupLogs([
    chunk(3, 12, "响应", 2, "B", {}, { requestId: 1, runId: "aaaaaaaaaaaaaaaa", epoch: 1 }),
    chunk(0, 10, "响应", 1, "A", {}, { requestId: 1, runId: "aaaaaaaaaaaaaaaa", epoch: 0 }),
    entry(4, 500, "later", {}, { requestId: 1, runId: "bbbbbbbbbbbbbbbb", epoch: 0 }),
  ]);
  expect(groups.map((g) => g.key)).toEqual(["run:bbbbbbbbbbbbbbbb:1", "run:aaaaaaaaaaaaaaaa:1"]);
  expect(groups[1].items).toHaveLength(1);
  expect(groups[1].items[0].body).toBe("AB");
  expect(groups[1].items[0].chunks).toBe(2);
});

// ---------------------------------------------------------------- groupLogs: 汇总

test("groupLogs 汇总 method、path、status、durationMs", () => {
  const g = only(
    groupLogs([
      entry(0, 0, "收到客户端请求", { method: "POST", path: "/v1/chat/completions" }, { requestId: 1 }),
      entry(1, 5, "其他", { method: "GET", path: "/ignored", status: 500 }, { requestId: 1 }),
      entry(2, 10, "客户端请求处理完成", { status: 200, duration_ms: 1234 }, { requestId: 1 }),
    ]),
  );
  expect(g.method).toBe("POST");
  expect(g.path).toBe("/v1/chat/completions");
  expect(g.status).toBe(200);
  expect(g.durationMs).toBe(1234);
});

test("groupLogs 没有请求/完成记录时汇总字段缺失", () => {
  const g = only(groupLogs([entry(0, 0, "其他", { method: "GET", path: "/x", status: 200 }, { requestId: 1 })]));
  expect(g.method).toBeUndefined();
  expect(g.path).toBeUndefined();
  expect(g.status).toBeUndefined();
  expect(g.durationMs).toBeUndefined();
});

test("groupLogs 的 status、duration_ms 不是 number 时忽略", () => {
  const g = only(
    groupLogs([entry(0, 0, "客户端请求处理完成", { status: "200", duration_ms: "12" }, { requestId: 1 })]),
  );
  expect(g.status).toBeUndefined();
  expect(g.durationMs).toBeUndefined();
});

test("groupLogs 的 level 取组内最高级别（ERROR > WARN > INFO > DEBUG），原样字符串", () => {
  const levelOf = (...levels: string[]) =>
    only(groupLogs(levels.map((level, i) => entry(i, i, "m", {}, { requestId: 1, level })))).level;
  expect(levelOf("INFO", "WARN", "DEBUG")).toBe("WARN");
  expect(levelOf("DEBUG", "ERROR", "INFO", "WARN")).toBe("ERROR");
  expect(levelOf("DEBUG", "INFO")).toBe("INFO");
  expect(levelOf("DEBUG", "DEBUG")).toBe("DEBUG");
  expect(levelOf("WARN")).toBe("WARN");
});

test("groupLogs 不认识的级别视为最低", () => {
  const levelOf = (...levels: string[]) =>
    only(groupLogs(levels.map((level, i) => entry(i, i, "m", {}, { requestId: 1, level })))).level;
  expect(levelOf("TRACE", "DEBUG")).toBe("DEBUG");
  expect(levelOf("", "INFO")).toBe("INFO");
  expect(levelOf("TRACE")).toBe("TRACE");
});

// ---------------------------------------------------------------- groupLogs: 正文块合并

test("groupLogs 把同一请求的多个正文块按 chunk_index 拼接成一个合并项", () => {
  const g = only(
    groupLogs([
      entry(0, 0, "收到客户端请求", {}, { requestId: 1 }),
      chunk(1, 10, "响应", 1, "AAA", { status: 200 }),
      chunk(2, 11, "响应", 2, "BBB", { status: 200 }),
      chunk(3, 12, "响应", 3, "CCC", { status: 200 }),
      entry(4, 20, "客户端请求处理完成", { status: 200 }, { requestId: 1 }),
    ]),
  );
  expect(g.items.map((i) => i.entry.msg)).toEqual([
    "收到客户端请求",
    "上游 API 响应正文块",
    "客户端请求处理完成",
  ]);
  const merged = g.items[1];
  expect(merged.body).toBe("AAABBBCCC");
  expect(merged.chunks).toBe(3);
  expect(merged.incomplete).toBe(false);
  expect(merged.entry.line).toBe(1); // 合并项的 entry 是第一块的记录
});

test("groupLogs 与输入数组顺序无关：按 (ts, line) 排序后再合并", () => {
  const g = only(
    groupLogs([
      chunk(2, 12, "响应", 3, "C"),
      chunk(0, 10, "响应", 1, "A"),
      chunk(1, 11, "响应", 2, "B"),
    ]),
  );
  expect(g.items).toHaveLength(1);
  expect(g.items[0].body).toBe("ABC");
  expect(g.items[0].chunks).toBe(3);
  expect(g.items[0].entry.line).toBe(0);
});

test("groupLogs 的合并项位于第一块所在位置，夹在中间的其他记录保持原位", () => {
  const g = only(
    groupLogs([
      entry(0, 0, "开始", {}, { requestId: 1 }),
      chunk(1, 10, "请求", 1, "A"),
      entry(2, 11, "中间", {}, { requestId: 1 }),
      chunk(3, 12, "请求", 2, "B"),
      entry(4, 13, "结束", {}, { requestId: 1 }),
    ]),
  );
  expect(g.items.map((i) => i.entry.msg)).toEqual(["开始", "上游 API 请求正文块", "中间", "结束"]);
  expect(g.items[1].body).toBe("AB");
});

test("groupLogs 的请求块与响应块分开合并", () => {
  const g = only(
    groupLogs([
      chunk(0, 10, "请求", 1, "REQ"),
      chunk(1, 11, "响应", 1, "RESP", { status: 200 }),
    ]),
  );
  expect(g.items.map((i) => [i.entry.msg, i.body, i.chunks])).toEqual([
    ["上游 API 请求正文块", "REQ", 1],
    ["上游 API 响应正文块", "RESP", 1],
  ]);
});

test("groupLogs 块键含 method、url、status、account：任一不同都分开合并", () => {
  const variants: Array<Record<string, unknown>> = [
    { url: "https://api.example/v1/other" },
    { method: "GET" },
    { status: 500 },
    { account: 3 },
  ];
  for (const variant of variants) {
    const g = only(
      groupLogs([
        chunk(0, 10, "响应", 1, "A", { status: 200, account: 0 }),
        chunk(1, 11, "响应", 2, "B", { status: 200, account: 0, ...variant }),
      ]),
    );
    expect(g.items, JSON.stringify(variant)).toHaveLength(2);
    expect(g.items.map((i) => i.body)).toEqual(["A", "B"]);
  }
});

test("groupLogs 块键中缺失的字段视为空，两块都缺失时仍合并", () => {
  const bare = (line: number, offset: number, index: number, body: string) =>
    entry(line, offset, "上游 API 响应正文块", { chunk_index: index, body }, { requestId: 1 });
  const g = only(groupLogs([bare(0, 10, 1, "A"), bare(1, 11, 2, "B")]));
  expect(g.items).toHaveLength(1);
  expect(g.items[0].body).toBe("AB");
});

test("groupLogs 同一 URL 重试时 chunk_index 重新从 1 开始，产生两个合并项", () => {
  const g = only(
    groupLogs([
      chunk(0, 10, "响应", 1, "A1", { status: 200 }),
      chunk(1, 11, "响应", 2, "A2", { status: 200 }),
      entry(2, 12, "重试", {}, { requestId: 1 }),
      chunk(3, 13, "响应", 1, "B1", { status: 200 }),
      chunk(4, 14, "响应", 2, "B2", { status: 200 }),
    ]),
  );
  expect(g.items.map((i) => [i.entry.msg, i.body, i.chunks, i.incomplete])).toEqual([
    ["上游 API 响应正文块", "A1A2", 2, false],
    ["重试", undefined, undefined, undefined],
    ["上游 API 响应正文块", "B1B2", 2, false],
  ]);
});

test("groupLogs 交错的两个不同 URL 的正文块各自合并，合并项位于各自第一块位置", () => {
  const g = only(
    groupLogs([
      chunk(0, 10, "响应", 1, "A1", { url: "https://a.example/x" }),
      chunk(1, 11, "响应", 1, "B1", { url: "https://b.example/x" }),
      chunk(2, 12, "响应", 2, "A2", { url: "https://a.example/x" }),
      chunk(3, 13, "响应", 2, "B2", { url: "https://b.example/x" }),
    ]),
  );
  expect(g.items.map((i) => [i.entry.line, i.body, i.chunks])).toEqual([
    [0, "A1A2", 2],
    [1, "B1B2", 2],
  ]);
});

test("groupLogs 的 chunk_index 不大于已收最大值时开启新合并项（按规则不会重排乱序块）", () => {
  // 按 (ts, line) 顺序遍历：块 2 先开启合并项，随后的块 1 不大于最大值 2，开启新合并项。
  const g = only(
    groupLogs([
      chunk(0, 10, "响应", 2, "B"),
      chunk(1, 11, "响应", 1, "A"),
    ]),
  );
  expect(g.items.map((i) => [i.body, i.chunks, i.incomplete])).toEqual([
    ["B", 1, true],
    ["A", 1, false],
  ]);
});

test("groupLogs 的 incomplete：块序号不是 1..n 连续时为 true", () => {
  const merged = (indexes: number[]) => {
    const g = only(groupLogs(indexes.map((idx, i) => chunk(i, 10 + i, "响应", idx, `p${idx}`))));
    expect(g.items).toHaveLength(1);
    return g.items[0];
  };
  expect(merged([1]).incomplete).toBe(false);
  expect(merged([1, 2, 3]).incomplete).toBe(false);
  expect(merged([2, 3]).incomplete).toBe(true); // 缺开头（日志被截断）
  expect(merged([1, 3]).incomplete).toBe(true); // 中间缺块
  expect(merged([1, 2, 4]).incomplete).toBe(true);
  expect(merged([2, 3]).chunks).toBe(2);
  expect(merged([2, 3]).body).toBe("p2p3");
});

test("groupLogs 的正文块缺少合法的 chunk_index 或 body 时按普通记录处理，不带 body", () => {
  const g = only(
    groupLogs([
      chunk(0, 10, "响应", "1", "string index"),
      chunk(1, 11, "响应", 1, 42),
      chunk(2, 12, "响应", undefined, "no index"),
    ]),
  );
  expect(g.items).toHaveLength(3);
  for (const item of g.items) {
    expect(item.body).toBeUndefined();
    expect(item.chunks).toBeUndefined();
    expect(item.incomplete).toBeUndefined();
  }
});

test("groupLogs 不会把不同 epoch 的正文块并到一起", () => {
  const groups = groupLogs([
    chunk(0, 10, "响应", 1, "A", {}, { requestId: 1, epoch: 0 }),
    chunk(1, 11, "响应", 2, "B", {}, { requestId: 1, epoch: 1 }),
  ]);
  expect(groups).toHaveLength(2);
  for (const g of groups) expect(g.items).toHaveLength(1);
});

// ---------------------------------------------------------------- groupLogs: 未切块的正文

test("groupLogs 的“上游 API 响应正文”取 fields.body", () => {
  const g = only(groupLogs([entry(0, 0, "上游 API 响应正文", { body: '{"ok":true}' }, { requestId: 1 })]));
  expect(g.items[0].body).toBe('{"ok":true}');
  expect(g.items[0].chunks).toBeUndefined();
});

test("groupLogs 的“上游 API 响应正文”body 不是字符串时无 body", () => {
  const g = only(groupLogs([entry(0, 0, "上游 API 响应正文", { body: { a: 1 } }, { requestId: 1 })]));
  expect(g.items[0].body).toBeUndefined();
});

test("groupLogs 的“上游 API 请求详情”取 fields.request.body（非空字符串）", () => {
  const g = only(
    groupLogs([
      entry(0, 0, "上游 API 请求详情", { request: { method: "POST", body: '{"model":"x"}' } }, { requestId: 1 }),
      entry(1, 1, "上游 API 请求详情", { request: { method: "POST", body: "" } }, { requestId: 1 }),
      entry(2, 2, "上游 API 请求详情", { request: { method: "POST" } }, { requestId: 1 }),
      entry(3, 3, "上游 API 请求详情", { request: { body: 5 } }, { requestId: 1 }),
      entry(4, 4, "上游 API 请求详情", { request: "not object" }, { requestId: 1 }),
      entry(5, 5, "上游 API 请求详情", {}, { requestId: 1 }),
    ]),
  );
  expect(g.items.map((i) => i.body)).toEqual(['{"model":"x"}', undefined, undefined, undefined, undefined, undefined]);
});

test("groupLogs 的其他记录即使带有 body 字段也没有 body", () => {
  const g = only(groupLogs([entry(0, 0, "别的消息", { body: "x", request: { body: "y" } }, { requestId: 1 })]));
  expect(g.items[0].body).toBeUndefined();
});

// ---------------------------------------------------------------- statusTone / levelTone

test("statusTone 按状态码区间返回色调", () => {
  expect(statusTone(200)).toBe("success");
  expect(statusTone(204)).toBe("success");
  expect(statusTone(299)).toBe("success");
  expect(statusTone(300)).toBe("redirect");
  expect(statusTone(302)).toBe("redirect");
  expect(statusTone(399)).toBe("redirect");
  expect(statusTone(400)).toBe("error");
  expect(statusTone(404)).toBe("error");
  expect(statusTone(499)).toBe("error");
  expect(statusTone(500)).toBe("error");
  expect(statusTone(599)).toBe("error");
});

test("statusTone 其他输入（含 undefined、非整数、非 number）为 none", () => {
  for (const value of [undefined, null, 100, 199, 600, 0, -200, 200.5, NaN, Infinity, "200", {}, []]) {
    expect(statusTone(value), String(value)).toBe("none");
  }
});

test("levelTone 大小写不敏感，未知级别为 info", () => {
  expect(levelTone("ERROR")).toBe("error");
  expect(levelTone("error")).toBe("error");
  expect(levelTone("WARN")).toBe("warn");
  expect(levelTone("Warn")).toBe("warn");
  expect(levelTone("DEBUG")).toBe("debug");
  expect(levelTone("debug")).toBe("debug");
  expect(levelTone("INFO")).toBe("info");
  expect(levelTone("info")).toBe("info");
  expect(levelTone("")).toBe("info");
  expect(levelTone("TRACE")).toBe("info");
});

// ---------------------------------------------------------------- filterGroups

/**
 * 构造用于筛选的组：time 固定为 2026-11-03T02:00:00.000Z（不含数字 7），
 * 记录的 fields 不含 request_id，使 request_id 匹配只能来自 String(requestId) === query。
 */
function filterFixture(): LogGroup[] {
  const at = (line: number, msg: string, extra: Record<string, unknown>, requestId: number) =>
    entry(line, 0, msg, extra, { requestId, omitRequestIdField: true });
  return groupLogs([
    at(0, "收到客户端请求", { method: "POST", path: "/v1/chat/completions" }, 7),
    at(1, "上游 API 响应正文", { body: "Hello World from upstream" }, 7),
    at(2, "收到客户端请求", { method: "GET", path: "/v1/models" }, 70),
    at(3, "账号冷却", { account_name: "AliceSmith" }, 5),
    at(4, "别的消息", { note: "ZZZ" }, 8),
  ]);
}

const requestIdsOf = (groups: LogGroup[]) => groups.map((g) => g.requestId).sort((a, b) => (a ?? 0) - (b ?? 0));

test("filterGroups 空查询或纯空白查询原样返回同一个数组", () => {
  const groups = filterFixture();
  expect(filterGroups(groups, "")).toBe(groups);
  expect(filterGroups(groups, "   ")).toBe(groups);
  expect(filterGroups(groups, "\t\n")).toBe(groups);
});

test("filterGroups 按 request_id 精确匹配", () => {
  const groups = filterFixture();
  expect(requestIdsOf(filterGroups(groups, "7"))).toEqual([7]); // 不匹配 70
  expect(requestIdsOf(filterGroups(groups, "70"))).toEqual([70]);
});

test("filterGroups 去除查询首尾空白", () => {
  expect(requestIdsOf(filterGroups(filterFixture(), "  7  "))).toEqual([7]);
});

test("filterGroups 按 method 或 path 包含匹配（大小写不敏感）", () => {
  const groups = filterFixture();
  expect(requestIdsOf(filterGroups(groups, "post"))).toEqual([7]);
  expect(requestIdsOf(filterGroups(groups, "/V1/MODELS"))).toEqual([70]);
  expect(requestIdsOf(filterGroups(groups, "/v1/"))).toEqual([7, 70]);
});

test("filterGroups 按 item 的 msg 匹配", () => {
  const groups = filterFixture();
  expect(requestIdsOf(filterGroups(groups, "账号冷却"))).toEqual([5]);
});

test("filterGroups 按 item 的 body 匹配（大小写不敏感）", () => {
  const groups = filterFixture();
  expect(requestIdsOf(filterGroups(groups, "hello world"))).toEqual([7]);
});

test("filterGroups 按 JSON.stringify(entry.fields) 匹配（大小写不敏感）", () => {
  const groups = filterFixture();
  expect(requestIdsOf(filterGroups(groups, "alicesmith"))).toEqual([5]);
  expect(requestIdsOf(filterGroups(groups, "zzz"))).toEqual([8]);
});

test("filterGroups 匹配合并后的正文块 body", () => {
  const groups = groupLogs([
    chunk(0, 0, "响应", 1, "first-half-", {}, { requestId: 1 }),
    chunk(1, 1, "响应", 2, "second-half", {}, { requestId: 1 }),
    entry(2, 2, "无关", {}, { requestId: 2 }),
  ]);
  // 跨块的子串只在合并后的 body 中出现
  expect(requestIdsOf(filterGroups(groups, "half-second"))).toEqual([1]);
});

test("filterGroups 无匹配返回空数组，匹配结果保持原顺序", () => {
  const groups = filterFixture();
  expect(filterGroups(groups, "no-such-thing")).toEqual([]);
  const kept = filterGroups(groups, "/v1/");
  expect(kept).toEqual(groups.filter((g) => kept.includes(g)));
  // 返回的是原组对象，而不是拷贝
  for (const g of kept) expect(groups).toContain(g);
});
