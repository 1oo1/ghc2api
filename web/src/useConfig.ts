// 配置页的状态：已加载的配置、编辑中的内容、防抖校验与保存。
// 状态放在控制台（App.tsx）中，因此切换页面不会丢失未保存的修改。
import { useCallback, useEffect, useRef, useState } from "react";
import {
  ApiError,
  errorMessage,
  isUnauthorized,
  type ConfigValidation,
  type ManagementClient,
} from "./api";
import { canSave, fieldList, fromEditorText, isDirty, toEditorText } from "./configEditor";

/** 内容停止变化多久后发起校验（毫秒）。 */
const VALIDATE_DELAY_MS = 400;

type Validation = { content: string; result: ConfigValidation };

export function useConfig(
  client: ManagementClient,
  onUnauthorized: () => void,
  /** 保存后管理密钥发生变化时调用（旧令牌已失效，需重新登录）。 */
  onManagementKeyChanged: () => void,
) {
  /** 尚未成功加载过时为 false。 */
  const [loaded, setLoaded] = useState(false);
  const [path, setPath] = useState("");
  const [revision, setRevision] = useState("");
  const [writable, setWritable] = useState(false);
  /** 原文是否使用 CRLF：编辑器内一律是 LF，保存时据此还原。 */
  const [crlf, setCrlf] = useState(false);
  const [original, setOriginal] = useState("");
  const [content, setContentState] = useState("");
  const [restartRequired, setRestartRequired] = useState<string[]>([]);
  const [loading, setLoading] = useState(false);
  const [loadError, setLoadError] = useState("");
  const [saving, setSaving] = useState(false);
  const [notice, setNotice] = useState("");
  const [validation, setValidation] = useState<Validation | null>(null);
  /** 校验请求本身失败（网络错误等）；按内容记录，内容变化后自动失效。 */
  const [validateFailure, setValidateFailure] = useState<{ content: string; message: string } | null>(null);
  /** 同步的“加载中”标记，避免重复触发并发请求。 */
  const inflight = useRef(false);

  const load = useCallback(async () => {
    if (inflight.current) return;
    inflight.current = true;
    setLoading(true);
    setLoadError("");
    try {
      const snapshot = await client.getConfig();
      setPath(snapshot.path);
      setRevision(snapshot.revision);
      setWritable(snapshot.writable);
      const editor = toEditorText(snapshot.content);
      setCrlf(editor.crlf);
      setOriginal(editor.text);
      setContentState(editor.text);
      setRestartRequired(snapshot.restart_required);
      setValidation(null);
      setValidateFailure(null);
      setNotice("");
      setLoaded(true);
    } catch (e) {
      if (isUnauthorized(e)) onUnauthorized();
      else setLoadError(errorMessage(e, "配置加载失败"));
    } finally {
      inflight.current = false;
      setLoading(false);
    }
  }, [client, onUnauthorized]);

  const setContent = useCallback((next: string) => {
    setContentState(next);
    setNotice("");
  }, []);
  const revert = useCallback(() => {
    setContentState(original);
    setNotice("");
  }, [original]);

  // 内容停止变化 400ms 后向服务端校验；内容再次变化或组件卸载时清除定时器，并丢弃过时的响应。
  // 未修改时不校验。
  useEffect(() => {
    if (!loaded || content === original) return;
    let stale = false;
    const timer = window.setTimeout(() => {
      client
        .validateConfig(content)
        .then((result) => {
          if (!stale) setValidation({ content, result });
        })
        .catch((e) => {
          if (stale) return;
          if (isUnauthorized(e)) onUnauthorized();
          else setValidateFailure({ content, message: errorMessage(e, "校验请求失败") });
        });
    }, VALIDATE_DELAY_MS);
    return () => {
      stale = true;
      window.clearTimeout(timer);
    };
  }, [client, loaded, content, original, onUnauthorized]);

  const dirty = isDirty({ original, content, writable, saving, validation });

  // 有未保存的修改时，关闭或刷新浏览器标签页前提示。
  useEffect(() => {
    if (!dirty) return;
    const warn = (e: BeforeUnloadEvent) => e.preventDefault();
    window.addEventListener("beforeunload", warn);
    return () => window.removeEventListener("beforeunload", warn);
  }, [dirty]);

  /** 保存当前内容。成功返回 null；失败返回错误文案（登录失效时回到登录页并返回 null）。 */
  const save = useCallback(async (): Promise<string | null> => {
    setSaving(true);
    try {
      const result = await client.saveConfig(fromEditorText(content, crlf), revision);
      setOriginal(content);
      setRevision(result.revision);
      setRestartRequired(result.restart_required);
      setNotice(
        "配置已保存并已生效" +
          (result.restart_required.length > 0
            ? `；需重启生效：${fieldList(result.restart_required)}`
            : ""),
      );
      if (result.changed.includes("management_key")) {
        onManagementKeyChanged();
      }
      return null;
    } catch (e) {
      if (isUnauthorized(e)) {
        onUnauthorized();
        return null;
      }
      const message = errorMessage(e, "保存失败");
      return e instanceof ApiError && e.status === 409
        ? `${message}（请关闭后点击 ↻ 刷新重新加载）`
        : message;
    } finally {
      setSaving(false);
    }
  }, [client, content, crlf, revision, onUnauthorized, onManagementKeyChanged]);

  const current = validation?.content === content ? validation.result : null;
  const failure = validateFailure?.content === content ? validateFailure.message : "";
  return {
    loaded,
    path,
    writable,
    original,
    content,
    setContent,
    revert,
    /** 配置文件中需重启才生效的字段（加载或保存后更新）。 */
    restartRequired,
    loading,
    loadError,
    load,
    saving,
    save,
    /** 保存成功的提示；再次编辑或重新加载后清除。 */
    notice,
    dirty,
    canSave: canSave({ original, content, writable, saving, validation }),
    /** 当前内容的校验结果；尚未校验（含校验中）时为 null。 */
    validation: current,
    /** 当前内容的校验请求失败原因；没有时为空串。 */
    validateError: failure,
  };
}

export type ConfigState = ReturnType<typeof useConfig>;
