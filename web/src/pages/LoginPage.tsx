// 登录页：输入管理密钥，通过 GET /v0/management/accounts 校验后进入控制台。
import { useState, type FormEvent } from "react";
import {
  ApiError,
  createManagementClient,
  errorMessage,
  isUnauthorized,
} from "../api";

export function LoginPage({
  notice,
  onLogin,
}: {
  /** 从控制台被踢回时的提示，例如密钥已失效。 */
  notice: string;
  onLogin: (managementKey: string) => void;
}) {
  const [input, setInput] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  async function submit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault();
    const key = input.trim();
    setBusy(true);
    setError("");
    try {
      await createManagementClient(key).listAccounts();
      onLogin(key);
    } catch (err) {
      setError(loginErrorMessage(err));
      setBusy(false);
    }
  }

  return (
    <main className="login">
      <span className="brand-mark">G</span>
      <h1>ghc2api</h1>
      <p>GitHub Copilot API 代理 · 管理控制台</p>
      <form onSubmit={(e) => void submit(e)}>
        <label>
          管理密钥
          <input
            autoComplete="current-password"
            aria-label="管理密钥"
            type="password"
            value={input}
            onChange={(e) => setInput(e.target.value)}
            required
          />
        </label>
        <button className="primary" disabled={busy}>
          进入控制台
        </button>
      </form>
      {notice && <div className="banner error">{notice}</div>}
      {error && <p role="alert" className="hint error">{error}</p>}
      <small>
        管理密钥即 config.yaml 中的 management_key。密钥保存在当前标签页的 sessionStorage 中，
        刷新页面后仍保持登录，关闭标签页或退出登录后清除。
      </small>
    </main>
  );
}

/** 将校验失败转换为用户可理解的提示。 */
function loginErrorMessage(error: unknown): string {
  if (isUnauthorized(error)) return "管理密钥无效";
  if (error instanceof ApiError && error.status === 404)
    return "管理接口未启用：请在 config.yaml 中设置 management_key 并重启服务";
  return `无法连接服务：${errorMessage(error)}`;
}
