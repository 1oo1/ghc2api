// 日志页的状态：读取大小、已加载的日志、筛选条件，以及解析/分组/筛选的缓存结果。
// 状态放在控制台（App.tsx）中，因此切换到账号页再回来不会丢失已加载的日志与筛选条件。
import { useCallback, useDeferredValue, useMemo, useRef, useState } from "react";
import { errorMessage, isUnauthorized, type ManagementClient } from "./api";
import { filterGroups, groupLogs, parseLogText } from "./logs";

const MIB = 1024 * 1024;

/** 可选的读取大小（字节），与服务端上限 64 MiB 一致。 */
export const LOG_SIZE_OPTIONS = [2 * MIB, 8 * MIB, 32 * MIB, 64 * MIB];
export const DEFAULT_LOG_BYTES = 8 * MIB;

/** 一次加载的结果；maxBytes 是本次请求使用的读取大小。 */
type LoadedLogs = { text: string; truncated: boolean; maxBytes: number };

export function useLogs(client: ManagementClient, onUnauthorized: () => void) {
  const [maxBytes, setMaxBytes] = useState(DEFAULT_LOG_BYTES);
  const [query, setQuery] = useState("");
  const [loaded, setLoaded] = useState<LoadedLogs | null>(null);
  /** 每次加载成功加 1，用作组列表的 key：重新加载后组键可能指向别的记录，需丢弃展开状态。 */
  const [loadId, setLoadId] = useState(0);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  /** 同步的“加载中”标记，避免重复触发并发请求。 */
  const inflight = useRef(false);

  const load = useCallback(async () => {
    if (inflight.current) return;
    inflight.current = true;
    setLoading(true);
    setError("");
    try {
      const result = await client.getLogs(maxBytes);
      setLoaded({ ...result, maxBytes });
      setLoadId((id) => id + 1);
    } catch (e) {
      if (isUnauthorized(e)) onUnauthorized();
      else setError(errorMessage(e, "日志加载失败"));
    } finally {
      inflight.current = false;
      setLoading(false);
    }
  }, [client, maxBytes, onUnauthorized]);

  // 解析与分组只在日志文本变化时重算；筛选使用延迟值，输入保持流畅。
  const parsed = useMemo(
    () => (loaded ? parseLogText(loaded.text) : null),
    [loaded],
  );
  const groups = useMemo(
    () => (parsed ? groupLogs(parsed.entries) : []),
    [parsed],
  );
  const deferredQuery = useDeferredValue(query);
  const visible = useMemo(
    () => filterGroups(groups, deferredQuery),
    [groups, deferredQuery],
  );

  return {
    maxBytes,
    setMaxBytes,
    query,
    setQuery,
    /** 尚未成功加载过时为 null。 */
    loaded,
    loadId,
    loading,
    error,
    load,
    /** 解析出的记录数与无效行数；尚未加载时为 0。 */
    entryCount: parsed?.entries.length ?? 0,
    invalidCount: parsed?.invalid ?? 0,
    /** 全部组数（筛选前）与筛选后的组。 */
    totalGroups: groups.length,
    visible,
  };
}

export type LogsState = ReturnType<typeof useLogs>;
