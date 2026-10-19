// 应用入口组件：未登录时显示登录页；登录后显示控制台（概览 / 账号与额度）。
import { useCallback, useEffect, useMemo, useState } from "react";
import {
  createManagementClient,
  errorMessage,
  isUnauthorized,
  type AccountView,
  type LoginStart,
} from "./api";
import type { Actions } from "./actions";
import { DeviceLoginPanel } from "./components/DeviceLoginPanel";
import { AccountsPage } from "./pages/AccountsPage";
import { LoginPage } from "./pages/LoginPage";
import { OverviewPage } from "./pages/OverviewPage";
import { clearSessionKey, loadSessionKey, saveSessionKey } from "./session";

/** 账号列表自动刷新间隔（毫秒）。 */
const REFRESH_INTERVAL_MS = 15_000;
const EXPIRED_MESSAGE = "管理密钥已失效，请重新输入";

type Page = "overview" | "accounts";
const pages: { id: Page; label: string; icon: string }[] = [
  { id: "overview", label: "概览", icon: "◎" },
  { id: "accounts", label: "账号与额度", icon: "◈" },
];

export function App() {
  // 管理密钥保存在 React state 中，并同步到当前标签页的 sessionStorage（见 session.ts），
  // 因此刷新页面后仍保持登录；关闭标签页或退出登录、密钥失效时清除。不写入 localStorage、cookie 或 URL。
  const [managementKey, setManagementKey] = useState(loadSessionKey);
  const [loginNotice, setLoginNotice] = useState("");

  const onLogin = useCallback((key: string) => {
    setLoginNotice("");
    saveSessionKey(key);
    setManagementKey(key);
  }, []);
  const onExpire = useCallback((message: string) => {
    setLoginNotice(message);
    clearSessionKey();
    setManagementKey("");
  }, []);
  const onLogout = useCallback(() => {
    clearSessionKey();
    setManagementKey("");
  }, []);

  if (!managementKey)
    return <LoginPage notice={loginNotice} onLogin={onLogin} />;
  return (
    <Console
      managementKey={managementKey}
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
  managementKey,
  onLogout,
  onExpire,
}: {
  managementKey: string;
  onLogout: () => void;
  /** 密钥失效时调用，回到登录页并给出提示。 */
  onExpire: (message: string) => void;
}) {
  const client = useMemo(
    () => createManagementClient(managementKey),
    [managementKey],
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
