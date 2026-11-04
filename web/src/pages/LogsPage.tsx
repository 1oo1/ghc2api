// 日志页：读取服务端日志尾部的原始文本，在浏览器中解析、按请求分组并支持筛选。
// 首次进入时自动加载一次，之后只在点击「加载」或顶栏「刷新」时重新加载，不自动刷新。
import { useEffect } from "react";
import { Empty } from "../components/Empty";
import { LogGroupList } from "../components/LogGroupList";
import { LOG_SIZE_OPTIONS, type LogsState } from "../useLogs";

const MIB = 1024 * 1024;

export function LogsPage({ logs }: { logs: LogsState }) {
  const { load, loaded, loading } = logs;
  // 首次进入（尚未加载过）时自动加载；失败后再次进入会重试。
  useEffect(() => {
    if (!loaded) void load();
    // 只在挂载时检查一次。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  return (
    <>
      <div className="logs-toolbar">
        <label className="logs-size">
          读取大小
          <select
            value={logs.maxBytes}
            disabled={loading}
            onChange={(e) => logs.setMaxBytes(Number(e.target.value))}
          >
            {LOG_SIZE_OPTIONS.map((bytes) => (
              <option key={bytes} value={bytes}>
                {bytes / MIB} MiB
              </option>
            ))}
          </select>
        </label>
        <button className="primary" disabled={loading} onClick={() => void load()}>
          {loading ? "加载中…" : "加载"}
        </button>
        <input
          type="search"
          className="logs-filter"
          placeholder="按 request_id、路径或内容筛选"
          aria-label="筛选日志"
          value={logs.query}
          onChange={(e) => logs.setQuery(e.target.value)}
        />
      </div>
      {logs.error && (
        <div className="banner error" role="alert">
          日志加载失败：{logs.error}
        </div>
      )}
      {/* 加载状态与统计放在 aria-live 区域，读屏软件可播报加载与完成。 */}
      <p className="logs-status" role="status" aria-live="polite">
        {loading
          ? "正在加载日志…"
          : loaded &&
            `${
              logs.visible.length === logs.totalGroups
                ? `${logs.totalGroups} 组`
                : `${logs.visible.length} / ${logs.totalGroups} 组`
            } · ${logs.entryCount} 条记录 · ${logs.invalidCount} 行无效`}
      </p>
      {loaded && (
        <>
          {loaded.truncated && (
            <div className="hint">
              只显示最近 {loaded.maxBytes / MIB} MiB 的日志，更早的内容未读取。
            </div>
          )}
          {logs.visible.length === 0 ? (
            <Empty
              title={logs.totalGroups === 0 ? "没有日志" : "没有匹配的日志"}
              text={
                logs.totalGroups === 0
                  ? "日志文件为空，或所选读取大小内没有完整的日志行。"
                  : "换一个筛选条件试试。"
              }
            />
          ) : (
            <LogGroupList key={logs.loadId} groups={logs.visible} />
          )}
        </>
      )}
    </>
  );
}
