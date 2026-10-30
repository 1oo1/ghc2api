// 账号代理表单的纯函数：拼装代理 URL、校验、比较与生成 PATCH 请求体。规则见 docs/proxy.md「控制台」。
import type { ProxyPatch, ProxyView } from "./api";

/** 代理表单各输入框的文本；port 是文本，因此未填写与非法输入都能表达。scheme 固定为 socks5，不在表单中。 */
export type ProxyForm = {
  host: string;
  port: string;
  username: string;
  password: string;
  /** 清除已保存的密码（仅编辑已有代理时有意义）。 */
  clearPassword: boolean;
};

export const emptyProxyForm: ProxyForm = {
  host: "",
  port: "",
  username: "",
  password: "",
  clearPassword: false,
};

/** 由账号当前代理预填表单：密码框留空；没有代理时为空表单。 */
export function proxyFormFromView(proxy?: ProxyView): ProxyForm {
  if (!proxy) return emptyProxyForm;
  return {
    host: proxy.host,
    port: String(proxy.port),
    username: proxy.username ?? "",
    password: "",
    clearPassword: false,
  };
}

/** 去掉 IPv6 主机两侧的方括号，便于与服务端返回的 host 比较。 */
function bareHost(host: string): string {
  const trimmed = host.trim();
  return trimmed.startsWith("[") && trimmed.endsWith("]") ? trimmed.slice(1, -1) : trimmed;
}

/** 展示与拼装 URL 用的主机：IPv6（含冒号）加方括号。 */
function bracketHost(host: string): string {
  const bare = bareHost(host);
  return bare.includes(":") ? `[${bare}]` : bare;
}

/** 拼装代理 URL（scheme 固定 socks5）；主机为空返回空串，表示不使用代理。 */
export function buildProxyUrl(form: ProxyForm): string {
  const host = bracketHost(form.host);
  if (host === "") return "";
  const username = form.username.trim();
  let userinfo = "";
  if (username !== "") {
    userinfo = encodeURIComponent(username);
    if (form.password !== "") userinfo += `:${encodeURIComponent(form.password)}`;
    userinfo += "@";
  }
  return `socks5://${userinfo}${host}:${form.port.trim()}`;
}

/** 表单的主机、端口、用户名是否都与账号当前代理相同（去除首尾空白后比较）。 */
function sameEndpoint(form: ProxyForm, current: ProxyView): boolean {
  return (
    bareHost(form.host) === current.host &&
    form.port.trim() === String(current.port) &&
    form.username.trim() === (current.username ?? "")
  );
}

/**
 * 校验表单，返回错误文案，合法返回 null。current 为账号当前代理（编辑场景），没有时为导入/登录场景。
 * 主机为空：编辑时一律出错（删除代理须用「删除」按钮）；导入/登录时全空表示不使用代理，只填了用户名或密码则出错。
 * 填了主机时端口必填且为 1 到 65535 的整数。
 */
export function validateProxyForm(form: ProxyForm, current?: ProxyView): string | null {
  if (form.host.trim() === "") {
    if (current) return "主机不能为空；如需删除代理，请使用「删除」按钮";
    if (form.username.trim() !== "" || form.password !== "")
      return "请填写主机，或清空用户名和密码";
    return null;
  }
  const port = form.port.trim();
  if (port === "") return "请填写端口";
  if (!/^\d+$/.test(port) || Number(port) < 1 || Number(port) > 65535)
    return "端口必须是 1 到 65535 之间的整数";
  return null;
}

/** 表单相对账号当前代理是否有变化；密码框为空且未勾选清除视为未变化。 */
export function proxyFormChanged(form: ProxyForm, current?: ProxyView): boolean {
  if (!current) return form.host.trim() !== "";
  return (
    bareHost(form.host) !== current.host ||
    form.port.trim() !== String(current.port) ||
    form.username.trim() !== (current.username ?? "") ||
    form.password !== "" ||
    form.clearPassword
  );
}

/** 卡片上的代理文字：无代理或 host:port（IPv6 主机加方括号）。 */
export function proxyLabel(proxy?: ProxyView): string {
  return proxy ? `${bracketHost(proxy.host)}:${proxy.port}` : "无代理";
}

/** 密码框的占位文案：当前代理有密码时，地址与用户名未改变可留空保持原密码，否则需要重新填写。 */
export function passwordPlaceholder(form: ProxyForm, current?: ProxyView): string {
  if (!current?.has_password) return "可选";
  return sameEndpoint(form, current) ? "留空保持原密码" : "地址或用户名已修改，需重新填写密码";
}

/**
 * 构造 PATCH 请求体中的代理部分。主机为空时 proxy 为空串（删除代理）；
 * 仅当当前代理有密码、密码框为空、未勾选清除，且主机、端口、用户名都未改变时才带 proxy_keep_password
 * （服务端同样只在这种情况下沿用密码，避免把密码发给另一台代理）。
 */
export function buildProxyPatch(form: ProxyForm, current?: ProxyView): ProxyPatch {
  const proxy = buildProxyUrl(form);
  const keep =
    proxy !== "" &&
    current?.has_password === true &&
    form.password === "" &&
    !form.clearPassword &&
    sameEndpoint(form, current);
  return keep ? { proxy, proxy_keep_password: true } : { proxy };
}
