// 管理接口客户端的补充测试。覆盖的方法：
//   - ManagementClient.clearCooldown(id: string)：发送 PATCH /v0/management/accounts，请求体为 {"id": id, "clear_cooldown": true}，
//     返回服务端的账号视图。
//   - ManagementClient.setProxy(id: string, patch: ProxyPatch)：发送 PATCH /v0/management/accounts，请求体为 { id, ...patch }，
//     ProxyPatch = { proxy: string; proxy_keep_password?: boolean }（见 docs/proxy.md）；proxy 为空串表示删除代理。
//   - ManagementClient.importAccount(credential: unknown, proxy?: string)：proxy 非空时写入请求体的 credential.proxy（覆盖凭证中原有的 proxy），
//     未提供或为空串时凭证原样发送。
//   - ManagementClient.startLogin(proxy?: string)：proxy 非空时请求体为 { proxy }，否则不带请求体。
//   - ManagementClient.getLogs(maxBytes?: number)：GET /v0/management/logs（有 maxBytes 时为 logs?max_bytes=<n>），
//     返回 { text, truncated }，truncated 取自响应头 X-Log-Truncated === "true"；非 2xx 抛 ApiError。
//   - ManagementClient.createSession()：发送 POST /v0/management/session，携带管理凭证或控制台令牌，
//     返回 { token, expires_at }（ConsoleSession）。
import { afterEach, expect, test, vi } from "vitest";
import type { ConsoleSession } from "./api";
import { ApiError, createManagementClient } from "./api";

afterEach(() => {
  vi.unstubAllGlobals();
});

/** 替换全局 fetch，返回记录调用的桩函数。 */
function stubFetch(status: number, body: unknown) {
  const fetchMock = vi.fn(
    async () =>
      new Response(JSON.stringify(body), {
        status,
        headers: { "Content-Type": "application/json" },
      }),
  );
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

test("clearCooldown 发送 PATCH，请求体含 clear_cooldown 且携带管理密钥", async () => {
  const view = { id: "alice", user: "alice", host: "", disabled: false, priority: 0, stats: { requests: 1, errors: 1 } };
  const fetchMock = stubFetch(200, view);

  await createManagementClient("mgmt-key").clearCooldown("alice");

  expect(fetchMock).toHaveBeenCalledTimes(1);
  const [url, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit];
  expect(url).toBe("/v0/management/accounts");
  expect(init.method).toBe("PATCH");
  expect((init.headers as Record<string, string>).Authorization).toBe("Bearer mgmt-key");
  expect(JSON.parse(init.body as string)).toEqual({ id: "alice", clear_cooldown: true });
});

test("clearCooldown 返回服务端的账号视图", async () => {
  stubFetch(200, { id: "alice", user: "alice", host: "", disabled: false, priority: 0, stats: { requests: 0, errors: 0 } });
  await expect(createManagementClient("mgmt-key").clearCooldown("alice")).resolves.toMatchObject({ id: "alice" });
});

test("clearCooldown 的错误保留服务端消息与状态码", async () => {
  stubFetch(404, { error: { message: "账号不存在" } });
  const error = await createManagementClient("mgmt-key").clearCooldown("nobody").catch((e: unknown) => e);
  expect(error).toBeInstanceOf(ApiError);
  expect((error as ApiError).status).toBe(404);
  expect((error as ApiError).message).toBe("账号不存在");
});

test("createSession 发送 POST /v0/management/session，携带凭证且无请求体", async () => {
  const session: ConsoleSession = { token: "token-123", expires_at: "2026-11-06T12:00:00Z" };
  const fetchMock = stubFetch(200, session);

  await createManagementClient("mgmt-key").createSession();

  expect(fetchMock).toHaveBeenCalledTimes(1);
  const [url, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit];
  expect(url).toBe("/v0/management/session");
  expect(init.method).toBe("POST");
  expect((init.headers as Record<string, string>).Authorization).toBe("Bearer mgmt-key");
  expect(init.body).toBeUndefined();
});

test("createSession 也接受控制台令牌作为凭证", async () => {
  const session: ConsoleSession = { token: "token-123", expires_at: "2026-11-06T12:00:00Z" };
  const fetchMock = stubFetch(200, session);

  await createManagementClient("token-123").createSession();

  const [, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit];
  expect((init.headers as Record<string, string>).Authorization).toBe(
    "Bearer token-123",
  );
});

test("createSession 返回服务端的令牌与过期时间", async () => {
  stubFetch(200, {
    token: "token-123",
    expires_at: "2026-11-06T12:00:00Z",
  });
  await expect(
    createManagementClient("mgmt-key").createSession(),
  ).resolves.toEqual({
    token: "token-123",
    expires_at: "2026-11-06T12:00:00Z",
  });
});

test("createSession 的错误保留服务端消息与状态码", async () => {
  stubFetch(401, { error: { message: "管理密钥无效或缺失" } });
  const error = await createManagementClient("bad")
    .createSession()
    .catch((e: unknown) => e);
  expect(error).toBeInstanceOf(ApiError);
  expect((error as ApiError).status).toBe(401);
  expect((error as ApiError).message).toBe("管理密钥无效或缺失");
});

test("setProxy 发送 PATCH，请求体为 id 加 proxy，不带 proxy_keep_password", async () => {
  const fetchMock = stubFetch(200, { id: "alice" });

  await createManagementClient("mgmt-key").setProxy("alice", { proxy: "socks5://127.0.0.1:1080" });

  const [url, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit];
  expect(url).toBe("/v0/management/accounts");
  expect(init.method).toBe("PATCH");
  expect((init.headers as Record<string, string>).Authorization).toBe("Bearer mgmt-key");
  expect(JSON.parse(init.body as string)).toEqual({ id: "alice", proxy: "socks5://127.0.0.1:1080" });
});

test("setProxy 带 proxy_keep_password 时原样放入请求体", async () => {
  const fetchMock = stubFetch(200, { id: "alice" });

  await createManagementClient("mgmt-key").setProxy("alice", {
    proxy: "socks5://user@127.0.0.1:1080",
    proxy_keep_password: true,
  });

  const [, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit];
  expect(JSON.parse(init.body as string)).toEqual({
    id: "alice",
    proxy: "socks5://user@127.0.0.1:1080",
    proxy_keep_password: true,
  });
});

test("setProxy 用空串删除代理，请求体保留 proxy: \"\"", async () => {
  const fetchMock = stubFetch(200, { id: "alice" });

  await createManagementClient("mgmt-key").setProxy("alice", { proxy: "" });

  const [, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit];
  expect(JSON.parse(init.body as string)).toEqual({ id: "alice", proxy: "" });
});

test("setProxy 返回服务端的账号视图", async () => {
  const view = {
    id: "alice",
    user: "alice",
    host: "",
    disabled: false,
    priority: 0,
    stats: { requests: 0, errors: 0 },
    proxy: { scheme: "socks5", host: "127.0.0.1", port: 1080, has_password: false },
  };
  stubFetch(200, view);
  await expect(
    createManagementClient("mgmt-key").setProxy("alice", { proxy: "socks5://127.0.0.1:1080" }),
  ).resolves.toEqual(view);
});

test("setProxy 的错误保留服务端消息与状态码（验证失败为 502）", async () => {
  stubFetch(502, { error: { message: "代理 127.0.0.1:1080 连接失败: connection refused" } });
  const error = await createManagementClient("mgmt-key")
    .setProxy("alice", { proxy: "socks5://127.0.0.1:1080" })
    .catch((e: unknown) => e);
  expect(error).toBeInstanceOf(ApiError);
  expect((error as ApiError).status).toBe(502);
  expect((error as ApiError).message).toBe("代理 127.0.0.1:1080 连接失败: connection refused");
});

test("importAccount 带代理时写入 credential.proxy，覆盖凭证中原有的 proxy", async () => {
  const fetchMock = stubFetch(200, { id: "alice" });

  await createManagementClient("mgmt-key").importAccount(
    { oauth_token: "gho_x", user: "alice", proxy: "socks5://old.example.com:1" },
    "socks5://new.example.com:2",
  );

  const [url, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit];
  expect(url).toBe("/v0/management/accounts");
  expect(init.method).toBe("POST");
  expect(JSON.parse(init.body as string)).toEqual({
    credential: { oauth_token: "gho_x", user: "alice", proxy: "socks5://new.example.com:2" },
  });
});

test("importAccount 没有提供代理（或为空串）时凭证原样发送", async () => {
  const fetchMock = stubFetch(200, { id: "alice" });
  const credential = { oauth_token: "gho_x", proxy: "socks5://pasted.example.com:1" };

  await createManagementClient("mgmt-key").importAccount(credential);
  await createManagementClient("mgmt-key").importAccount(credential, "");

  for (const call of fetchMock.mock.calls) {
    const [, init] = call as unknown as [string, RequestInit];
    expect(JSON.parse(init.body as string)).toEqual({ credential });
  }
});

test("startLogin 带代理时请求体为 { proxy }", async () => {
  const fetchMock = stubFetch(200, { state: "s", url: "u", user_code: "c", interval: 5, expires_in: 900 });

  await createManagementClient("mgmt-key").startLogin("socks5://u:p@127.0.0.1:1080");

  const [url, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit];
  expect(url).toBe("/v0/management/login");
  expect(init.method).toBe("POST");
  expect(JSON.parse(init.body as string)).toEqual({ proxy: "socks5://u:p@127.0.0.1:1080" });
});

test("startLogin 没有代理时不带请求体", async () => {
  const fetchMock = stubFetch(200, { state: "s", url: "u", user_code: "c", interval: 5, expires_in: 900 });

  await createManagementClient("mgmt-key").startLogin();
  await createManagementClient("mgmt-key").startLogin("");

  for (const call of fetchMock.mock.calls) {
    const [, init] = call as unknown as [string, RequestInit];
    expect(init.method).toBe("POST");
    expect(init.body).toBeUndefined();
  }
});

/** 替换全局 fetch，返回纯文本响应（日志接口的 NDJSON 正文）。 */
function stubTextFetch(status: number, text: string, headers: Record<string, string> = {}) {
  const fetchMock = vi.fn(async () => new Response(text, { status, headers }));
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

test("getLogs 不带 maxBytes 时请求 /v0/management/logs 且携带 Authorization", async () => {
  const fetchMock = stubTextFetch(200, "", { "X-Log-Truncated": "false" });

  await createManagementClient("mgmt-key").getLogs();

  expect(fetchMock).toHaveBeenCalledTimes(1);
  const [url, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit];
  expect(url).toBe("/v0/management/logs");
  expect(init.method === undefined || init.method === "GET").toBe(true);
  expect((init.headers as Record<string, string>).Authorization).toBe("Bearer mgmt-key");
  expect(init.body).toBeUndefined();
});

test("getLogs 带 maxBytes 时 URL 为 logs?max_bytes=<n>", async () => {
  const fetchMock = stubTextFetch(200, "");

  await createManagementClient("mgmt-key").getLogs(2097152);

  const [url, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit];
  expect(url).toBe("/v0/management/logs?max_bytes=2097152");
  expect((init.headers as Record<string, string>).Authorization).toBe("Bearer mgmt-key");
});

test("getLogs 返回原始文本与 truncated（响应头为 true）", async () => {
  const text = '{"time":"2026-11-03T10:00:00Z","level":"INFO","msg":"a"}\n{"time":"2026-11-03T10:00:01Z","level":"INFO","msg":"b"}\n';
  stubTextFetch(200, text, { "Content-Type": "application/x-ndjson; charset=utf-8", "X-Log-Truncated": "true" });

  await expect(createManagementClient("mgmt-key").getLogs()).resolves.toEqual({ text, truncated: true });
});

test("getLogs 的 truncated 在响应头为 false 或缺失时为 false", async () => {
  stubTextFetch(200, "line\n", { "X-Log-Truncated": "false" });
  await expect(createManagementClient("mgmt-key").getLogs()).resolves.toEqual({ text: "line\n", truncated: false });

  stubTextFetch(200, "line\n");
  await expect(createManagementClient("mgmt-key").getLogs()).resolves.toEqual({ text: "line\n", truncated: false });
});

test("getLogs 的非 2xx 响应抛 ApiError，保留服务端消息与状态码", async () => {
  stubFetch(404, { error: { message: "未配置日志文件" } });
  const error = await createManagementClient("mgmt-key").getLogs().catch((e: unknown) => e);
  expect(error).toBeInstanceOf(ApiError);
  expect((error as ApiError).status).toBe(404);
  expect((error as ApiError).message).toBe("未配置日志文件");
});

test("getLogs 的 401 抛 ApiError(status 401)", async () => {
  stubFetch(401, { error: { message: "未授权" } });
  const error = await createManagementClient("bad").getLogs().catch((e: unknown) => e);
  expect(error).toBeInstanceOf(ApiError);
  expect((error as ApiError).status).toBe(401);
});

test("getLogs 的错误体缺少消息时使用默认文案", async () => {
  stubFetch(500, {});
  const error = await createManagementClient("mgmt-key").getLogs().catch((e: unknown) => e);
  expect(error).toBeInstanceOf(ApiError);
  expect((error as ApiError).status).toBe(500);
  expect((error as ApiError).message).toBe("请求失败（HTTP 500）");
});
