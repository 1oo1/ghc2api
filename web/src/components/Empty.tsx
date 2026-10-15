// 空状态提示。
export function Empty({ title, text }: { title: string; text: string }) {
  return (
    <section className="empty">
      <span>◎</span>
      <h2>{title}</h2>
      <p>{text}</p>
    </section>
  );
}
