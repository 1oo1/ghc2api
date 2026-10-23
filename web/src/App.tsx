// 应用入口组件：未登录时显示登录页；登录后显示控制台（概览 / 账号与额度）。
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  createManagementClient,
  errorMessage,
  isUnauthorized,
  type AccountView,
  type ConsoleSession,
  type LoginStart,
} from "./api";
import type { Actions } from "./actions";
import { DeviceLoginPanel } from "./components/DeviceLoginPanel";
import { AccountsPage } from "./pages/AccountsPage";
import { LoginPage } from "./pages/LoginPage";
import { OverviewPage } from "./pages/OverviewPage";
import { clearSession, loadSession, saveSession } from "./session";

/** 账号列表自动刷新间隔（毫秒）。 */
const REFRESH_INTERVAL_MS = 15_000;
const EXPIRED_MESSAGE = "登录已失效，请重新输入管理密钥";

type Page = "overview" | "accounts";
const pages: { id: Page; label: string; icon: string }[] = [
  { id: "overview", label: "概览", icon: "◎" },
  { id: "accounts", label: "账号与额度", icon: "◈" },
];

export function App() {
  // 控制台令牌（由管理密钥换取，见 session.ts）保存在 React state 与 localStorage 中；
  // 管理密钥本身只在登录时发送一次，不做任何保存。退出登录或令牌失效时清除。
  const [token, setToken] = useState(loadSession);
  const [loginNotice, setLoginNotice] = useState("");
  // 当前令牌的同步副本，供异步续期回调判断结果是否已过时（期间已退出或重新登录）。
  const tokenRef = useRef(token);
  const updateToken = useCallback((next: string) => {
    tokenRef.current = next;
    setToken(next);
  }, []);

  const onLogin = useCallback(
    (session: ConsoleSession) => {
      setLoginNotice("");
      saveSession(session.token, session.expires_at);
      updateToken(session.token);
    },
    [updateToken],
  );
  const onExpire = useCallback(
    (message: string) => {
      setLoginNotice(message);
      clearSession();
      updateToken("");
    },
    [updateToken],
  );
  const onLogout = useCallback(() => {
    clearSession();
    updateToken("");
  }, [updateToken]);

  // 打开页面时若已登录，用现有令牌换新令牌，有效期重新计为 14 天；
  // 令牌被拒（如管理密钥已更换）则回到登录页，网络错误时继续使用现有令牌。
  // 响应返回前令牌已变化（退出、重新登录或 StrictMode 下的另一次续期已生效）时忽略该响应。
  useEffect(() => {
    const saved = tokenRef.current;
    if (!saved) return;
    createManagementClient(saved)
      .createSession()
      .then((session) => {
        if (tokenRef.current === saved) onLogin(session);
      })
      .catch((e) => {
        if (tokenRef.current === saved && isUnauthorized(e))
          onExpire(EXPIRED_MESSAGE);
      });
  }, [onLogin, onExpire]);

  if (!token) return <LoginPage notice={loginNotice} onLogin={onLogin} />;
  return (
    <Console
      credential={token}
      onLogout={onLogout}
      onExpire={onExpire}
    />
  );
}

/**
 * 已登录的控制台。负责加载账号列表、定时刷新、执行变更操作与设备登录；
 * 各页面只负责展示。组件卸载即丢弃全部状态（退出登录时）。
 */
function Console({
  credential,
  onLogout,
  onExpire,
}: {
  /** 控制台令牌。 */
  credential: string;
  onLogout: () => void;
  /** 令牌失效时调用，回到登录页并给出提示。 */
  onExpire: (message: string) => void;
}) {
  const client = useMemo(
    () => createManagementClient(credential),
    [credential],
  );
  const [page, setPage] = useState<Page>("overview");
  const [accounts, setAccounts] = useState<AccountView[]>([]);
  /** 服务端版本；加载失败时不显示，不影响控制台使用。 */
  const [version, setVersion] = useState("");
  const [login, setLogin] = useState<LoginStart | null>(null);
  const [busy, setBusy] = useState(false);
  const [notice, setNotice] = useState("");
  /** 操作类错误（点击按钮后的失败）。 */
  const [error, setError] = useState("");
  /** 列表刷新错误；下一次刷新成功后清除。 */
  const [syncError, setSyncError] = useState("");

  useEffect(() => {
    client
      .getVersion()
      .then((v) => setVersion(v.version))
      .catch(() => {});
  }, [client]);

  const reload = useCallback(async () => {
    try {
      setAccounts(await client.listAccounts());
      setSyncError("");
    } catch (e) {
      if (isUnauthorized(e)) onExpire(EXPIRED_MESSAGE);
      else setSyncError(errorMessage(e));
    }
  }, [client, onExpire]);

  /** 执行一次变更操作：期间禁用按钮，完成后刷新账号列表。 */
  const run = useCallback(
    async (task: () => Promise<string | void>) => {
      setBusy(true);
      setError("");
      setNotice("");
      try {
        const message = await task();
        if (message) setNotice(message);
      } catch (e) {
        if (isUnauthorized(e)) {
          onExpire(EXPIRED_MESSAGE);
          return;
        }
        setError(errorMessage(e));
      } finally {
        setBusy(false);
      }
      await reload();
    },
    [reload, onExpire],
  );

  const fail = useCallback((message: string) => setError(message), []);

  // 进入控制台后立即加载，并每 15 秒自动刷新。
  useEffect(() => {
    void reload();
    const timer = window.setInterval(() => void reload(), REFRESH_INTERVAL_MS);
    return () => window.clearInterval(timer);
  }, [reload]);

  const startLogin = () =>
    void run(async () => {
      setLogin(await client.startLogin());
    });

  const actions: Actions = { client, busy, run, fail };
  const title = pages.find((p) => p.id === page)?.label ?? "";

  return (
    <div className="shell">
      <aside>
        <div className="brand">
          <span className="brand-mark">G</span>
          <div>
            ghc2api
            <small>GITHUB COPILOT</small>
            {version && <small className="version">{version}</small>}
          </div>
        </div>
        <nav>
          {pages.map((p) => (
            <button
              className={page === p.id ? "selected" : ""}
              key={p.id}
              onClick={() => setPage(p.id)}
            >
              <span>{p.icon}</span>
              {p.label}
            </button>
          ))}
        </nav>
        <div className="sidebar-foot">
          <button onClick={onLogout}>退出登录</button>
        </div>
      </aside>
      <main>
        <header>
          <h1>{title}</h1>
          <button onClick={() => void reload()} disabled={busy}>
            ↻ 刷新
          </button>
        </header>
        {error && (
          <div className="banner error" role="alert">
            {error}
          </div>
        )}
        {syncError && (
          <div className="banner error" role="alert">
            账号列表加载失败：{syncError}
          </div>
        )}
        {notice && (
          <div className="banner" role="status">
            {notice}
          </div>
        )}
        {login && (
          <DeviceLoginPanel
            login={login}
            actions={actions}
            onClose={() => setLogin(null)}
          />
        )}
        {page === "overview" ? (
          <OverviewPage accounts={accounts} />
        ) : (
          <AccountsPage
            accounts={accounts}
            actions={actions}
            loginPending={login !== null}
            onStartLogin={startLogin}
          />
        )}
      </main>
    </div>
  );
}
