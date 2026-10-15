// 管理密钥在当前标签页 sessionStorage 中的存取。只使用 sessionStorage：关闭标签页即清除，不写入 localStorage。
// 存储不可用（隐私模式、配额已满、浏览器禁用存储）时所有函数都静默忽略，登录状态退化为只在内存中保存。

/** sessionStorage 中保存管理密钥的键名。 */
export const SESSION_KEY = "ghc2api.managementKey";

/** 读取已保存的管理密钥；没有保存或存储不可用时返回空字符串。 */
export function loadSessionKey(): string {
  try {
    return sessionStorage.getItem(SESSION_KEY) ?? "";
  } catch {
    return "";
  }
}

/** 保存管理密钥，同名覆盖。 */
export function saveSessionKey(key: string): void {
  try {
    sessionStorage.setItem(SESSION_KEY, key);
  } catch {
    // 存储不可用时忽略，本次登录只在 React state 中有效。
  }
}

/** 清除已保存的管理密钥（退出登录或密钥失效时调用）。 */
export function clearSessionKey(): void {
  try {
    sessionStorage.removeItem(SESSION_KEY);
  } catch {
    // 存储不可用时忽略。
  }
}
