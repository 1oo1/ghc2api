// 控制台登录态在 localStorage 中的存取。保存的是服务端签发的控制台令牌与其过期时间，不保存管理密钥本身。
// 过期判断只用于前端提前回到登录页，真正的有效期由服务端校验。
// 存储不可用（隐私模式、配额已满、浏览器禁用存储）时所有函数都静默忽略，登录状态退化为只在内存中保存。

/** localStorage 中保存控制台令牌的键名。 */
export const STORAGE_KEY = "ghc2api.consoleSession";

/** 旧版在 sessionStorage 中保存明文管理密钥的键名，读取时顺带清除。 */
export const LEGACY_SESSION_KEY = "ghc2api.managementKey";

/** 保存控制台令牌；expiresAt 为服务端返回的 RFC3339 过期时间。同名覆盖。 */
export function saveSession(token: string, expiresAt: string): void {
  try {
    localStorage.setItem(STORAGE_KEY, JSON.stringify({ token, expiresAt }));
  } catch {
    // 存储不可用时忽略，本次登录只在 React state 中有效。
  }
}

/**
 * 读取未过期的控制台令牌；没有保存、已过期、内容损坏或存储不可用时返回空字符串。
 * 过期或损坏的记录会被删除。每次调用都会清除旧版 sessionStorage 中的明文管理密钥。
 */
export function loadSession(now = Date.now()): string {
  try {
    sessionStorage.removeItem(LEGACY_SESSION_KEY);
  } catch {
    // 存储不可用时忽略。
  }
  try {
    const raw = localStorage.getItem(STORAGE_KEY);
    if (raw === null) return "";
    const token = validToken(raw, now);
    if (!token) localStorage.removeItem(STORAGE_KEY);
    return token;
  } catch {
    return "";
  }
}

/** 清除已保存的控制台令牌（退出登录或令牌失效时调用）。 */
export function clearSession(): void {
  try {
    localStorage.removeItem(STORAGE_KEY);
  } catch {
    // 存储不可用时忽略。
  }
}

/** 解析保存的记录，格式正确且未过期时返回令牌，否则返回空字符串。 */
function validToken(raw: string, now: number): string {
  let value: unknown;
  try {
    value = JSON.parse(raw);
  } catch {
    return "";
  }
  if (typeof value !== "object" || value === null) return "";
  const { token, expiresAt } = value as Record<string, unknown>;
  if (typeof token !== "string" || typeof expiresAt !== "string") return "";
  return Date.parse(expiresAt) > now ? token : "";
}
