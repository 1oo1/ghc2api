// 管理接口客户端的解除冷却请求。新增的方法：
//   - ManagementClient.clearCooldown(id: string)：发送 PATCH /v0/management/accounts，请求体为 {"id": id, "clear_cooldown": true}，
//     返回服务端的账号视图。
import { afterEach, expect, test, vi } from "vitest";
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
