// 账号与额度页：工具栏（导入凭证、GitHub 登录）与账号卡片列表。
import type { AccountView } from "../api";
import type { Actions } from "../actions";
import { AccountCard } from "../components/AccountCard";
import { Empty } from "../components/Empty";

/** 凭证文件大小上限，与服务端请求体上限（1 MiB）一致。 */
const MAX_CREDENTIAL_BYTES = 1024 * 1024;

export function AccountsPage({
  accounts,
  actions,
  loginPending,
  onStartLogin,
}: {
  accounts: AccountView[];
  actions: Actions;
  /** 设备登录进行中，此时禁止再次发起。 */
  loginPending: boolean;
  onStartLogin: () => void;
}) {
  function importCredential(file: File) {
    void actions.run(async () => {
      if (file.size > MAX_CREDENTIAL_BYTES)
        throw new Error("凭证文件过大（上限 1 MiB）");
      let credential: unknown;
      try {
        credential = JSON.parse(await file.text());
      } catch {
        throw new Error("凭证文件不是合法的 JSON");
      }
      const account = await actions.client.importAccount(credential);
      return `已导入账号 ${account.user}`;
    });
  }

  return (
    <>
      <div className="toolbar">
        <p>额度数据来自 GitHub，由服务端按配置的间隔（默认 1 小时）在后台自动更新，点击「刷新额度」可立即刷新；账号列表每 15 秒自动刷新。</p>
        <div>
          <label className="button">
            导入凭证 JSON
            <input
              type="file"
              accept="application/json,.json"
              hidden
              disabled={actions.busy}
              onChange={(e) => {
                const file = e.target.files?.[0];
                e.target.value = "";
                if (file) importCredential(file);
              }}
            />
          </label>
          <button
            className="primary"
            disabled={actions.busy || loginPending}
            onClick={onStartLogin}
          >
            ＋ GitHub 登录
          </button>
        </div>
      </div>
      <section className="account-grid">
        {accounts.map((a) => (
          <AccountCard key={a.id} account={a} actions={actions} />
        ))}
      </section>
      {accounts.length === 0 && (
        <Empty
          title="添加你的第一个 Copilot 账号"
          text="使用 GitHub 设备登录，或导入已有的 Copilot 凭证 JSON 文件。"
        />
      )}
    </>
  );
}
