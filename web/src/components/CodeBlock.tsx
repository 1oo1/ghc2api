// 代码块：把正文格式化（JSON 美化、SSE 逐行处理）后按记号高亮。
// 日志含用户提示词等不可信内容，这里只用文本节点渲染记号，不使用 dangerouslySetInnerHTML。
import { useMemo } from "react";
import { formatBody } from "../highlight";

/** 着色的记号数上限：超过时仍显示格式化后的文本，但不着色，避免生成数十万个节点卡住页面。 */
const MAX_HIGHLIGHT_TOKENS = 50_000;

export function CodeBlock({ text }: { text: string }) {
  const body = useMemo(() => formatBody(text, MAX_HIGHLIGHT_TOKENS), [text]);
  return (
    <>
      {body.format !== "text" && !body.highlighted && (
        <p className="code-note">内容较大，未着色</p>
      )}
      <pre className="code-block">
        <code>
          {body.tokens.map((token, i) => (
            <span key={i} className={"tok-" + token.type}>
              {token.text}
            </span>
          ))}
        </code>
      </pre>
    </>
  );
}
