// 确认对话框：禁用、删除等操作前弹出，点击「确认」才执行，防止误触。
import { useEffect, useRef } from "react";

export function ConfirmDialog({
  message,
  onConfirm,
  onCancel,
}: {
  message: string;
  onConfirm: () => void;
  /** 点击「取消」或按 Esc 时调用。 */
  onCancel: () => void;
}) {
  const ref = useRef<HTMLDialogElement>(null);
  const cancelRef = useRef<HTMLButtonElement>(null);
  // 挂载即以模态方式打开，并聚焦「取消」，误按回车不会执行操作；
  // 卸载时对话框随元素移除而关闭，焦点交还给打开前的元素（触发按钮）。
  useEffect(() => {
    const opener = document.activeElement as HTMLElement | null;
    const dialog = ref.current;
    if (dialog && !dialog.open) dialog.showModal();
    cancelRef.current?.focus();
    return () => opener?.focus();
  }, []);
  return (
    <dialog
      ref={ref}
      className="confirm-dialog"
      onCancel={(e) => {
        e.preventDefault();
        onCancel();
      }}
    >
      <p>{message}</p>
      <div className="confirm-actions">
        <button ref={cancelRef} onClick={onCancel}>
          取消
        </button>
        <button className="danger" onClick={onConfirm}>
          确认
        </button>
      </div>
    </dialog>
  );
}
