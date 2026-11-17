import ReactMarkdown, { defaultUrlTransform } from "react-markdown";
import remarkGfm from "remark-gfm";
import rehypeRaw from "rehype-raw";
import rehypeSanitize, { defaultSchema } from "rehype-sanitize";
import { createContext, useContext, useMemo, type ComponentProps, type ReactNode } from "react";
import type { Components } from "react-markdown";
import CodeBlock, { type CodeBlockInfo } from "./CodeBlock";

type MarkdownContentProps = {
  content: string;
  assetBase?: string;
  /** Enable the safe HTML subset used by model README files. */
  allowHtml?: boolean;
  /** Extra header actions for fenced code blocks, next to the copy button. */
  codeBlockActions?: (block: CodeBlockInfo) => ReactNode;
};

const readmeSchema = {
  ...defaultSchema,
  attributes: {
    ...defaultSchema.attributes,
    "*": [...(defaultSchema.attributes?.["*"] ?? []), "style"],
  },
};

// The renderers are module-level so their identity never changes: an inline
// renderer is a new component type on every render, which remounts every code
// block (and its buttons' state) on each streamed chunk. What varies per
// render reaches them through this context instead.
const CodeBlockContext = createContext<{ source?: string; actions?: (block: CodeBlockInfo) => ReactNode }>({});

function MarkdownLink({ children, href }: ComponentProps<"a">) {
  return (
    <a href={href} target="_blank" rel="noreferrer">
      {children}
    </a>
  );
}

function MarkdownPre(props: ComponentProps<typeof CodeBlock>) {
  const { source, actions } = useContext(CodeBlockContext);
  return <CodeBlock {...props} source={source} actions={actions} />;
}

const components: Components = { a: MarkdownLink, pre: MarkdownPre };

export default function MarkdownContent({ content, assetBase, allowHtml = false, codeBlockActions }: MarkdownContentProps) {
  // Block offsets refer to the string react-markdown parsed, so CodeBlock
  // reads its source from the same normalized text.
  const source = normalizeLooseStrong(content);
  const blockContext = useMemo(() => ({ source, actions: codeBlockActions }), [source, codeBlockActions]);
  return (
    <div className={`markdown-content${allowHtml ? " markdown-content--allow-html" : ""}`}>
      <CodeBlockContext.Provider value={blockContext}>
        <ReactMarkdown
          urlTransform={assetBase ? (raw) => {
            const safe = defaultUrlTransform(raw);
            if (!safe) return "";
            try { return new URL(safe, assetBase).href; } catch { return ""; }
          } : undefined}
          remarkPlugins={[remarkGfm]}
          rehypePlugins={allowHtml ? [rehypeRaw, [rehypeSanitize, assetBase ? defaultSchema : readmeSchema]] : undefined}
          components={components}
        >
          {source}
        </ReactMarkdown>
      </CodeBlockContext.Provider>
    </div>
  );
}

function normalizeLooseStrong(content: string): string {
  let fence: "```" | "~~~" | undefined;
  return content.split(/(\r?\n)/).map((part) => {
    if (/^\r?\n$/.test(part)) return part;
    const marker = part.trimStart().match(/^(```|~~~)/)?.[1] as "```" | "~~~" | undefined;
    if (marker) {
      if (!fence) fence = marker;
      else if (fence === marker) fence = undefined;
      return part;
    }
    if (fence) return part;
    return part.split(/(`+[^`]*?`+)/g).map((segment, index) => (
      index % 2 === 1
        ? segment
        : segment.replace(/\*\*[ \t]*([^*\n]*?\S)[ \t]*\*\*/g, "**$1**")
    )).join("");
  }).join("");
}
