// 日志组列表：每组对应一个请求（或一条没有 request_id 的记录），默认折叠，展开后逐条显示；
// 最多先渲染 PAGE_SIZE 组，「显示更多」再加一批。组的展开状态按组键保存在组件内，
// 因此每次重新加载日志时调用方须用 key 重新挂载本组件（组键相对加载的文本，重新加载后可能指向别的记录）。正文与字段的代码块在展开时才格式化。
import { useState, type ReactNode } from "react";
import {
  formatLogTime,
  levelTone,
  statusTone,
  type LogGroup,
  type LogItem,
} from "../logs";
import { CodeBlock } from "./CodeBlock";

/** 每批渲染的组数。 */
const PAGE_SIZE = 200;

/** 记录摘要中显示的字段，按此顺序。 */
const SUMMARY_FIELDS = [
  "method",
  "url",
  "path",
  "status",
  "duration_ms",
  "account",
  "model",
  "error",
] as const;

export function LogGroupList({ groups }: { groups: LogGroup[] }) {
  const [limit, setLimit] = useState(PAGE_SIZE);
  return (
    <div className="log-groups">
      {groups.slice(0, limit).map((group) => (
        <LogGroupRow key={group.key} group={group} />
      ))}
      {groups.length > limit && (
        <button
          className="log-more"
          onClick={() => setLimit(limit + PAGE_SIZE)}
        >
          显示更多（已显示 {limit} / {groups.length} 组）
        </button>
      )}
    </div>
  );
}

function LogGroupRow({ group }: { group: LogGroup }) {
  const [open, setOpen] = useState(false);
  return (
    <article className="log-group">
      <button
        className="log-group-head"
        aria-expanded={open}
        onClick={() => setOpen(!open)}
      >
        <span className="log-caret" aria-hidden="true">
          {open ? "▾" : "▸"}
        </span>
        <span className="log-time">{formatLogTime(group.start)}</span>
        <span className="log-rid">
          {group.requestId === undefined ? "—" : `#${group.requestId}`}
          {group.runId !== undefined && (
            <small className="log-run" title={`run_id: ${group.runId}`}>
              {" "}
              {group.runId.slice(0, 8)}
            </small>
          )}
        </span>
        <span className="log-route">
          {[group.method, group.path].filter(Boolean).join(" ")}
        </span>
        {group.status === undefined ? (
          <span className="badge tone-none">未完成</span>
        ) : (
          <span className={`badge tone-${statusTone(group.status)}`}>
            {group.status}
          </span>
        )}
        <span className="log-duration">
          {group.durationMs === undefined ? "" : `${group.durationMs} ms`}
        </span>
        <span className="log-count">{group.items.length} 条</span>
        <span className={`badge level-${levelTone(group.level)}`}>
          {group.level || "—"}
        </span>
      </button>
      {open && (
        <ol className="log-items">
          {group.items.map((item) => (
            <LogItemRow key={item.entry.line} item={item} />
          ))}
        </ol>
      )}
    </article>
  );
}

/** 字段值的显示文本：字符串、数字、布尔原样，其他（对象、null）用 JSON。 */
function displayValue(value: unknown): string {
  return typeof value === "string" ? value : JSON.stringify(value);
}

function LogItemRow({ item }: { item: LogItem }) {
  const { entry } = item;
  return (
    <li className="log-item">
      <div className="log-item-line">
        <span className="log-time">{formatLogTime(entry.ts, false)}</span>
        <span className={`badge level-${levelTone(entry.level)}`}>
          {entry.level || "—"}
        </span>
        <span className="log-msg">{entry.msg}</span>
        {SUMMARY_FIELDS.filter((name) => entry.fields[name] !== undefined).map(
          (name) => (
            <span key={name} className="log-field">
              <b>{name}</b>{" "}
              <span
                className={
                  name === "status"
                    ? `log-status tone-${statusTone(entry.fields[name])}`
                    : undefined
                }
              >
                {displayValue(entry.fields[name])}
              </span>
            </span>
          ),
        )}
      </div>
      {item.body !== undefined && (
        <Fold
          label={
            <>
              正文
              {item.chunks !== undefined && ` · 共 ${item.chunks} 块`}
              {item.incomplete && <em className="log-warn"> · 不完整</em>}
            </>
          }
          render={() => <CodeBlock text={item.body as string} />}
        />
      )}
      <Fold
        label="字段"
        render={() => <CodeBlock text={JSON.stringify(entry.fields, null, 2)} />}
      />
    </li>
  );
}

/** 可折叠区域：默认折叠，展开后才调用 render 生成内容。 */
function Fold({ label, render }: { label: ReactNode; render: () => ReactNode }) {
  const [open, setOpen] = useState(false);
  return (
    <details
      className="log-fold"
      onToggle={(e) => setOpen(e.currentTarget.open)}
    >
      <summary>{label}</summary>
      {open && render()}
    </details>
  );
}
