import { expect, test } from "vitest";
import { MAX_PRIORITY, MIN_PRIORITY, parsePriority } from "./account";

test("优先级范围为 -1 到 100", () => {
  expect(MIN_PRIORITY).toBe(-1);
  expect(MAX_PRIORITY).toBe(100);
});

test("合法整数（含边界）返回对应数值", () => {
  expect(parsePriority("-1")).toBe(-1);
  expect(parsePriority("0")).toBe(0);
  expect(parsePriority("42")).toBe(42);
  expect(parsePriority("100")).toBe(100);
});

test("去除首尾空白后再解析", () => {
  expect(parsePriority("  7 ")).toBe(7);
  expect(parsePriority("\t-1\n")).toBe(-1);
});

test("空串与纯空白返回 null", () => {
  expect(parsePriority("")).toBeNull();
  expect(parsePriority("   ")).toBeNull();
});

test("非数字返回 null", () => {
  for (const text of ["abc", "1a", "0x10", "-", "--1", "1 2"]) {
    expect(parsePriority(text)).toBeNull();
  }
});

test("小数与科学计数法返回 null", () => {
  for (const text of ["1.5", "5.0", "0.5", "1e1"]) {
    expect(parsePriority(text)).toBeNull();
  }
});

test("带正号返回 null", () => {
  expect(parsePriority("+5")).toBeNull();
});

test("超出 -1 到 100 的范围返回 null", () => {
  for (const text of ["-2", "-100", "101", "1000"]) {
    expect(parsePriority(text)).toBeNull();
  }
});
