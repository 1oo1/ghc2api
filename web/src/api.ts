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
  /** 账号的 SOCKS5 代理（不含密码）；直连时不存在。 */
  proxy?: ProxyView;
};

/** 账号代理信息，与服务端 ProxyView 一致。 */
export type ProxyView = {
  scheme: string;
  /** IPv6 主机不带方括号。 */
  host: string;
  port: number;
  username?: string;
  has_password: boolean;
};

/** PATCH 账号时的代理部分：proxy 为空串表示删除代理；proxy_keep_password 为 true 时沿用当前密码。 */
export type ProxyPatch = {
  proxy: string;
  proxy_keep_password?: boolean;
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

/** 配置文件快照，与服务端 ConfigSnapshot 一致。 */
export type ConfigSnapshot = {
  path: string;
  content: string;
  /** 内容的 sha256，保存时用于检测文件是否已被外部修改。 */
  revision: string;
  /** 配置文件不可写（如只读挂载）时为 false。 */
  writable: boolean;
  /** 文件内容中与进程启动值不同、需重启才生效的字段。 */
  restart_required: string[];
};

/** 配置校验结果；无效配置同样是 HTTP 200，valid 为 false。 */
export type ConfigValidation = {
  valid: boolean;
  error?: string;
  /** 错误所在行（从 1 起），无法定位时不存在。 */
  line?: number;
  restart_required: string[];
};

/** 保存配置的结果：changed 为已热更新生效的字段，restart_required 为需重启生效的字段。 */
export type ConfigSaveResult = {
  revision: string;
  changed: string[];
  restart_required: string[];
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
  /** 发送请求；非 2xx 时抛 ApiError（错误体应为 JSON，解析失败时用默认文案）。 */
  async function send(
    path: string,
    method = "GET",
    body?: unknown,
  ): Promise<Response> {
    const response = await fetch(`/v0/management/${path}`, {
      method,
      headers: {
        Authorization: `Bearer ${credential}`,
        ...(body === undefined ? {} : { "Content-Type": "application/json" }),
      },
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    if (!response.ok) {
      const data = await response.json().catch(() => ({}));
      throw new ApiError(
        data?.error?.message ?? `请求失败（HTTP ${response.status}）`,
        response.status,
      );
    }
    return response;
  }

  async function request<T>(
    path: string,
    method = "GET",
    body?: unknown,
  ): Promise<T> {
    const response = await send(path, method, body);
    return (await response.json().catch(() => ({}))) as T;
  }

  return {
    /** 签发新的控制台令牌：凭据为管理密钥时即登录，为令牌时即续期。 */
    createSession: () => request<ConsoleSession>("session", "POST"),
    /** 服务端程序版本，例如 v0.0.1。 */
    getVersion: () => request<{ version: string }>("version"),
    listAccounts: () => request<AccountView[]>("accounts"),
    /** proxy 非空时写入 credential.proxy，覆盖凭证中原有的 proxy。 */
    importAccount: (credential: unknown, proxy?: string) =>
      request<AccountView>("accounts", "POST", {
        credential:
          proxy && typeof credential === "object" && credential !== null && !Array.isArray(credential)
            ? { ...credential, proxy }
            : credential,
      }),
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
    /** 设置、修改或删除（proxy 为空串）账号代理；设置时服务端会先通过新代理验证。 */
    setProxy: (id: string, patch: ProxyPatch) =>
      request<AccountView>("accounts", "PATCH", { id, ...patch }),
    refreshQuota: (id: string) =>
      request<AccountView>("quota/refresh", "POST", { id }),
    /** proxy 非空时设备登录全程经该代理，并保存到新账号的凭证中。 */
    startLogin: (proxy?: string) =>
      request<LoginStart>("login", "POST", proxy ? { proxy } : undefined),
    loginStatus: (state: string) =>
      request<LoginStatus>(
        `login/status?state=${encodeURIComponent(state)}`,
      ),
    getConfig: () => request<ConfigSnapshot>("config"),
    /** 由服务端校验配置内容（与启动时相同的规则）。 */
    validateConfig: (content: string) =>
      request<ConfigValidation>("config/validate", "POST", { content }),
    /** revision 为读取时得到的值；文件已被修改时服务端返回 409。 */
    saveConfig: (content: string, revision: string) =>
      request<ConfigSaveResult>("config", "PUT", { content, revision }),
    /** 读取日志尾部原始文本（NDJSON）；maxBytes 缺省时不带参数（服务端默认 8 MiB）。 */
    getLogs: async (
      maxBytes?: number,
    ): Promise<{ text: string; truncated: boolean }> => {
      const response = await send(
        maxBytes === undefined ? "logs" : `logs?max_bytes=${maxBytes}`,
      );
      return {
        text: await response.text(),
        truncated: response.headers.get("X-Log-Truncated") === "true",
      };
    },
  };
}
