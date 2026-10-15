// 账号页的操作上下文，由控制台（App.tsx）创建并向下传递。
import type { ManagementClient } from "./api";

export type Actions = {
  client: ManagementClient;
  /** 有变更操作进行中，此时禁用按钮。 */
  busy: boolean;
  /**
   * 执行一次变更操作：期间禁用按钮；任务返回的文字作为成功提示；
   * 无论成功与否都会刷新账号列表。
   */
  run: (task: () => Promise<string | void>) => Promise<void>;
  /** 直接显示错误（用于后台流程，如设备登录失败）。 */
  fail: (message: string) => void;
};
