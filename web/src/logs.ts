// 日志解析与聚合：把服务端返回的原始 NDJSON 文本解析、按请求分组、合并上游正文块并支持筛选。
// 全部为纯函数，在浏览器中执行；服务端只负责读取原始日志，见 docs/management.md。

export type LogEntry = {
  /** 在文本中的行号（从 0 开始，计入空行与无效行），用于同一时间戳下的稳定排序。 */
  line: number;
  /** 原始 time 字符串。 */
  time: string;
  /** Date.parse(time) 的毫秒值。 */
  ts: number;
  /** 原样保留，如 "INFO"。 */
  level: string;
  msg: string;
  /** 仅当 request_id 是 number 时存在。 */
  requestId?: number;
  /** 进程运行标识：仅当 fields.run_id 是非空字符串时存在（旧日志没有）。 */
  runId?: string;
  /** 进程运行序号：从 0 开始，遇到 msg === "配置加载完成" 的记录时先加 1（该记录属于新的序号）。 */
  epoch: number;
  /** 该行解析出的完整 JSON 对象（含 time/level/msg 等全部字段）。 */
  fields: Record<string, unknown>;
};

/** 每次进程启动后输出的第一条日志；request_id 每次启动都从 1 重新计数，以此区分不同进程的记录。 */
const STARTUP_MSG = "配置加载完成";
const REQUEST_START_MSG = "收到客户端请求";
const REQUEST_DONE_MSG = "客户端请求处理完成";
const REQUEST_CHUNK_MSG = "上游 API 请求正文块";
const RESPONSE_CHUNK_MSG = "上游 API 响应正文块";
const RESPONSE_BODY_MSG = "上游 API 响应正文";
const REQUEST_DETAIL_MSG = "上游 API 请求详情";

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

/**
 * 按行解析 NDJSON（容忍 \r\n）。空白行跳过且不计入 invalid；
 * 不是 JSON 对象、msg 或 time 不是字符串、time 无法解析的行计入 invalid 且不产生记录。
 */
export function parseLogText(text: string): {
  entries: LogEntry[];
  invalid: number;
} {
  const entries: LogEntry[] = [];
  let invalid = 0;
  let epoch = 0;
  const lines = text.split("\n");
  for (let line = 0; line < lines.length; line++) {
    const raw = lines[line];
    if (raw.trim() === "") continue;
    let fields: unknown;
    try {
      fields = JSON.parse(raw);
    } catch {
      invalid++;
      continue;
    }
    if (!isRecord(fields)) {
      invalid++;
      continue;
    }
    const { time, msg, level, request_id: requestId } = fields;
    const ts = typeof time === "string" ? Date.parse(time) : NaN;
    if (typeof msg !== "string" || typeof time !== "string" || Number.isNaN(ts)) {
      invalid++;
      continue;
    }
    if (msg === STARTUP_MSG) epoch++;
    const entry: LogEntry = {
      line,
      time,
      ts,
      level: typeof level === "string" ? level : "",
      msg,
      epoch,
      fields,
    };
    if (typeof requestId === "number") entry.requestId = requestId;
    if (typeof fields.run_id === "string" && fields.run_id !== "")
      entry.runId = fields.run_id;
    entries.push(entry);
  }
  return { entries, invalid };
}

export type LogItem = {
  /** 普通记录即其本身；合并的正文块为第一块（chunk_index 最小者）的记录。 */
  entry: LogEntry;
  /** 需要以代码块显示的正文；没有正文时不存在。 */
  body?: string;
  /** 合并的块数，仅正文块合并项存在。 */
  chunks?: number;
  /** 正文块的 chunk_index 不是从 1 开始的连续整数（例如日志被截断），仅正文块合并项存在。 */
  incomplete?: boolean;
};

export type LogGroup = {
  /**
   * 有 request_id 且有 run_id：`run:${runId}:${requestId}`；有 request_id 无 run_id（旧日志）：`${epoch}:${requestId}`；
   * 没有 request_id：`line:${line}`（每条记录单独成组）。
   */
  key: string;
  requestId?: number;
  runId?: string;
  epoch: number;
  /** 组内最早、最晚记录的 ts。 */
  start: number;
  end: number;
  /** 按 (ts, line) 升序。 */
  items: LogItem[];
  /** 取自组内 msg === "收到客户端请求" 的记录的 method、path。 */
  method?: string;
  path?: string;
  /** 取自组内 msg === "客户端请求处理完成" 的记录的 status、duration_ms（须为 number）。 */
  status?: number;
  durationMs?: number;
  /** 组内最高级别（ERROR > WARN > INFO > DEBUG，不认识的级别视为最低），取原样字符串。 */
  level: string;
};

const byTimeThenLine = (a: LogEntry, b: LogEntry) =>
  a.ts - b.ts || a.line - b.line;

/** 级别高低；不认识的级别为 -1，低于 DEBUG。 */
function levelRank(level: string): number {
  switch (level.toUpperCase()) {
    case "ERROR":
      return 3;
    case "WARN":
      return 2;
    case "INFO":
      return 1;
    case "DEBUG":
      return 0;
    default:
      return -1;
  }
}

/** 正在合并的正文块：已收的块序号（升序）与内容。 */
type ChunkMerge = { item: LogItem; indexes: number[]; parts: string[] };

/** 把一组已排序的记录转换为 items，同一次上游调用的正文块合并为一项。 */
function buildItems(entries: LogEntry[]): LogItem[] {
  const items: LogItem[] = [];
  /** 块键对应的、尚未结束的合并项。 */
  const open = new Map<string, ChunkMerge>();
  const merges: ChunkMerge[] = [];
  for (const entry of entries) {
    const { fields } = entry;
    if (entry.msg === REQUEST_CHUNK_MSG || entry.msg === RESPONSE_CHUNK_MSG) {
      const index = fields.chunk_index;
      const body = fields.body;
      if (typeof index === "number" && typeof body === "string") {
        const key = [
          entry.msg,
          fields.method,
          fields.url,
          fields.status,
          fields.account,
        ]
          .map((v) => String(v ?? ""))
          .join("\u0000");
        let merge = open.get(key);
        if (merge && index > merge.indexes[merge.indexes.length - 1]) {
          merge.indexes.push(index);
          merge.parts.push(body);
        } else {
          merge = { item: { entry }, indexes: [index], parts: [body] };
          open.set(key, merge);
          merges.push(merge);
          items.push(merge.item);
        }
        continue;
      }
    } else if (entry.msg === RESPONSE_BODY_MSG) {
      items.push(
        typeof fields.body === "string"
          ? { entry, body: fields.body }
          : { entry },
      );
      continue;
    } else if (entry.msg === REQUEST_DETAIL_MSG) {
      const body = isRecord(fields.request) ? fields.request.body : undefined;
      items.push(
        typeof body === "string" && body !== ""
          ? { entry, body }
          : { entry },
      );
      continue;
    }
    items.push({ entry });
  }
  for (const { item, indexes, parts } of merges) {
    item.body = parts.join("");
    item.chunks = parts.length;
    item.incomplete = indexes.some((index, i) => index !== i + 1);
  }
  return items;
}

/** 汇总一组记录；entries 已按 (ts, line) 升序且非空。 */
function buildGroup(key: string, entries: LogEntry[]): LogGroup {
  const first = entries[0];
  const group: LogGroup = {
    key,
    requestId: first.requestId,
    runId: first.runId,
    epoch: first.epoch,
    start: first.ts,
    end: entries[entries.length - 1].ts,
    items: buildItems(entries),
    level: first.level,
  };
  for (const { msg, level, fields } of entries) {
    if (levelRank(level) > levelRank(group.level)) group.level = level;
    if (msg === REQUEST_START_MSG) {
      if (typeof fields.method === "string") group.method = fields.method;
      if (typeof fields.path === "string") group.path = fields.path;
    } else if (msg === REQUEST_DONE_MSG) {
      if (typeof fields.status === "number") group.status = fields.status;
      if (typeof fields.duration_ms === "number")
        group.durationMs = fields.duration_ms;
    }
  }
  return group;
}

/**
 * 按 run_id + request_id 分组（旧日志没有 run_id 时按 epoch + request_id；没有 request_id 的记录各自成组），组按 start 降序（最新在前），
 * start 相同按组内最小行号降序。组内记录按 (ts, line) 升序，上游正文块合并为一项。
 */
export function groupLogs(entries: LogEntry[]): LogGroup[] {
  const buckets = new Map<string, LogEntry[]>();
  for (const entry of entries) {
    const key =
      entry.requestId === undefined
        ? `line:${entry.line}`
        : entry.runId === undefined
          ? `${entry.epoch}:${entry.requestId}`
          : `run:${entry.runId}:${entry.requestId}`;
    const bucket = buckets.get(key);
    if (bucket) bucket.push(entry);
    else buckets.set(key, [entry]);
  }
  const groups: { group: LogGroup; minLine: number }[] = [];
  for (const [key, bucket] of buckets) {
    bucket.sort(byTimeThenLine);
    groups.push({
      group: buildGroup(key, bucket),
      minLine: Math.min(...bucket.map((e) => e.line)),
    });
  }
  groups.sort(
    (a, b) => b.group.start - a.group.start || b.minLine - a.minLine,
  );
  return groups.map((g) => g.group);
}

export type Tone = "success" | "redirect" | "error" | "none";

/** 2xx success，3xx redirect，4xx/5xx error，其他（含 undefined、非整数）none。 */
export function statusTone(status: unknown): Tone {
  if (typeof status !== "number" || !Number.isInteger(status)) return "none";
  if (status >= 200 && status < 300) return "success";
  if (status >= 300 && status < 400) return "redirect";
  if (status >= 400 && status < 600) return "error";
  return "none";
}

/** 级别配色：ERROR→"error"，WARN→"warn"，DEBUG→"debug"，其余→"info"（大小写不敏感）。 */
export function levelTone(level: string): "error" | "warn" | "info" | "debug" {
  switch (level.toUpperCase()) {
    case "ERROR":
      return "error";
    case "WARN":
      return "warn";
    case "DEBUG":
      return "debug";
    default:
      return "info";
  }
}

/** 记录的 fields 序列化并转为小写的结果缓存；输入框每次变化都要筛选，避免重复序列化。 */
const fieldsTextCache = new WeakMap<LogEntry, string>();
/** item 的小写 body 缓存，同样避免每次输入重复转小写。 */
const bodyTextCache = new WeakMap<LogItem, string>();

function fieldsText(entry: LogEntry): string {
  let text = fieldsTextCache.get(entry);
  if (text === undefined) {
    text = JSON.stringify(entry.fields).toLowerCase();
    fieldsTextCache.set(entry, text);
  }
  return text;
}

function bodyText(item: LogItem, body: string): string {
  let text = bodyTextCache.get(item);
  if (text === undefined) {
    text = body.toLowerCase();
    bodyTextCache.set(item, text);
  }
  return text;
}

/**
 * 过滤组：query 去除首尾空白后为空时原样返回；否则（大小写不敏感）满足任一条件即保留：
 * request_id 等于 query；method 或 path 包含 query；任一 item 的 msg、body 或 fields 的 JSON 文本包含 query。保持原顺序。
 */
export function filterGroups(groups: LogGroup[], query: string): LogGroup[] {
  const trimmed = query.trim();
  if (trimmed === "") return groups;
  const q = trimmed.toLowerCase();
  const has = (text: string | undefined) =>
    text !== undefined && text.toLowerCase().includes(q);
  return groups.filter(
    (group) =>
      (group.requestId !== undefined && String(group.requestId) === trimmed) ||
      has(group.method) ||
      has(group.path) ||
      group.items.some(
        (item) =>
          has(item.entry.msg) ||
          (item.body !== undefined && bodyText(item, item.body).includes(q)) ||
          fieldsText(item.entry).includes(q),
      ),
  );
}

const pad = (n: number, width = 2) => String(n).padStart(width, "0");

/** 本地时间，含毫秒；withDate 为 false 时省略日期。 */
export function formatLogTime(ts: number, withDate = true): string {
  const d = new Date(ts);
  const time = `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}.${pad(d.getMilliseconds(), 3)}`;
  return withDate
    ? `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${time}`
    : time;
}
