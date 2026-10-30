// 代理弹窗：填写或编辑账号的 SOCKS5 代理（主机、端口、用户名、密码，scheme 固定 socks5）。
// 两种用法：导入凭证 / GitHub 登录前选择代理（留空即不使用代理），以及编辑账号当前代理（保存、删除）。
import { useEffect, useId, useRef, useState } from "react";
import type { ProxyView } from "../api";
import {
  proxyFormChanged,
  passwordPlaceholder,
  proxyFormFromView,
  validateProxyForm,
  type ProxyForm,
} from "../proxy";
import { ConfirmDialog } from "./ConfirmDialog";

/** 回调的返回值：错误文案会显示在弹窗内且弹窗保持打开；返回空值表示成功，由调用方关闭弹窗。 */
type Outcome = string | null | void;

export function ProxyDialog({
  title,
  hint,
  current,
  confirmLabel,
  requireChange = false,
  onConfirm,
  onDelete,
  onCancel,
}: {
  title: string;
  /** 标题下方的说明文字。 */
  hint?: string;
  /** 账号当前的代理；用于预填表单、判断是否有变化，并决定是否显示「清除密码」与「删除」。 */
  current?: ProxyView;
  /** 确认按钮的文字，如「保存」「继续」。 */
  confirmLabel: string;
  /** 为 true 时表单没有变化则确认按钮置灰（编辑场景）。 */
  requireChange?: boolean;
  onConfirm: (form: ProxyForm) => Outcome | Promise<Outcome>;
  /** 提供时，当前有代理才显示「删除」，二次确认后调用。 */
  onDelete?: () => Outcome | Promise<Outcome>;
  /** 点击「取消」或按 Esc 时调用。 */
  onCancel: () => void;
}) {
  const ref = useRef<HTMLDialogElement>(null);
  const hostRef = useRef<HTMLInputElement>(null);
  const titleId = useId();
  const [form, setForm] = useState(() => proxyFormFromView(current));
  const [error, setError] = useState("");
  const [working, setWorking] = useState(false);
  const [confirmingDelete, setConfirmingDelete] = useState(false);

  // 挂载即以模态方式打开并聚焦主机输入框；卸载时焦点交还给打开前的元素。
  useEffect(() => {
    const opener = document.activeElement as HTMLElement | null;
    const dialog = ref.current;
    if (dialog && !dialog.open) dialog.showModal();
    hostRef.current?.focus();
    return () => opener?.focus();
  }, []);

  const invalid = validateProxyForm(form, current);
  const canConfirm = !working && invalid === null && (!requireChange || proxyFormChanged(form, current));
  const update = (patch: Partial<ProxyForm>) => setForm((f) => ({ ...f, ...patch }));

  /** 执行回调：期间禁用按钮，失败时在弹窗内显示错误。 */
  async function attempt(task: () => Outcome | Promise<Outcome>) {
    setWorking(true);
    setError("");
    try {
      const failure = await task();
      if (failure) setError(failure);
    } finally {
      setWorking(false);
    }
  }

  return (
    <>
      <dialog
        ref={ref}
        className="proxy-dialog"
        aria-labelledby={titleId}
        onCancel={(e) => {
          e.preventDefault();
          if (!working) onCancel();
        }}
      >
        <form
          onSubmit={(e) => {
            e.preventDefault();
            if (canConfirm) void attempt(() => onConfirm(form));
          }}
        >
          <h2 id={titleId}>{title}</h2>
          {hint && <p className="muted">{hint}</p>}
          <div className="proxy-fields">
            <label className="proxy-host">
              主机
              <input
                ref={hostRef}
                value={form.host}
                placeholder="127.0.0.1"
                autoComplete="off"
                onChange={(e) => update({ host: e.target.value })}
              />
            </label>
            <label className="proxy-port">
              端口
              <input
                value={form.port}
                inputMode="numeric"
                placeholder="1080"
                autoComplete="off"
                aria-invalid={invalid !== null}
                onChange={(e) => update({ port: e.target.value })}
              />
            </label>
            <label>
              用户名
              <input
                value={form.username}
                placeholder="可选"
                autoComplete="off"
                onChange={(e) => update({ username: e.target.value })}
              />
            </label>
            <label>
              密码
              <input
                type="password"
                value={form.password}
                placeholder={passwordPlaceholder(form, current)}
                autoComplete="new-password"
                onChange={(e) => update({ password: e.target.value })}
              />
            </label>
          </div>
          {current && (
            <label className="proxy-clear">
              <input
                type="checkbox"
                checked={form.clearPassword}
                onChange={(e) => update({ clearPassword: e.target.checked })}
              />
              清除密码
            </label>
          )}
          {invalid && <p className="hint error">{invalid}</p>}
          {error && (
            <p className="hint error" role="alert">
              {error}
            </p>
          )}
          <div className="confirm-actions">
            {current && onDelete && (
              <button
                type="button"
                className="danger proxy-delete"
                disabled={working}
                onClick={() => setConfirmingDelete(true)}
              >
                删除
              </button>
            )}
            <button type="button" disabled={working} onClick={onCancel}>
              取消
            </button>
            <button type="submit" className="primary" disabled={!canConfirm}>
              {working ? "处理中…" : confirmLabel}
            </button>
          </div>
        </form>
      </dialog>
      {confirmingDelete && onDelete && (
        <ConfirmDialog
          message="删除该账号的代理？之后账号将直连 GitHub。"
          onCancel={() => setConfirmingDelete(false)}
          onConfirm={() => {
            setConfirmingDelete(false);
            void attempt(onDelete);
          }}
        />
      )}
    </>
  );
}
