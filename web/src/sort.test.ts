import { expect, test } from "vitest";
import type { AccountView } from "./api";
import type { Quota } from "./quota";
import { nextSort, sortAccounts, type SortKey, type SortState } from "./sort";

/** 构造一个账号视图，默认无统计、无额度。 */
function accountOf(id: string, overrides: Partial<AccountView> = {}): AccountView {
  return {
    id,
    user: id,
    host: "",
    disabled: false,
    priority: 0,
    stats: { requests: 0, errors: 0 },
    ...overrides,
  };
}

/** 构造带额度窗口的账号：每个 remainingFraction 对应一个窗口，分布在两个分组里以覆盖展开逻辑。 */
function withQuota(id: string, fractions: number[]): AccountView {
  const toBucket = (remainingFraction: number, i: number) => ({
    window: `w${i}`,
    remainingFraction,
  });
  const mid = Math.ceil(fractions.length / 2);
  const quota: Quota = {
    groups: [
      { buckets: fractions.slice(0, mid).map(toBucket) },
      { buckets: fractions.slice(mid).map((f, i) => toBucket(f, mid + i)) },
    ],
  };
  return accountOf(id, { quota });
}

const withRequests = (id: string, requests: number) =>
  accountOf(id, { stats: { requests, errors: 0 } });
const withErrors = (id: string, errors: number) =>
  accountOf(id, { stats: { requests: 0, errors } });
const withLastUsed = (id: string, last_used_at?: string) =>
  accountOf(id, { stats: { requests: 0, errors: 0, last_used_at } });

const ids = (accounts: AccountView[]) => accounts.map((a) => a.id);

// ---- nextSort ----

test("点击未排序的列：从降序开始", () => {
  expect(nextSort(null, "quota")).toEqual({ key: "quota", direction: "desc" });
});

test("点击其他列：切换到新列并从降序开始", () => {
  expect(nextSort({ key: "quota", direction: "asc" }, "requests")).toEqual({
    key: "requests",
    direction: "desc",
  });
  expect(nextSort({ key: "quota", direction: "desc" }, "errors")).toEqual({
    key: "errors",
    direction: "desc",
  });
});

test("再次点击降序列：变为升序", () => {
  expect(nextSort({ key: "lastUsed", direction: "desc" }, "lastUsed")).toEqual({
    key: "lastUsed",
    direction: "asc",
  });
});

test("再次点击升序列：恢复默认顺序", () => {
  expect(nextSort({ key: "priority", direction: "asc" }, "priority")).toBeNull();
});

test("连续点击同一列：降序、升序、默认循环", () => {
  let state: SortState = null;
  state = nextSort(state, "errors");
  expect(state).toEqual({ key: "errors", direction: "desc" });
  state = nextSort(state, "errors");
  expect(state).toEqual({ key: "errors", direction: "asc" });
  state = nextSort(state, "errors");
  expect(state).toBeNull();
  state = nextSort(state, "errors");
  expect(state).toEqual({ key: "errors", direction: "desc" });
});

// ---- sortAccounts：基本行为 ----

test("sort 为 null 时保持原顺序，且返回新数组", () => {
  const accounts = [withRequests("b", 1), withRequests("a", 9), withRequests("c", 5)];
  const result = sortAccounts(accounts, null);
  expect(ids(result)).toEqual(["b", "a", "c"]);
  expect(result).not.toBe(accounts);
});

test("空数组返回空数组", () => {
  expect(sortAccounts([], { key: "requests", direction: "desc" })).toEqual([]);
});

test("排序不修改入参数组的顺序", () => {
  const accounts = [withRequests("b", 1), withRequests("a", 9), withRequests("c", 5)];
  const snapshot = [...accounts];
  const result = sortAccounts(accounts, { key: "requests", direction: "desc" });
  expect(result).not.toBe(accounts);
  expect(ids(result)).toEqual(["a", "c", "b"]);
  expect(accounts).toEqual(snapshot);
  expect(ids(accounts)).toEqual(["b", "a", "c"]);
});

// ---- requests / errors / priority ----

test("按请求数排序：降序从大到小，升序从小到大", () => {
  const accounts = [withRequests("a", 5), withRequests("b", 20), withRequests("c", 1)];
  expect(ids(sortAccounts(accounts, { key: "requests", direction: "desc" }))).toEqual([
    "b",
    "a",
    "c",
  ]);
  expect(ids(sortAccounts(accounts, { key: "requests", direction: "asc" }))).toEqual([
    "c",
    "a",
    "b",
  ]);
});

test("按错误数排序：降序从大到小，升序从小到大", () => {
  const accounts = [withErrors("a", 3), withErrors("b", 0), withErrors("c", 10)];
  expect(ids(sortAccounts(accounts, { key: "errors", direction: "desc" }))).toEqual([
    "c",
    "a",
    "b",
  ]);
  expect(ids(sortAccounts(accounts, { key: "errors", direction: "asc" }))).toEqual([
    "b",
    "a",
    "c",
  ]);
});

test("按优先级排序：含 -1 与 0，值总是存在", () => {
  const accounts = [
    accountOf("a", { priority: 0 }),
    accountOf("b", { priority: 100 }),
    accountOf("c", { priority: -1 }),
    accountOf("d", { priority: 50 }),
  ];
  expect(ids(sortAccounts(accounts, { key: "priority", direction: "desc" }))).toEqual([
    "b",
    "d",
    "a",
    "c",
  ]);
  expect(ids(sortAccounts(accounts, { key: "priority", direction: "asc" }))).toEqual([
    "c",
    "a",
    "d",
    "b",
  ]);
});

test("数值为 0 的账号参与排序，不被当作缺失值", () => {
  const accounts = [withRequests("zero", 0), withRequests("one", 1)];
  expect(ids(sortAccounts(accounts, { key: "requests", direction: "asc" }))).toEqual([
    "zero",
    "one",
  ]);
  expect(ids(sortAccounts(accounts, { key: "requests", direction: "desc" }))).toEqual([
    "one",
    "zero",
  ]);
});

// ---- lastUsed ----

test("按最近使用时间排序：降序最近的在前，升序最早的在前", () => {
  const accounts = [
    withLastUsed("a", "2026-10-20T08:00:00Z"),
    withLastUsed("b", "2026-10-22T08:00:00Z"),
    withLastUsed("c", "2026-10-21T08:00:00Z"),
  ];
  expect(ids(sortAccounts(accounts, { key: "lastUsed", direction: "desc" }))).toEqual([
    "b",
    "c",
    "a",
  ]);
  expect(ids(sortAccounts(accounts, { key: "lastUsed", direction: "asc" }))).toEqual([
    "a",
    "c",
    "b",
  ]);
});

test("最近使用时间按时间戳比较，而不是按字符串", () => {
  // 字符串比较会得到相反结果：+08:00 的 "2026-10-22T01:00" 实际早于 UTC 的 "2026-10-21T20:00"。
  const accounts = [
    withLastUsed("a", "2026-10-22T01:00:00+08:00"),
    withLastUsed("b", "2026-10-21T20:00:00Z"),
  ];
  expect(ids(sortAccounts(accounts, { key: "lastUsed", direction: "desc" }))).toEqual([
    "b",
    "a",
  ]);
});

test("最近使用时间缺失或无效时视为缺失值，升序降序都排在最后", () => {
  const accounts = [
    withLastUsed("missing"),
    withLastUsed("early", "2026-10-20T08:00:00Z"),
    withLastUsed("invalid", "not-a-time"),
    withLastUsed("late", "2026-10-22T08:00:00Z"),
    withLastUsed("empty", ""),
  ];
  expect(ids(sortAccounts(accounts, { key: "lastUsed", direction: "desc" }))).toEqual([
    "late",
    "early",
    "missing",
    "invalid",
    "empty",
  ]);
  expect(ids(sortAccounts(accounts, { key: "lastUsed", direction: "asc" }))).toEqual([
    "early",
    "late",
    "missing",
    "invalid",
    "empty",
  ]);
});

// ---- quota ----

test("额度取已用比例最高的窗口，即最小的 remainingFraction", () => {
  // A 有 0.9 和 0.1，最高已用为 90%；B 只有 0.5，已用 50%。
  const a = withQuota("A", [0.9, 0.1]);
  const b = withQuota("B", [0.5]);
  expect(ids(sortAccounts([b, a], { key: "quota", direction: "desc" }))).toEqual(["A", "B"]);
  expect(ids(sortAccounts([a, b], { key: "quota", direction: "asc" }))).toEqual(["B", "A"]);
});

test("额度窗口跨分组时取全部窗口中的最小值", () => {
  // 两个窗口分属不同分组：withQuota 会把它们分到两组。
  const a = withQuota("A", [0.8, 0.2]);
  const b = withQuota("B", [0.4, 0.6]);
  expect(ids(sortAccounts([b, a], { key: "quota", direction: "desc" }))).toEqual(["A", "B"]);
});

test("额度按已用比例排序：已用越多越靠前（降序）", () => {
  const accounts = [
    withQuota("low", [0.9]),
    withQuota("high", [0.05]),
    withQuota("mid", [0.5]),
  ];
  expect(ids(sortAccounts(accounts, { key: "quota", direction: "desc" }))).toEqual([
    "high",
    "mid",
    "low",
  ]);
  expect(ids(sortAccounts(accounts, { key: "quota", direction: "asc" }))).toEqual([
    "low",
    "mid",
    "high",
  ]);
});

test("remainingFraction 为 0（已用尽）参与排序，不被当作缺失值", () => {
  const accounts = [withQuota("half", [0.5]), withQuota("empty", [0])];
  expect(ids(sortAccounts(accounts, { key: "quota", direction: "desc" }))).toEqual([
    "empty",
    "half",
  ]);
  expect(ids(sortAccounts(accounts, { key: "quota", direction: "asc" }))).toEqual([
    "half",
    "empty",
  ]);
});

test("没有 quota、buckets 为空或窗口值为 NaN 时视为缺失值，升序降序都排在最后", () => {
  const noQuota = accountOf("noQuota");
  const emptyQuota = accountOf("emptyQuota", { quota: {} });
  const emptyGroups = accountOf("emptyGroups", { quota: { groups: [{ buckets: [] }, {}] } });
  const nanOnly = accountOf("nanOnly", {
    quota: { groups: [{ buckets: [{ window: "w", remainingFraction: NaN }] }] },
  });
  const high = withQuota("high", [0.1]);
  const low = withQuota("low", [0.8]);
  const accounts = [noQuota, high, emptyQuota, low, emptyGroups, nanOnly];
  const missing = ["noQuota", "emptyQuota", "emptyGroups", "nanOnly"];

  expect(ids(sortAccounts(accounts, { key: "quota", direction: "desc" }))).toEqual([
    "high",
    "low",
    ...missing,
  ]);
  expect(ids(sortAccounts(accounts, { key: "quota", direction: "asc" }))).toEqual([
    "low",
    "high",
    ...missing,
  ]);
});

test("NaN 窗口被过滤后，账号按剩余的有效窗口排序", () => {
  const mixed = accountOf("mixed", {
    quota: {
      groups: [
        {
          buckets: [
            { window: "bad", remainingFraction: NaN },
            { window: "ok", remainingFraction: 0.3 },
          ],
        },
      ],
    },
  });
  const other = withQuota("other", [0.5]);
  expect(ids(sortAccounts([other, mixed], { key: "quota", direction: "desc" }))).toEqual([
    "mixed",
    "other",
  ]);
});

// ---- 缺失值与稳定性 ----

test("缺失值之间保持原顺序，升序降序都一样", () => {
  const accounts = [
    withLastUsed("m1"),
    withLastUsed("v1", "2026-10-21T00:00:00Z"),
    withLastUsed("m2"),
    withLastUsed("v2", "2026-10-22T00:00:00Z"),
    withLastUsed("m3"),
  ];
  expect(ids(sortAccounts(accounts, { key: "lastUsed", direction: "desc" }))).toEqual([
    "v2",
    "v1",
    "m1",
    "m2",
    "m3",
  ]);
  expect(ids(sortAccounts(accounts, { key: "lastUsed", direction: "asc" }))).toEqual([
    "v1",
    "v2",
    "m1",
    "m2",
    "m3",
  ]);
});

test("全部为缺失值时保持原顺序", () => {
  const accounts = [accountOf("c"), accountOf("a"), accountOf("b")];
  for (const direction of ["asc", "desc"] as const) {
    expect(ids(sortAccounts(accounts, { key: "quota", direction }))).toEqual(["c", "a", "b"]);
    expect(ids(sortAccounts(accounts, { key: "lastUsed", direction }))).toEqual(["c", "a", "b"]);
  }
});

test("值相等时保持原顺序：降序和升序都是稳定排序", () => {
  const accounts = [
    withRequests("t1", 5),
    withRequests("big", 9),
    withRequests("t2", 5),
    withRequests("small", 1),
    withRequests("t3", 5),
  ];
  expect(ids(sortAccounts(accounts, { key: "requests", direction: "desc" }))).toEqual([
    "big",
    "t1",
    "t2",
    "t3",
    "small",
  ]);
  expect(ids(sortAccounts(accounts, { key: "requests", direction: "asc" }))).toEqual([
    "small",
    "t1",
    "t2",
    "t3",
    "big",
  ]);
});

test("每一列的相等值都保持原顺序", () => {
  const sameQuota = [withQuota("x", [0.5]), withQuota("y", [0.5]), withQuota("z", [0.5])];
  const sameTime = ["x", "y", "z"].map((id) => withLastUsed(id, "2026-10-22T00:00:00Z"));
  const sameErrors = ["x", "y", "z"].map((id) => withErrors(id, 2));
  const samePriority = ["x", "y", "z"].map((id) => accountOf(id, { priority: 7 }));
  const cases: [SortKey, AccountView[]][] = [
    ["quota", sameQuota],
    ["lastUsed", sameTime],
    ["errors", sameErrors],
    ["priority", samePriority],
  ];
  for (const [key, accounts] of cases) {
    for (const direction of ["asc", "desc"] as const) {
      expect(ids(sortAccounts(accounts, { key, direction }))).toEqual(["x", "y", "z"]);
    }
  }
});

test("额度的最小值相同时保持原顺序，与其他窗口无关", () => {
  const accounts = [withQuota("a", [0.9, 0.3]), withQuota("b", [0.3]), withQuota("c", [0.3, 0.7])];
  for (const direction of ["asc", "desc"] as const) {
    expect(ids(sortAccounts(accounts, { key: "quota", direction }))).toEqual(["a", "b", "c"]);
  }
});
