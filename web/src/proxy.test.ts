// 账号代理表单的纯函数测试（契约见 docs/proxy.md「控制台」）。被测模块为 ./proxy，需要导出：
//   - type ProxyForm = { host: string; port: string; username: string; password: string; clearPassword: boolean }
//     表单各输入框的文本；port 是文本，因此未填写与非法输入都能表达。
//   - emptyProxyForm: ProxyForm：全部为空、clearPassword 为 false 的表单。
//   - proxyFormFromView(proxy?: ProxyView): ProxyForm：由账号当前代理预填表单（host、port、username；
//     密码框留空，clearPassword 为 false）；没有代理时等于 emptyProxyForm。ProxyView 见 ./api。
//   - buildProxyUrl(form: ProxyForm): string：拼装代理 URL。scheme 固定 socks5；host 与 port 去除首尾空白；
//     用户名、密码用 encodeURIComponent；IPv6 主机（含冒号且未带方括号）加方括号；密码为空时不含密码；host 为空返回 ""。
//   - validateProxyForm(form: ProxyForm, current?: ProxyView): string | null：返回错误文案，合法返回 null。
//     current 为账号当前代理（编辑场景），没有时为导入/登录场景。
//     填了 host 时 port 必填，且必须是 1 到 65535 的十进制整数（去除首尾空白后判断），错误文案都包含"端口"。
//     host 为空时：有 current 一律出错，文案提到"删除"（删除代理请使用「删除」按钮，不能靠清空主机保存）；
//     没有 current 时，全空合法（不使用代理），但填了用户名或密码则出错，文案提到"主机"。
//   - proxyFormChanged(form: ProxyForm, current?: ProxyView): boolean：表单相对账号当前代理是否有变化（用于保存按钮置灰）。
//     有当前代理时：host、port、username（去除首尾空白后比较）任一不同，或密码框非空，或勾选了 clearPassword 即为有变化；
//     密码框为空且未勾选清除视为未变化。没有当前代理时：host 非空才算有变化（只填端口等不构成代理）。
//   - proxyLabel(proxy?: ProxyView): string：卡片上的代理文字，无代理为"无代理"，否则为 "host:port"（IPv6 主机加方括号）。
//   - buildProxyPatch(form: ProxyForm, current?: ProxyView): ProxyPatch：构造 PATCH 请求体中的代理部分
//     （ProxyPatch 见 ./api：{ proxy: string; proxy_keep_password?: boolean }）。
//     proxy 为 buildProxyUrl(form)（host 为空时为 ""，即删除）。proxy_keep_password 为 true 当且仅当：
//     当前代理有密码、密码框为空、未勾选 clearPassword，且 host、port、username 都与当前代理相同（去除首尾空白后比较）；
//     其余情况字段省略（不是 false）。
//   - passwordPlaceholder(form: ProxyForm, current?: ProxyView): string：密码框占位文案。当前代理有密码时：
//     host、port、username 都未改变为"留空保持原密码"，任一改变为"地址或用户名已修改，需重新填写密码"；其余情况为"可选"。
import { expect, test } from "vitest";
import type { ProxyView } from "./api";
import {
  buildProxyPatch,
  buildProxyUrl,
  emptyProxyForm,
  proxyFormChanged,
  proxyFormFromView,
  proxyLabel,
  passwordPlaceholder,
  validateProxyForm,
} from "./proxy";
import type { ProxyForm } from "./proxy";

/** 有密码的当前代理。 */
const current: ProxyView = {
  scheme: "socks5",
  host: "127.0.0.1",
  port: 1080,
  username: "user",
  has_password: true,
};

function form(overrides: Partial<ProxyForm> = {}): ProxyForm {
  return { ...emptyProxyForm, host: "127.0.0.1", port: "1080", ...overrides };
}

test("emptyProxyForm 全部为空且不清除密码", () => {
  expect(emptyProxyForm).toEqual({
    host: "",
    port: "",
    username: "",
    password: "",
    clearPassword: false,
  });
});

test("proxyFormFromView 预填 host、port、username，密码框留空", () => {
  expect(proxyFormFromView(current)).toEqual({
    host: "127.0.0.1",
    port: "1080",
    username: "user",
    password: "",
    clearPassword: false,
  });
});

test("proxyFormFromView 没有用户名或没有代理时对应项为空", () => {
  expect(proxyFormFromView({ scheme: "socks5", host: "::1", port: 1081, has_password: false })).toEqual({
    host: "::1",
    port: "1081",
    username: "",
    password: "",
    clearPassword: false,
  });
  expect(proxyFormFromView(undefined)).toEqual(emptyProxyForm);
});

test("buildProxyUrl 只有主机和端口时不含用户信息，scheme 固定为 socks5", () => {
  expect(buildProxyUrl(form())).toBe("socks5://127.0.0.1:1080");
});

test("buildProxyUrl 带用户名与密码", () => {
  expect(buildProxyUrl(form({ username: "u", password: "p" }))).toBe("socks5://u:p@127.0.0.1:1080");
});

test("buildProxyUrl 只有用户名时不含密码；密码为空等同没有密码", () => {
  expect(buildProxyUrl(form({ username: "u" }))).toBe("socks5://u@127.0.0.1:1080");
  expect(buildProxyUrl(form({ username: "u", password: "" }))).toBe("socks5://u@127.0.0.1:1080");
});

test("buildProxyUrl 对用户名和密码做 encodeURIComponent", () => {
  expect(buildProxyUrl(form({ username: "a@b", password: "p:/ ?#%" }))).toBe(
    "socks5://a%40b:p%3A%2F%20%3F%23%25@127.0.0.1:1080",
  );
});

test("buildProxyUrl 给 IPv6 主机加方括号，已带方括号时不重复添加", () => {
  expect(buildProxyUrl(form({ host: "::1" }))).toBe("socks5://[::1]:1080");
  expect(buildProxyUrl(form({ host: "2001:db8::1", username: "u", password: "p" }))).toBe(
    "socks5://u:p@[2001:db8::1]:1080",
  );
  expect(buildProxyUrl(form({ host: "[::1]" }))).toBe("socks5://[::1]:1080");
});

test("buildProxyUrl 去除主机与端口的首尾空白；主机为空返回空串", () => {
  expect(buildProxyUrl(form({ host: "  proxy.example.com ", port: " 1080 " }))).toBe("socks5://proxy.example.com:1080");
  expect(buildProxyUrl(form({ host: "   " }))).toBe("");
  expect(buildProxyUrl(emptyProxyForm)).toBe("");
});

test("validateProxyForm：导入/登录场景（没有当前代理）全空合法，只填端口也视为不使用代理", () => {
  expect(validateProxyForm(emptyProxyForm)).toBeNull();
  expect(validateProxyForm(emptyProxyForm, undefined)).toBeNull();
  expect(validateProxyForm({ ...emptyProxyForm, port: "abc" })).toBeNull();
  expect(validateProxyForm({ ...emptyProxyForm, host: "   " })).toBeNull();
});

test("validateProxyForm：没有当前代理时，主机为空却填了用户名或密码要报错并提示填写主机", () => {
  expect(validateProxyForm({ ...emptyProxyForm, username: "u" })).toMatch(/主机/);
  expect(validateProxyForm({ ...emptyProxyForm, password: "p" })).toMatch(/主机/);
  expect(validateProxyForm({ ...emptyProxyForm, host: "  ", username: "u", password: "p" })).toMatch(/主机/);
});

test("validateProxyForm：编辑已有代理时主机不能为空，提示使用「删除」按钮", () => {
  expect(validateProxyForm({ ...proxyFormFromView(current), host: "" }, current)).toMatch(/删除/);
  expect(validateProxyForm({ ...proxyFormFromView(current), host: "   " }, current)).toMatch(/删除/);
  expect(validateProxyForm(emptyProxyForm, current)).toMatch(/删除/);
});

test("validateProxyForm：编辑已有代理时主机非空则照常校验端口", () => {
  expect(validateProxyForm(proxyFormFromView(current), current)).toBeNull();
  expect(validateProxyForm({ ...proxyFormFromView(current), port: "" }, current)).toMatch(/端口/);
  expect(validateProxyForm({ ...proxyFormFromView(current), port: "65536" }, current)).toMatch(/端口/);
});

test("validateProxyForm：填了主机时端口必填", () => {
  expect(validateProxyForm(form({ port: "" }))).toMatch(/端口/);
  expect(validateProxyForm(form({ port: "   " }))).toMatch(/端口/);
});

test("validateProxyForm：端口必须是 1 到 65535 的整数", () => {
  for (const port of ["1", "1080", "65535", " 8080 "]) {
    expect(validateProxyForm(form({ port }))).toBeNull();
  }
  for (const port of ["0", "65536", "-1", "1.5", "abc", "1e3", "+80", "08x"]) {
    expect(validateProxyForm(form({ port }))).toMatch(/端口/);
  }
});

test("proxyFormChanged：与当前代理一致（密码框为空且未勾选清除）视为未变化", () => {
  expect(proxyFormChanged(proxyFormFromView(current), current)).toBe(false);
});

test("proxyFormChanged：host、port、username 任一变化即为有变化", () => {
  const base = proxyFormFromView(current);
  expect(proxyFormChanged({ ...base, host: "10.0.0.1" }, current)).toBe(true);
  expect(proxyFormChanged({ ...base, port: "1081" }, current)).toBe(true);
  expect(proxyFormChanged({ ...base, username: "other" }, current)).toBe(true);
  expect(proxyFormChanged({ ...base, username: "" }, current)).toBe(true);
});

test("proxyFormChanged：比较时忽略 host 与 port 的首尾空白", () => {
  const base = proxyFormFromView(current);
  expect(proxyFormChanged({ ...base, host: " 127.0.0.1 ", port: " 1080 " }, current)).toBe(false);
});

test("proxyFormChanged：填写了新密码或勾选清除密码即为有变化", () => {
  const base = proxyFormFromView(current);
  expect(proxyFormChanged({ ...base, password: "new" }, current)).toBe(true);
  expect(proxyFormChanged({ ...base, clearPassword: true }, current)).toBe(true);
});

test("proxyFormChanged：IPv6 当前代理按不带方括号的 host 比较", () => {
  const ipv6: ProxyView = { scheme: "socks5", host: "::1", port: 1081, has_password: false };
  expect(proxyFormChanged(proxyFormFromView(ipv6), ipv6)).toBe(false);
});

test("proxyFormChanged：没有当前代理时，空表单未变化，填了主机才算变化", () => {
  expect(proxyFormChanged(emptyProxyForm, undefined)).toBe(false);
  expect(proxyFormChanged({ ...emptyProxyForm, host: "   " }, undefined)).toBe(false);
  expect(proxyFormChanged({ ...emptyProxyForm, port: "1080" }, undefined)).toBe(false);
  expect(proxyFormChanged(form(), undefined)).toBe(true);
});

test("proxyLabel：无代理与 host:port", () => {
  expect(proxyLabel(undefined)).toBe("无代理");
  expect(proxyLabel(current)).toBe("127.0.0.1:1080");
  expect(proxyLabel({ scheme: "socks5h", host: "proxy.example.com", port: 1, has_password: false })).toBe(
    "proxy.example.com:1",
  );
});

test("proxyLabel：IPv6 主机加方括号", () => {
  expect(proxyLabel({ scheme: "socks5", host: "::1", port: 1080, has_password: false })).toBe("[::1]:1080");
});

test("buildProxyPatch：主机、端口、用户名都未改变且保持原密码时带 proxy_keep_password", () => {
  expect(buildProxyPatch(proxyFormFromView(current), current)).toEqual({
    proxy: "socks5://user@127.0.0.1:1080",
    proxy_keep_password: true,
  });
  // 比较时忽略首尾空白。
  expect(buildProxyPatch({ ...proxyFormFromView(current), host: " 127.0.0.1 ", port: " 1080 " }, current)).toEqual({
    proxy: "socks5://user@127.0.0.1:1080",
    proxy_keep_password: true,
  });
});

test("buildProxyPatch：主机、端口或用户名任一改变时不带 proxy_keep_password（旧密码不能带给另一台代理）", () => {
  const base = proxyFormFromView(current);
  for (const [edited, proxy] of [
    [{ ...base, host: "10.0.0.1" }, "socks5://user@10.0.0.1:1080"],
    [{ ...base, port: "1081" }, "socks5://user@127.0.0.1:1081"],
    [{ ...base, username: "other" }, "socks5://other@127.0.0.1:1080"],
  ] as const) {
    const patch = buildProxyPatch(edited, current);
    expect(patch).toEqual({ proxy });
    expect("proxy_keep_password" in patch).toBe(false);
  }
});

test("buildProxyPatch：IPv6 当前代理未改变时仍带 proxy_keep_password", () => {
  const ipv6: ProxyView = { scheme: "socks5", host: "::1", port: 1081, username: "u", has_password: true };
  expect(buildProxyPatch(proxyFormFromView(ipv6), ipv6)).toEqual({
    proxy: "socks5://u@[::1]:1081",
    proxy_keep_password: true,
  });
});

test("passwordPlaceholder：当前有密码且地址、用户名未改变时提示留空保持原密码", () => {
  expect(passwordPlaceholder(proxyFormFromView(current), current)).toBe("留空保持原密码");
});

test("passwordPlaceholder：当前有密码但主机、端口或用户名已改变时提示需重新填写密码", () => {
  const base = proxyFormFromView(current);
  for (const edited of [
    { ...base, host: "10.0.0.1" },
    { ...base, port: "1081" },
    { ...base, username: "other" },
  ]) {
    expect(passwordPlaceholder(edited, current)).toBe("地址或用户名已修改，需重新填写密码");
  }
});

test("passwordPlaceholder：当前无代理或当前代理没有密码时为「可选」", () => {
  expect(passwordPlaceholder(emptyProxyForm, undefined)).toBe("可选");
  const noPassword: ProxyView = { scheme: "socks5", host: "127.0.0.1", port: 1080, has_password: false };
  expect(passwordPlaceholder(proxyFormFromView(noPassword), noPassword)).toBe("可选");
  expect(passwordPlaceholder({ ...proxyFormFromView(noPassword), host: "10.0.0.1" }, noPassword)).toBe("可选");
});

test("buildProxyPatch：填写了新密码时以新密码为准，不带 proxy_keep_password", () => {
  const edited = { ...proxyFormFromView(current), password: "new:pw" };
  const patch = buildProxyPatch(edited, current);
  expect(patch).toEqual({ proxy: "socks5://user:new%3Apw@127.0.0.1:1080" });
  expect("proxy_keep_password" in patch).toBe(false);
});

test("buildProxyPatch：勾选清除密码后不带 proxy_keep_password", () => {
  const edited = { ...proxyFormFromView(current), clearPassword: true };
  const patch = buildProxyPatch(edited, current);
  expect(patch).toEqual({ proxy: "socks5://user@127.0.0.1:1080" });
  expect("proxy_keep_password" in patch).toBe(false);
});

test("buildProxyPatch：当前没有代理时只带 proxy", () => {
  const patch = buildProxyPatch(form({ username: "u", password: "p" }), undefined);
  expect(patch).toEqual({ proxy: "socks5://u:p@127.0.0.1:1080" });
  expect("proxy_keep_password" in patch).toBe(false);
});

test("buildProxyPatch：主机清空时 proxy 为空串（删除代理），不带 proxy_keep_password", () => {
  const patch = buildProxyPatch({ ...proxyFormFromView(current), host: "", port: "" }, current);
  expect(patch).toEqual({ proxy: "" });
  expect("proxy_keep_password" in patch).toBe(false);
});
