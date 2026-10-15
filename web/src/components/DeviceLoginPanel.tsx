// GitHub 设备登录面板：展示验证码与授权页链接，按服务端给出的间隔轮询授权结果。
import { useEffect } from "react";
import type { LoginStart } from "../api";
import type { Actions } from "../actions";

export function DeviceLoginPanel({
  login,
  actions,
  onClose,
}: {
  login: LoginStart;
  actions: Actions;
  /** 隐藏面板并停止轮询：用户点击“隐藏”，或授权已有结果时调用。 */
  onClose: () => void;
}) {
  useEffect(() => {
    let alive = true;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const intervalMs = Math.max(1, login.interval) * 1000;

    const poll = async () => {
      try {
        const result = await actions.client.loginStatus(login.state);
        if (!alive) return;
        if (result.status === "pending") {
          timer = setTimeout(poll, intervalMs);
          return;
        }
        onClose();
        if (result.status === "success") {
          void actions.run(async () => "GitHub Copilot 账号已添加");
        } else {
          actions.fail(result.error ?? "GitHub 授权未完成");
        }
      } catch (e) {
        if (!alive) return;
        onClose();
        actions.fail(e instanceof Error ? e.message : "授权状态查询失败");
      }
    };

    timer = setTimeout(poll, intervalMs);
    return () => {
      alive = false;
      clearTimeout(timer);
    };
    // 只在登录会话变化时重新轮询；actions.run / actions.fail 由控制台以 useCallback 提供，引用稳定。
  }, [login, actions.client]);

  return (
    <section className="panel login-flow">
      <h2>完成 GitHub 授权</h2>
      <p>
        在 GitHub 授权页输入验证码 <code>{login.user_code}</code>
        ，完成后此处会自动添加账号。
      </p>
      <div className="login-actions">
        <a
          className="button primary"
          href={login.url}
          target="_blank"
          rel="noopener noreferrer"
        >
          打开 GitHub 授权页 ↗
        </a>
        <button onClick={onClose}>隐藏</button>
      </div>
      <small>
        验证码约 {Math.max(1, Math.round(login.expires_in / 60))} 分钟内有效，
        等待授权中（每 {Math.max(1, login.interval)} 秒检查一次）。隐藏后不再显示授权结果；
        授权完成后账号仍会被添加，列表会自动刷新。
      </small>
    </section>
  );
}
