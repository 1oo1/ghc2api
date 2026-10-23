// 登录页：输入管理密钥，通过 POST /v0/management/session 换取控制台令牌后进入控制台。
import { useState, type FormEvent } from "react";
import {
  ApiError,
  createManagementClient,
  errorMessage,
  isUnauthorized,
  type ConsoleSession,
} from "../api";

export function LoginPage({
  notice,
  onLogin,
}: {
  /** 从控制台被踢回时的提示，例如密钥已失效。 */
  notice: string;
  onLogin: (session: ConsoleSession) => void;
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
      onLogin(await createManagementClient(key).createSession());
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
        管理密钥即 config.yaml 中的 management_key。密钥只用于换取 14 天有效的登录令牌，不会保存在浏览器中；
        14 天内打开控制台会自动续期，超过 14 天未打开或退出登录后需重新输入。
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
