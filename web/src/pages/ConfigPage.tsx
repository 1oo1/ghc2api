// 配置页：查看并编辑配置文件（YAML），修改经服务端校验、diff 确认后保存。
// 首次进入时自动加载一次；之后只在顶栏「刷新」时重新加载。状态由控制台持有（useConfig）。
import { useEffect, useState } from "react";
import { ConfigDiffDialog } from "../components/ConfigDiffDialog";
import { YamlEditor } from "../components/YamlEditor";
import { fieldList } from "../configEditor";
import type { ConfigState } from "../useConfig";

export function ConfigPage({ config }: { config: ConfigState }) {
  const { load, loaded, loading } = config;
  const [confirming, setConfirming] = useState(false);
  const [saveError, setSaveError] = useState("");

  // 首次进入（尚未加载过）时自动加载；失败后再次进入会重试。
  useEffect(() => {
    if (!loaded) void load();
    // 只在挂载时检查一次。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const confirmSave = async () => {
    setSaveError("");
    const failure = await config.save();
    if (failure) setSaveError(failure);
    else setConfirming(false);
  };

  const { validation } = config;
  const status = !config.dirty
    ? ""
    : config.validateError
      ? `✗ 校验请求失败：${config.validateError}`
      : validation === null
        ? "校验中…"
        : validation.valid
          ? "✓ 配置有效"
          : `✗ ${validation.line ? `第 ${validation.line} 行：` : ""}${validation.error ?? "配置无效"}`;
  const statusTone = validation && !validation.valid ? "invalid" : config.validateError ? "invalid" : "";

  return (
    <>
      {config.loadError && (
        <div className="banner error" role="alert">
          配置加载失败：{config.loadError}
        </div>
      )}
      {config.notice && (
        <div className="banner" role="status">
          {config.notice}
        </div>
      )}
      {!loaded ? (
        <p className="logs-status" role="status">
          {loading ? "正在加载配置…" : ""}
        </p>
      ) : (
        <>
          <div className="config-head">
            <code className="config-path">{config.path}</code>
            {!config.writable && <span className="badge disabled">只读</span>}
          </div>
          {!config.writable && (
            <div className="hint">配置文件不可写（例如以只读方式挂载），只能查看</div>
          )}
          {config.restartRequired.length > 0 && (
            <div className="hint">
              以下配置已写入文件，需重启服务后生效：{fieldList(config.restartRequired)}
            </div>
          )}
          <YamlEditor
            value={config.content}
            onChange={config.setContent}
            readOnly={!config.writable || config.saving}
            errorLine={validation && !validation.valid ? validation.line : undefined}
          />
          <div className="config-footer">
            <p className={"config-status " + statusTone} role="status" aria-live="polite">
              {status}
            </p>
            <button disabled={!config.dirty || config.saving} onClick={config.revert}>
              撤销修改
            </button>
            <button
              className="primary"
              disabled={!config.canSave}
              onClick={() => {
                setSaveError("");
                setConfirming(true);
              }}
            >
              保存
            </button>
          </div>
        </>
      )}
      {confirming && (
        <ConfigDiffDialog
          original={config.original}
          content={config.content}
          restartRequired={validation?.restart_required ?? []}
          saving={config.saving}
          error={saveError}
          onConfirm={() => void confirmSave()}
          onCancel={() => setConfirming(false)}
        />
      )}
    </>
  );
}
