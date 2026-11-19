import { describe, expect, test } from "bun:test";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { Markdown } from "./markdown";

const html = (text: string) => renderToStaticMarkup(createElement(Markdown, { text }));

describe("Markdown", () => {
  test("paragraphs and shifted headings", () => {
    expect(html("one\n\ntwo")).toBe('<div class="md"><p>one</p><p>two</p></div>');
    expect(html("# a\n\n###### b")).toBe('<div class="md"><h2>a</h2><h6>b</h6></div>');
  });

  test("fenced code keeps its language and text", () => {
    expect(html("```go\nx := 1 < 2\n```")).toContain('<pre class="console" data-lang="go"><code>x := 1 &lt; 2</code></pre>');
    expect(html("```\nplain\n```")).not.toContain("data-lang");
  });

  test("inline formatting", () => {
    const out = html("`c<d>` **b** *i* ~~s~~");
    expect(out).toContain("<code>c&lt;d&gt;</code>");
    expect(out).toContain("<strong>b</strong>");
    expect(out).toContain("<em>i</em>");
    expect(out).toContain("<del>s</del>");
  });

  test("nested and ordered lists", () => {
    expect(html("- a\n  - b\n- c")).toBe(
      '<div class="md"><ul><li><span>a</span><ul><li><span>b</span></li></ul></li><li><span>c</span></li></ul></div>',
    );
    expect(html("3. x\n4. y")).toContain('<ol start="3">');
    expect(html("1. x\n2. y")).toContain("<ol>");
  });

  test("task lists render disabled checkboxes", () => {
    const out = html("- [x] done\n- [ ] todo");
    expect(out.match(/<input[^>]*>/g)).toHaveLength(2);
    expect(out).toContain('type="checkbox" disabled=""');
    expect(out).toContain('checked=""');
  });

  test("tables keep alignment", () => {
    const out = html("| a | b |\n|:-:|--:|\n| 1 | 2 |");
    expect(out).toContain('<th style="text-align:center">a</th>');
    expect(out).toContain('<td style="text-align:right">2</td>');
    expect(out).toContain("<thead>");
    expect(out).toContain("<tbody>");
  });

  test("blockquotes", () => {
    expect(html("> quoted")).toBe('<div class="md"><blockquote><p>quoted</p></blockquote></div>');
  });

  test("entities and angle brackets are text", () => {
    expect(html("a &amp; b")).toBe('<div class="md"><p>a &amp; b</p></div>');
    expect(html("x < y")).toBe('<div class="md"><p>x &lt; y</p></div>');
    expect(html("&lt;b&gt; &#65;")).toBe('<div class="md"><p>&lt;b&gt; A</p></div>');
  });

  test("raw html is escaped", () => {
    const out = html("<script>alert(1)</script>\n\ntext <img src=x onerror=alert(1)> more");
    expect(out).toContain("&lt;script&gt;");
    expect(out).not.toContain("<script");
    expect(out).not.toContain("<img");
  });

  test("unsafe link schemes are not links", () => {
    for (const href of ["javascript:alert(1)", "data:text/html,x", "vbscript:x", "/relative", "//host/x"]) {
      const out = html(`[x](${href})`);
      expect(out).not.toContain("href");
      expect(out).not.toContain("<a");
      expect(out).toContain("x");
    }
  });

  test("safe links open safely", () => {
    const out = html("[ok](https://example.com) [m](mailto:a@example.com)");
    expect(out).toContain('href="https://example.com/"');
    expect(out).toContain('target="_blank"');
    expect(out).toContain('rel="noopener noreferrer nofollow"');
    expect(out).toContain('href="mailto:a@example.com"');
  });

  test("images become links, never img", () => {
    const out = html("![alt](https://example.com/a.png)");
    expect(out).toContain('<a href="https://example.com/a.png"');
    expect(out).toContain(">alt</a>");
    expect(out).not.toContain("<img");
    expect(html("![](https://example.com/a.png)")).toContain(">https://example.com/a.png</a>");
    const bad = html("![alt](javascript:x)");
    expect(bad).not.toContain("<a");
    expect(bad).toContain("alt");
  });

  test("empty and blank input render", () => {
    expect(html("")).toBe('<div class="md"></div>');
    expect(html("  \n \n")).toBe('<div class="md"></div>');
  });
});
