// 账号详情弹窗：以模态框显示账号卡片，所有操作在卡片内完成。
// 按 Esc、点击遮罩或「×」关闭；关闭后焦点交还给打开前的元素（表格行）。
import { useEffect, useRef } from "react";
import type { AccountView } from "../api";
import type { Actions } from "../actions";
import { AccountCard } from "./AccountCard";

export function AccountDialog({
  account,
  actions,
  onClose,
}: {
  account: AccountView;
  actions: Actions;
  onClose: () => void;
}) {
  const ref = useRef<HTMLDialogElement>(null);
  useEffect(() => {
    const opener = document.activeElement as HTMLElement | null;
    const dialog = ref.current;
    if (dialog && !dialog.open) dialog.showModal();
    return () => opener?.focus();
  }, []);
  return (
    <dialog
      ref={ref}
      className="account-dialog"
      aria-label={`账号 ${account.user}`}
      // 卡片内的确认、代理弹窗的 cancel 与 click 会沿 React 组件树冒泡到这里，
      // 只处理目标为本弹窗自身的事件（Esc、点击遮罩）。
      onCancel={(e) => {
        if (e.target !== e.currentTarget) return;
        e.preventDefault();
        onClose();
      }}
      onClick={(e) => {
        if (e.target === e.currentTarget) onClose();
      }}
    >
      <button className="dialog-close" aria-label="关闭" onClick={onClose}>
        ×
      </button>
      <AccountCard account={account} actions={actions} />
    </dialog>
  );
}
