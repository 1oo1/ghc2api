// 管理接口客户端：封装 /v0/management 下的请求与错误处理，接口定义见 docs/management.md。
import type { Quota } from "./quota";

/** 账号视图，与服务端 AccountView 的 JSON 结构一致。 */
export type AccountView = {
  id: string;
  /** GitHub 用户名。 */
  user: string;
  /** 空表示 github.com。 */
  host: string;
  disabled: boolean;
  /** 优先级，范围 -1 到 100；数值越大越优先被选择。 */
  priority: number;
  /** 仅在冷却中时存在。 */
  cooldown_until?: string;
  /** 仅在冷却中时存在：触发冷却的状态码（如 "403"），网络错误为 "network"。 */
  cooldown_reason?: string;
  stats: {
    requests: number;
    errors: number;
    last_used_at?: string;
  };
  quota?: Quota;
  /** 最近一次额度刷新失败的原因；旧数据仍可用时出现。 */
  quota_error?: string;
};

/** 发起设备登录后的返回值。 */
export type LoginStart = {
  state: string;
  url: string;
  user_code: string;
  /** 轮询间隔（秒）。 */
  interval: number;
  /** 授权有效期（秒）。 */
  expires_in: number;
};

/** 设备登录轮询结果。 */
export type LoginStatus = {
  status: "pending" | "success" | "error";
  id?: string;
  error?: string;
};

/** 控制台令牌：由管理密钥换取，或用未过期的令牌续期。 */
export type ConsoleSession = {
  token: string;
  /** RFC3339 过期时间。 */
  expires_at: string;
};

/** 服务端返回的非 2xx 错误，保留 HTTP 状态码便于区分处理。 */
export class ApiError extends Error {
  readonly status: number;

  constructor(message: string, status: number) {
    super(message);
    this.name = "ApiError";
    this.status = status;
  }
}

/** 是否为 401（管理密钥无效或已失效）。 */
export function isUnauthorized(error: unknown): boolean {
  return error instanceof ApiError && error.status === 401;
}

/** 取出可展示的错误文字。 */
export function errorMessage(error: unknown, fallback = "操作失败"): string {
  return error instanceof Error ? error.message : fallback;
}

export type ManagementClient = ReturnType<typeof createManagementClient>;

/**
 * 使用给定的凭据（管理密钥或控制台令牌）创建客户端。
 * 凭据只放在请求头中发送，本模块不做任何持久化。
 */
export function createManagementClient(credential: string) {
  async function request<T>(
    path: string,
    method = "GET",
    body?: unknown,
  ): Promise<T> {
    const response = await fetch(`/v0/management/${path}`, {
      method,
      headers: {
        Authorization: `Bearer ${credential}`,
        ...(body === undefined ? {} : { "Content-Type": "application/json" }),
      },
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    const data = await response.json().catch(() => ({}));
    if (!response.ok) {
      throw new ApiError(
        data?.error?.message ?? `请求失败（HTTP ${response.status}）`,
        response.status,
      );
    }
    return data as T;
  }

  return {
    /** 签发新的控制台令牌：凭据为管理密钥时即登录，为令牌时即续期。 */
    createSession: () => request<ConsoleSession>("session", "POST"),
    /** 服务端程序版本，例如 v0.0.1。 */
    getVersion: () => request<{ version: string }>("version"),
    listAccounts: () => request<AccountView[]>("accounts"),
    importAccount: (credential: unknown) =>
      request<AccountView>("accounts", "POST", { credential }),
    setDisabled: (id: string, disabled: boolean) =>
      request<AccountView>("accounts", "PATCH", { id, disabled }),
    setPriority: (id: string, priority: number) =>
      request<AccountView>("accounts", "PATCH", { id, priority }),
    /** 立即解除冷却，计数清零；返回服务端的账号视图。 */
    clearCooldown: (id: string) =>
      request<AccountView>("accounts", "PATCH", { id, clear_cooldown: true }),
    deleteAccount: (id: string) =>
      request<{ ok: boolean }>(
        `accounts?id=${encodeURIComponent(id)}`,
        "DELETE",
      ),
    refreshQuota: (id: string) =>
      request<AccountView>("quota/refresh", "POST", { id }),
    startLogin: () => request<LoginStart>("login", "POST"),
    loginStatus: (state: string) =>
      request<LoginStatus>(
        `login/status?state=${encodeURIComponent(state)}`,
      ),
  };
}
