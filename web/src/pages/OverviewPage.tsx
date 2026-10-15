// 概览页：可用账号数、累计请求、错误数，以及各账号的请求统计表。
import type { AccountView } from "../api";
import { accountStatus, availableCount, formatTime, statusLabel, sumStats } from "../account";
import { Empty } from "../components/Empty";

export function OverviewPage({ accounts }: { accounts: AccountView[] }) {
  const now = Date.now();
  const totals = sumStats(accounts);
  const metrics: [string, string][] = [
    ["可用账号", `${availableCount(accounts, now)} / ${accounts.length}`],
    ["累计请求", totals.requests.toLocaleString("zh-CN")],
    ["错误数", totals.errors.toLocaleString("zh-CN")],
  ];

  return (
    <>
      <section className="metrics">
        {metrics.map(([label, value]) => (
          <article key={label}>
            <span>{label}</span>
            <strong>{value}</strong>
          </article>
        ))}
      </section>
      {accounts.length === 0 ? (
        <Empty
          title="还没有账号"
          text="请到「账号与额度」页导入凭证或完成 GitHub 登录。"
        />
      ) : (
        <section className="panel table-wrap">
          <table>
            <thead>
              <tr>
                <th>用户</th>
                <th>状态</th>
                <th>请求</th>
                <th>错误</th>
                <th>最近使用时间</th>
              </tr>
            </thead>
            <tbody>
              {accounts.map((a) => {
                const status = accountStatus(a, now);
                return (
                  <tr key={a.id}>
                    <td>{a.user}</td>
                    <td>
                      <span className={`badge ${status}`}>
                        {statusLabel[status]}
                      </span>
                    </td>
                    <td>{a.stats.requests.toLocaleString("zh-CN")}</td>
                    <td>{a.stats.errors.toLocaleString("zh-CN")}</td>
                    <td>{formatTime(a.stats.last_used_at)}</td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </section>
      )}
    </>
  );
}
