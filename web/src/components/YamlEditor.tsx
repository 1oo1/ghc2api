// YAML 编辑器：行号栏 + 高亮层（<pre>）+ 透明文字的 <textarea> 叠放。
// 三层的字体、行高、内边距、tab 宽度必须一致（见 style.css 的 .yaml-text）；滚动时用 transform 同步高亮层与行号栏。
// 记号以 React 文本节点渲染，不使用 dangerouslySetInnerHTML。
import { useLayoutEffect, useMemo, useRef } from "react";
import { insertIndent } from "../configEditor";
import { highlightYaml } from "../yamlHighlight";

export function YamlEditor({
  value,
  onChange,
  readOnly,
  errorLine,
}: {
  value: string;
  onChange: (value: string) => void;
  readOnly: boolean;
  /** 校验失败的行号（从 1 起），行号栏标红。 */
  errorLine?: number;
}) {
  const lines = useMemo(() => highlightYaml(value), [value]);
  const areaRef = useRef<HTMLTextAreaElement>(null);
  const gutterRef = useRef<HTMLDivElement>(null);
  const layerRef = useRef<HTMLPreElement>(null);
  /** Tab 插入缩进后，等新内容渲染完再恢复光标位置。 */
  const cursorRef = useRef<number | null>(null);

  useLayoutEffect(() => {
    const area = areaRef.current;
    if (area && cursorRef.current !== null) {
      area.setSelectionRange(cursorRef.current, cursorRef.current);
      cursorRef.current = null;
    }
  }, [value]);

  const syncScroll = () => {
    const area = areaRef.current;
    if (!area) return;
    if (layerRef.current) layerRef.current.style.transform = `translate(${-area.scrollLeft}px, ${-area.scrollTop}px)`;
    if (gutterRef.current) gutterRef.current.style.transform = `translateY(${-area.scrollTop}px)`;
  };

  return (
    <div className="yaml-editor">
      <div className="yaml-gutter" aria-hidden="true">
        <div ref={gutterRef}>
          {lines.map((_, i) => (
            <div key={i} className={"yaml-text yaml-lineno" + (i + 1 === errorLine ? " error" : "")}>
              {i + 1}
            </div>
          ))}
        </div>
      </div>
      <div className="yaml-main">
        <div className="yaml-layer" aria-hidden="true">
          <pre ref={layerRef} className="yaml-text yaml-highlight">
            {lines.map((tokens, i) => (
              <span key={i}>
                {tokens.map((token, j) => (
                  <span key={j} className={"tok-" + token.type}>
                    {token.text}
                  </span>
                ))}
                {i < lines.length - 1 ? "\n" : ""}
              </span>
            ))}
            {/* 末尾补一个空格：内容以换行结尾时，最后一个空行也要有行框，才能与 textarea 对齐。 */}
            {" "}
          </pre>
        </div>
        <textarea
          ref={areaRef}
          className="yaml-text yaml-input"
          aria-label="配置文件内容"
          value={value}
          readOnly={readOnly}
          spellCheck={false}
          wrap="off"
          autoCapitalize="off"
          autoCorrect="off"
          onChange={(e) => onChange(e.target.value)}
          onScroll={syncScroll}
          onKeyDown={(e) => {
            if (e.key !== "Tab" || e.shiftKey || e.ctrlKey || e.metaKey || e.altKey || readOnly) return;
            e.preventDefault();
            const { selectionStart, selectionEnd } = e.currentTarget;
            const next = insertIndent(value, selectionStart, selectionEnd);
            cursorRef.current = next.cursor;
            onChange(next.value);
          }}
        />
      </div>
    </div>
  );
}
