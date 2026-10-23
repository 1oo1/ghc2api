import { afterEach, expect, test, vi } from "vitest";
import {
  LEGACY_SESSION_KEY,
  STORAGE_KEY,
  clearSession,
  loadSession,
  saveSession,
} from "./session";

/** 内存版 Storage，只实现模块用到的方法，并记录调用便于断言。 */
function memoryStorage() {
  const data = new Map<string, string>();
  return {
    getItem: vi.fn((key: string) => data.get(key) ?? null),
    setItem: vi.fn((key: string, value: string) => {
      data.set(key, value);
    }),
    removeItem: vi.fn((key: string) => {
      data.delete(key);
    }),
  };
}

/** 所有方法都抛异常的存储，模拟隐私模式或配额满等场景。 */
function brokenStorage() {
  const fail = () => {
    throw new Error("storage unavailable");
  };
  return { getItem: fail, setItem: fail, removeItem: fail };
}

/** 固定“当前时间”，避免依赖真实时钟的过期断言出现抖动。 */
const NOW = Date.parse("2026-10-23T12:00:00Z");
/** 未过期的 RFC3339 过期时间。 */
const FUTURE = "2026-11-06T12:00:00Z";
/** 已过期的 RFC3339 过期时间。 */
const PAST = "2026-10-09T12:00:00Z";

/** 同时装载 localStorage 与 sessionStorage，返回两者便于断言。 */
function stubBothStorages() {
  const local = memoryStorage();
  const session = memoryStorage();
  vi.stubGlobal("localStorage", local);
  vi.stubGlobal("sessionStorage", session);
  return { local, session };
}

afterEach(() => {
  vi.unstubAllGlobals();
});

test("键名为 ghc2api.consoleSession 与 ghc2api.managementKey", () => {
  expect(STORAGE_KEY).toBe("ghc2api.consoleSession");
  expect(LEGACY_SESSION_KEY).toBe("ghc2api.managementKey");
});

test("保存后读取得到相同令牌", () => {
  stubBothStorages();
  saveSession("token-123", FUTURE);
  expect(loadSession(NOW)).toBe("token-123");
});

test("保存内容为 { token, expiresAt } 的 JSON，写入 localStorage[STORAGE_KEY]", () => {
  const { local } = stubBothStorages();
  saveSession("token-123", FUTURE);
  expect(local.setItem).toHaveBeenCalledWith(
    STORAGE_KEY,
    JSON.stringify({ token: "token-123", expiresAt: FUTURE }),
  );
});

test("未保存时返回空字符串", () => {
  stubBothStorages();
  expect(loadSession(NOW)).toBe("");
});

test("clearSession 后返回空字符串", () => {
  const { local } = stubBothStorages();
  saveSession("token-123", FUTURE);
  clearSession();
  expect(loadSession(NOW)).toBe("");
  expect(local.removeItem).toHaveBeenCalledWith(STORAGE_KEY);
});

test("重复保存时取最新值", () => {
  stubBothStorages();
  saveSession("old-token", FUTURE);
  saveSession("new-token", FUTURE);
  expect(loadSession(NOW)).toBe("new-token");
});

test("expiresAt 严格大于 now 时返回令牌，恰好等于 now 时视为过期", () => {
  stubBothStorages();
  const at = new Date(NOW).toISOString();
  saveSession("token-123", at);
  // Date.parse(expiresAt) > now 为严格大于：恰好到点即失效。
  expect(loadSession(NOW)).toBe("");
  saveSession("token-123", new Date(NOW + 1000).toISOString());
  expect(loadSession(NOW)).toBe("token-123");
});

test("已过期时返回空字符串并删除该项", () => {
  const { local } = stubBothStorages();
  saveSession("token-123", PAST);
  expect(loadSession(NOW)).toBe("");
  expect(local.removeItem).toHaveBeenCalledWith(STORAGE_KEY);
  expect(loadSession(NOW)).toBe("");
});

test("过期判定使用传入的 now", () => {
  stubBothStorages();
  saveSession("token-123", FUTURE);
  expect(loadSession(Date.parse(FUTURE) - 1000)).toBe("token-123");
  expect(loadSession(Date.parse(FUTURE))).toBe("");
});

test("JSON 非法时返回空字符串并删除该项", () => {
  const { local } = stubBothStorages();
  local.getItem.mockReturnValue("{not json");
  expect(loadSession(NOW)).toBe("");
  expect(local.removeItem).toHaveBeenCalledWith(STORAGE_KEY);
});

test("字段缺失时返回空字符串并删除该项", () => {
  for (const raw of ["{}", JSON.stringify({ token: "t" }), JSON.stringify({ expiresAt: FUTURE })]) {
    const { local } = stubBothStorages();
    local.getItem.mockReturnValue(raw);
    expect(loadSession(NOW)).toBe("");
    expect(local.removeItem).toHaveBeenCalledWith(STORAGE_KEY);
  }
});

test("字段类型不对时返回空字符串并删除该项", () => {
  const cases = [
    JSON.stringify({ token: 42, expiresAt: FUTURE }),
    JSON.stringify({ token: null, expiresAt: FUTURE }),
    JSON.stringify({ token: "t", expiresAt: 1893456000000 }),
    JSON.stringify({ token: "t", expiresAt: null }),
    JSON.stringify({ token: true, expiresAt: FUTURE }),
  ];
  for (const raw of cases) {
    const { local } = stubBothStorages();
    local.getItem.mockReturnValue(raw);
    expect(loadSession(NOW)).toBe("");
    expect(local.removeItem).toHaveBeenCalledWith(STORAGE_KEY);
  }
});

test("日期无法解析时返回空字符串并删除该项", () => {
  for (const raw of [
    JSON.stringify({ token: "t", expiresAt: "not-a-date" }),
    JSON.stringify({ token: "t", expiresAt: "" }),
  ]) {
    const { local } = stubBothStorages();
    local.getItem.mockReturnValue(raw);
    expect(loadSession(NOW)).toBe("");
    expect(local.removeItem).toHaveBeenCalledWith(STORAGE_KEY);
  }
});

test("存储值不是对象时返回空字符串并删除该项", () => {
  for (const raw of ["hello", "[]", "null", ""]) {
    const { local } = stubBothStorages();
    local.getItem.mockReturnValue(raw);
    expect(loadSession(NOW)).toBe("");
    expect(local.removeItem).toHaveBeenCalledWith(STORAGE_KEY);
  }
});

test("每次 loadSession 都清理 sessionStorage 中的旧版明文密钥", () => {
  const { session } = stubBothStorages();
  session.setItem(LEGACY_SESSION_KEY, "plain-secret");
  expect(loadSession(NOW)).toBe("");
  expect(session.removeItem).toHaveBeenCalledWith(LEGACY_SESSION_KEY);
  // 重新放入旧密钥后再调用，仍会被清理（每次调用都会尝试清理）。
  session.setItem(LEGACY_SESSION_KEY, "plain-secret");
  loadSession(NOW);
  expect(session.removeItem).toHaveBeenCalledTimes(2);
});

test("有会话时也会清理旧版明文密钥", () => {
  const { session } = stubBothStorages();
  session.setItem(LEGACY_SESSION_KEY, "plain-secret");
  saveSession("token-123", FUTURE);
  expect(loadSession(NOW)).toBe("token-123");
  expect(session.removeItem).toHaveBeenCalledWith(LEGACY_SESSION_KEY);
});

test("sessionStorage 不可用不影响 localStorage 的正常读取", () => {
  const local = memoryStorage();
  vi.stubGlobal("localStorage", local);
  vi.stubGlobal("sessionStorage", brokenStorage());
  saveSession("token-123", FUTURE);
  expect(() => loadSession(NOW)).not.toThrow();
  expect(loadSession(NOW)).toBe("token-123");
});

test("localStorage 不可用时仍尝试清理旧版明文密钥", () => {
  const session = memoryStorage();
  vi.stubGlobal("localStorage", brokenStorage());
  vi.stubGlobal("sessionStorage", session);
  expect(loadSession(NOW)).toBe("");
  expect(session.removeItem).toHaveBeenCalledWith(LEGACY_SESSION_KEY);
});

test("存储方法抛异常时三个函数都不抛错，load 返回空字符串", () => {
  vi.stubGlobal("localStorage", brokenStorage());
  vi.stubGlobal("sessionStorage", brokenStorage());
  expect(() => saveSession("token-123", FUTURE)).not.toThrow();
  expect(() => clearSession()).not.toThrow();
  expect(() => loadSession(NOW)).not.toThrow();
  expect(loadSession(NOW)).toBe("");
});

test("访问 localStorage 本身抛异常（存储被禁用）时不抛错", () => {
  // 部分浏览器在禁用存储时，访问全局 localStorage 即会抛出 SecurityError。
  const original = Object.getOwnPropertyDescriptor(globalThis, "localStorage");
  const session = memoryStorage();
  vi.stubGlobal("sessionStorage", session);
  Object.defineProperty(globalThis, "localStorage", {
    configurable: true,
    get() {
      throw new Error("SecurityError");
    },
  });
  try {
    expect(() => saveSession("token-123", FUTURE)).not.toThrow();
    expect(() => clearSession()).not.toThrow();
    expect(() => loadSession(NOW)).not.toThrow();
    expect(loadSession(NOW)).toBe("");
  } finally {
    if (original) {
      Object.defineProperty(globalThis, "localStorage", original);
    } else {
      Reflect.deleteProperty(globalThis, "localStorage");
    }
  }
});

test("访问 sessionStorage 本身抛异常时 load 仍读取 localStorage", () => {
  const local = memoryStorage();
  vi.stubGlobal("localStorage", local);
  const original = Object.getOwnPropertyDescriptor(globalThis, "sessionStorage");
  Object.defineProperty(globalThis, "sessionStorage", {
    configurable: true,
    get() {
      throw new Error("SecurityError");
    },
  });
  try {
    saveSession("token-123", FUTURE);
    expect(() => loadSession(NOW)).not.toThrow();
    expect(loadSession(NOW)).toBe("token-123");
  } finally {
    if (original) {
      Object.defineProperty(globalThis, "sessionStorage", original);
    } else {
      Reflect.deleteProperty(globalThis, "sessionStorage");
    }
  }
});

test("会话写入 localStorage 而不是 sessionStorage", () => {
  const { local, session } = stubBothStorages();
  saveSession("token-123", FUTURE);
  expect(loadSession(NOW)).toBe("token-123");
  // 确认确实走了 localStorage，避免用例因未调用而空转。
  expect(local.setItem).toHaveBeenCalled();
  expect(session.setItem).not.toHaveBeenCalled();
  expect(session.getItem).not.toHaveBeenCalled();
});
