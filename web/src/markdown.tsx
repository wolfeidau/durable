import { marked, type Token, type Tokens } from "marked";
import { createElement, useMemo, type ReactElement, type ReactNode } from "react";

const named: Record<string, string> = { amp: "&", lt: "<", gt: ">", quot: '"', apos: "'", nbsp: " " };

// The lexer leaves character references undecoded, and React escapes the output itself.
function decode(s: string): string {
  return s.replace(/&(#x[0-9a-f]{1,6}|#[0-9]{1,7}|[a-z]+);/gi, (m, ref: string) => {
    if (ref[0] !== "#") return named[ref.toLowerCase()] ?? m;
    const code = ref[1] === "x" || ref[1] === "X" ? parseInt(ref.slice(2), 16) : parseInt(ref.slice(1), 10);
    return code > 0 && code <= 0x10ffff && !(code >= 0xd800 && code <= 0xdfff) ? String.fromCodePoint(code) : m;
  });
}

const safeProtocols = new Set(["http:", "https:", "mailto:"]);

function safeHref(href: string): string | undefined {
  try {
    const url = new URL(decode(href));
    return safeProtocols.has(url.protocol) ? url.href : undefined;
  } catch {
    return undefined;
  }
}

function inline(tokens: Token[] | undefined): ReactNode[] {
  return (tokens ?? []).map((t, i) => render(t, i));
}

function render(t: Token, key: number): ReactNode {
  switch (t.type) {
    case "space":
    case "def":
      return null;
    case "paragraph":
      return <p key={key}>{inline((t as Tokens.Paragraph).tokens)}</p>;
    case "heading": {
      const h = t as Tokens.Heading;
      return createElement(`h${Math.min(h.depth + 1, 6)}`, { key }, inline(h.tokens));
    }
    case "code": {
      const c = t as Tokens.Code;
      return (
        <pre key={key} className="console" data-lang={c.lang || undefined}>
          <code>{c.text}</code>
        </pre>
      );
    }
    case "blockquote":
      return <blockquote key={key}>{inline((t as Tokens.Blockquote).tokens)}</blockquote>;
    case "list": {
      const l = t as Tokens.List;
      const items = l.items.map((it, i) => <li key={i}>{inline(it.tokens)}</li>);
      return l.ordered ? (
        <ol key={key} start={l.start === "" || l.start === 1 ? undefined : l.start}>
          {items}
        </ol>
      ) : (
        <ul key={key}>{items}</ul>
      );
    }
    case "checkbox":
      return <input key={key} type="checkbox" checked={!!(t as Tokens.Generic).checked} disabled readOnly />;
    case "table": {
      const tb = t as Tokens.Table;
      const cell = (c: Tokens.TableCell, i: number) =>
        createElement(c.header ? "th" : "td", { key: i, style: { textAlign: c.align ?? undefined } }, inline(c.tokens));
      return (
        <table key={key}>
          <thead>
            <tr>{tb.header.map(cell)}</tr>
          </thead>
          <tbody>
            {tb.rows.map((r, i) => (
              <tr key={i}>{r.map(cell)}</tr>
            ))}
          </tbody>
        </table>
      );
    }
    case "hr":
      return <hr key={key} />;
    case "text": {
      const x = t as Tokens.Text;
      return x.tokens ? <span key={key}>{inline(x.tokens)}</span> : decode(x.text);
    }
    case "strong":
      return <strong key={key}>{inline((t as Tokens.Strong).tokens)}</strong>;
    case "em":
      return <em key={key}>{inline((t as Tokens.Em).tokens)}</em>;
    case "del":
      return <del key={key}>{inline((t as Tokens.Del).tokens)}</del>;
    case "codespan":
      return <code key={key}>{(t as Tokens.Codespan).text}</code>;
    case "br":
      return <br key={key} />;
    case "escape":
      return (t as Tokens.Escape).text;
    case "link": {
      const l = t as Tokens.Link;
      const href = safeHref(l.href);
      const children = inline(l.tokens);
      return href ? (
        <a key={key} href={href} target="_blank" rel="noopener noreferrer nofollow">
          {children}
        </a>
      ) : (
        <span key={key}>{children}</span>
      );
    }
    case "image": {
      const im = t as Tokens.Image;
      const href = safeHref(im.href);
      const alt = decode(im.text);
      return href ? (
        <a key={key} href={href} target="_blank" rel="noopener noreferrer nofollow">
          {alt || href}
        </a>
      ) : (
        alt
      );
    }
    default:
      return t.raw;
  }
}

export function Markdown({ text }: { text: string }): ReactElement {
  const tokens = useMemo(() => marked.lexer(text, { gfm: true }), [text]);
  return <div className="md">{tokens.map((t, i) => render(t, i))}</div>;
}
