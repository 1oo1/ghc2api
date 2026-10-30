// 账号与额度页：工具栏（导入凭证、GitHub 登录）与账号卡片列表。
// 导入与登录前先弹出代理弹窗，可留空跳过（不使用代理）。
import { useRef, useState } from "react";
import type { AccountView } from "../api";
import type { Actions } from "../actions";
import { AccountCard } from "../components/AccountCard";
import { Empty } from "../components/Empty";
import { ProxyDialog } from "../components/ProxyDialog";
import { buildProxyUrl } from "../proxy";

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
  /** proxy 为空串表示不使用代理。 */
  onStartLogin: (proxy: string) => void;
}) {
  /** 正在弹出代理弹窗的流程；null 表示没有弹窗。 */
  const [choosing, setChoosing] = useState<"import" | "login" | null>(null);
  const fileInput = useRef<HTMLInputElement>(null);
  /** 导入流程已选的代理，供选择文件后使用。 */
  const importProxy = useRef("");

  function importCredential(file: File, proxy: string) {
    void actions.run(async () => {
      if (file.size > MAX_CREDENTIAL_BYTES)
        throw new Error("凭证文件过大（上限 1 MiB）");
      let credential: unknown;
      try {
        credential = JSON.parse(await file.text());
      } catch {
        throw new Error("凭证文件不是合法的 JSON");
      }
      const account = await actions.client.importAccount(credential, proxy);
      return `已导入账号 ${account.user}`;
    });
  }

  return (
    <>
      <div className="toolbar">
        <p>额度数据来自 GitHub，由服务端按配置的间隔（默认 1 小时）在后台自动更新，点击「刷新额度」可立即刷新；账号列表每 15 秒自动刷新。</p>
        <div>
          <button disabled={actions.busy} onClick={() => setChoosing("import")}>
            导入凭证 JSON
          </button>
          <input
            ref={fileInput}
            type="file"
            accept="application/json,.json"
            hidden
            onChange={(e) => {
              const file = e.target.files?.[0];
              e.target.value = "";
              if (file) importCredential(file, importProxy.current);
            }}
          />
          <button
            className="primary"
            disabled={actions.busy || loginPending}
            onClick={() => setChoosing("login")}
          >
            ＋ GitHub 登录
          </button>
        </div>
      </div>
      {choosing && (
        <ProxyDialog
          title={choosing === "import" ? "导入凭证：使用代理？" : "GitHub 登录：使用代理？"}
          hint={
            choosing === "import"
              ? "可选。填写后该账号的所有请求都经由此 SOCKS5 代理；留空则使用凭证 JSON 中的 proxy（若有），否则直连。"
              : "可选。填写后该账号的所有请求都经由此 SOCKS5 代理；留空则直连。"
          }
          confirmLabel="继续"
          onCancel={() => setChoosing(null)}
          onConfirm={(form) => {
            const proxy = buildProxyUrl(form);
            setChoosing(null);
            if (choosing === "login") {
              onStartLogin(proxy);
            } else {
              importProxy.current = proxy;
              // 确认按钮的点击提供了用户激活，可以直接打开文件选择框。
              fileInput.current?.click();
            }
          }}
        />
      )}
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
