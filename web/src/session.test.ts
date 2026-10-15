import { afterEach, expect, test, vi } from "vitest";
import {
  SESSION_KEY,
  clearSessionKey,
  loadSessionKey,
  saveSessionKey,
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

afterEach(() => {
  vi.unstubAllGlobals();
});

test("键名为 ghc2api.managementKey", () => {
  expect(SESSION_KEY).toBe("ghc2api.managementKey");
});

test("保存后读取得到相同密钥", () => {
  vi.stubGlobal("sessionStorage", memoryStorage());
  saveSessionKey("secret-123");
  expect(loadSessionKey()).toBe("secret-123");
});

test("未保存时返回空字符串", () => {
  vi.stubGlobal("sessionStorage", memoryStorage());
  expect(loadSessionKey()).toBe("");
});

test("clear 后返回空字符串", () => {
  vi.stubGlobal("sessionStorage", memoryStorage());
  saveSessionKey("secret-123");
  clearSessionKey();
  expect(loadSessionKey()).toBe("");
});

test("重复保存时取最新值", () => {
  vi.stubGlobal("sessionStorage", memoryStorage());
  saveSessionKey("old-key");
  saveSessionKey("new-key");
  expect(loadSessionKey()).toBe("new-key");
});

test("保存与清除都使用 SESSION_KEY 作为键名", () => {
  const storage = memoryStorage();
  vi.stubGlobal("sessionStorage", storage);
  saveSessionKey("secret-123");
  expect(storage.setItem).toHaveBeenCalledWith(SESSION_KEY, "secret-123");
  clearSessionKey();
  expect(storage.removeItem).toHaveBeenCalledWith(SESSION_KEY);
});

test("存储方法抛异常时三个函数都不抛错，load 返回空字符串", () => {
  vi.stubGlobal("sessionStorage", brokenStorage());
  expect(() => saveSessionKey("secret-123")).not.toThrow();
  expect(() => clearSessionKey()).not.toThrow();
  expect(() => loadSessionKey()).not.toThrow();
  expect(loadSessionKey()).toBe("");
});

test("访问 sessionStorage 本身抛异常（存储被禁用）时不抛错", () => {
  // 部分浏览器在禁用存储时，访问全局 sessionStorage 即会抛出 SecurityError。
  const original = Object.getOwnPropertyDescriptor(
    globalThis,
    "sessionStorage",
  );
  Object.defineProperty(globalThis, "sessionStorage", {
    configurable: true,
    get() {
      throw new Error("SecurityError");
    },
  });
  try {
    expect(() => saveSessionKey("secret-123")).not.toThrow();
    expect(() => clearSessionKey()).not.toThrow();
    expect(() => loadSessionKey()).not.toThrow();
    expect(loadSessionKey()).toBe("");
  } finally {
    if (original) {
      Object.defineProperty(globalThis, "sessionStorage", original);
    } else {
      Reflect.deleteProperty(globalThis, "sessionStorage");
    }
  }
});

test("不写入 localStorage", () => {
  // 模块只应使用 sessionStorage，localStorage 的任何方法都不应被调用。
  const session = memoryStorage();
  const local = memoryStorage();
  vi.stubGlobal("sessionStorage", session);
  vi.stubGlobal("localStorage", local);
  saveSessionKey("secret-123");
  expect(loadSessionKey()).toBe("secret-123");
  clearSessionKey();
  // 确认确实走了 sessionStorage，避免用例因未调用而空转。
  expect(session.setItem).toHaveBeenCalled();
  expect(local.setItem).not.toHaveBeenCalled();
  expect(local.getItem).not.toHaveBeenCalled();
  expect(local.removeItem).not.toHaveBeenCalled();
});
