// 保存确认弹窗：显示修改前后的统一 diff（只列改动及上下文），确认后才写入配置文件。
// Esc 或「取消」关闭；保存中不可关闭；失败时错误显示在弹窗内且弹窗保持打开。
import { useEffect, useMemo, useRef } from "react";
import { fieldList } from "../configEditor";
import { diffLines, foldDiff } from "../diff";

export function ConfigDiffDialog({
  original,
  content,
  restartRequired,
  saving,
  error,
  onConfirm,
  onCancel,
}: {
  original: string;
  content: string;
  /** 当前校验结果中需重启才生效的字段。 */
  restartRequired: string[];
  saving: boolean;
  error: string;
  onConfirm: () => void;
  onCancel: () => void;
}) {
  const ref = useRef<HTMLDialogElement>(null);
  const cancelRef = useRef<HTMLButtonElement>(null);
  // 挂载即以模态方式打开，并聚焦「取消」；卸载时焦点交还给打开前的元素。
  useEffect(() => {
    const opener = document.activeElement as HTMLElement | null;
    const dialog = ref.current;
    if (dialog && !dialog.open) dialog.showModal();
    cancelRef.current?.focus();
    return () => opener?.focus();
  }, []);

  const lines = useMemo(() => diffLines(original, content), [original, content]);
  const items = useMemo(() => foldDiff(lines), [lines]);
  const added = lines.filter((l) => l.type === "add").length;
  const removed = lines.filter((l) => l.type === "del").length;

  return (
    <dialog
      ref={ref}
      className="config-diff-dialog"
      aria-label="确认保存配置"
      onCancel={(e) => {
        e.preventDefault();
        if (!saving) onCancel();
      }}
    >
      <h2>
        确认保存配置
        <small className="diff-stat">
          <span className="diff-added">+{added}</span> <span className="diff-removed">−{removed}</span>
        </small>
      </h2>
      <div className="diff-body">
        {items.length === 0 ? (
          // diffLines 按行比较，内容不同却没有差异行，只可能是文件末尾换行不同。
          <p className="muted diff-empty">仅文件末尾换行不同</p>
        ) : (
          items.map((item, i) =>
            item.type === "skip" ? (
              <div key={i} className="diff-row skip">
                ⋯ 省略 {item.count} 行未变内容
              </div>
            ) : (
              <div key={i} className={"diff-row " + item.type}>
                <span className="diff-no">{item.oldLine}</span>
                <span className="diff-no">{item.newLine}</span>
                <span className="diff-sign">{item.type === "add" ? "+" : item.type === "del" ? "−" : " "}</span>
                <span className="diff-text">{item.text}</span>
              </div>
            ),
          )
        )}
      </div>
      {restartRequired.length > 0 && (
        <p className="hint">以下字段需重启服务后生效：{fieldList(restartRequired)}</p>
      )}
      {error && (
        <p className="hint error" role="alert">
          {error}
        </p>
      )}
      <div className="confirm-actions">
        <button ref={cancelRef} disabled={saving} onClick={onCancel}>
          取消
        </button>
        <button className="primary" disabled={saving} onClick={onConfirm}>
          {saving ? "保存中…" : "确认保存"}
        </button>
      </div>
    </dialog>
  );
}
