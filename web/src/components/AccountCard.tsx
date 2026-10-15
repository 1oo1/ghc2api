// 单个账号的卡片：状态、额度、请求统计、优先级与操作按钮。
import { useState } from "react";
import type { AccountView } from "../api";
import type { Actions } from "../actions";
import {
  accountStatus,
  formatTime,
  hostLabel,
  MAX_PRIORITY,
  MIN_PRIORITY,
  parsePriority,
  statusLabel,
} from "../account";
import { buckets, resetLabel, usedPercent, type Bucket, type Quota } from "../quota";

/** 已用比例达到该值时进度条改用警示色。 */
const HIGH_USAGE_PERCENT = 90;

export function AccountCard({
  account,
  actions,
}: {
  account: AccountView;
  actions: Actions;
}) {
  const status = accountStatus(account);
  const { busy, client, run } = actions;
  return (
    <article className="account-card">
      <header className="account-heading">
        <span className="avatar">{account.user.slice(0, 2).toUpperCase()}</span>
        <div>
          <h2>{account.user}</h2>
          <small>{hostLabel(account.host)}</small>
        </div>
        <span className={`badge ${status}`}>{statusLabel[status]}</span>
      </header>
      <QuotaPanel quota={account.quota} quotaError={account.quota_error} />
      <p className="stats-line">
        请求 {account.stats.requests.toLocaleString("zh-CN")} · 错误{" "}
        {account.stats.errors.toLocaleString("zh-CN")} · 最近使用{" "}
        {formatTime(account.stats.last_used_at)}
        {status === "cooling" &&
          ` · 冷却至 ${formatTime(account.cooldown_until)}`}
      </p>
      {/* 以服务端的优先级为 key：优先级变化后重新挂载，输入框随之显示新值。 */}
      <PriorityEditor key={account.priority} account={account} actions={actions} />
      <footer>
        <button
          disabled={busy}
          onClick={() =>
            void run(async () => {
              const view = await client.refreshQuota(account.id);
              if (view.quota_error)
                throw new Error(`额度刷新失败：${view.quota_error}`);
              return `${account.user} 额度已刷新`;
            })
          }
        >
          刷新额度
        </button>
        <button
          disabled={busy}
          onClick={() =>
            void run(async () => {
              await client.setDisabled(account.id, !account.disabled);
              return `${account.user} 已${account.disabled ? "启用" : "禁用"}`;
            })
          }
        >
          {account.disabled ? "启用" : "禁用"}
        </button>
        <button
          className="danger"
          disabled={busy}
          onClick={() => {
            if (!window.confirm(`删除账号 ${account.user}？其凭证文件也会被删除。`))
              return;
            void run(async () => {
              await client.deleteAccount(account.id);
              return `已删除 ${account.user}`;
            });
          }}
        >
          删除
        </button>
      </footer>
    </article>
  );
}

/** 优先级编辑：输入新值后保存。输入非法或与当前值相同时不能保存。 */
function PriorityEditor({
  account,
  actions,
}: {
  account: AccountView;
  actions: Actions;
}) {
  const { busy, client, run } = actions;
  const [text, setText] = useState(String(account.priority));
  const value = parsePriority(text);
  const changed = value !== null && value !== account.priority;

  return (
    <form
      className="priority-editor"
      onSubmit={(e) => {
        e.preventDefault();
        if (value === null || !changed) return;
        void run(async () => {
          await client.setPriority(account.id, value);
          return `${account.user} 优先级已设为 ${value}`;
        });
      }}
    >
      <label>
        优先级
        <input
          type="number"
          min={MIN_PRIORITY}
          max={MAX_PRIORITY}
          value={text}
          disabled={busy}
          aria-invalid={value === null}
          onChange={(e) => setText(e.target.value)}
        />
      </label>
      <button type="submit" disabled={busy || !changed}>
        保存
      </button>
      {value === null && (
        <p className="hint error">
          优先级必须是 {MIN_PRIORITY} 到 {MAX_PRIORITY} 之间的整数
        </p>
      )}
    </form>
  );
}

/** 额度区域：订阅计划、各额度窗口进度条、Credits 用量与各类提示。 */
function QuotaPanel({
  quota,
  quotaError,
}: {
  quota?: Quota;
  quotaError?: string;
}) {
  const windows = quota ? buckets(quota) : [];
  const credits = quota?.summary?.filter((m) => m.key === "credits_used") ?? [];
  return (
    <section className="quota">
      <strong>
        {quota?.subscription?.plan || quota?.subscription?.tierName || "GitHub Copilot"}
      </strong>
      {quota?.stale && (
        <p className="hint">
          额度数据可能已过期：GitHub 限流，显示的是上一次成功获取的结果。
        </p>
      )}
      {quotaError && <p className="hint error">额度查询失败：{quotaError}</p>}
      {windows.map((bucket, i) => (
        <QuotaRow key={`${bucket.window}-${i}`} bucket={bucket} />
      ))}
      {credits.map((m) => (
        <p className="credits" key={m.key}>
          已用 Credits：{m.value.toLocaleString("zh-CN")}
        </p>
      ))}
      {windows.length === 0 && credits.length === 0 && (
        <p className="muted">暂无额度数据，可点击「刷新额度」。</p>
      )}
    </section>
  );
}

function QuotaRow({ bucket }: { bucket: Bucket }) {
  const used = usedPercent(bucket.remainingFraction);
  const reset = resetLabel(bucket.resetTime);
  return (
    <div className="quota-row">
      <div>
        <span>{bucket.description ?? bucket.window}</span>
        <b>已用 {used}%</b>
      </div>
      <div className="meter">
        <i className={used >= HIGH_USAGE_PERCENT ? "high" : ""} style={{ width: `${used}%` }} />
      </div>
      {reset && <small>{reset}</small>}
    </div>
  );
}
