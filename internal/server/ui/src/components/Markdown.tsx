import { memo } from 'react'
import ReactMarkdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import rehypeHighlight from 'rehype-highlight'
import { taskWikiMarkdown } from '../lib/wikiLinks'

// All flow prose (briefs, updates, KB, memories, transcripts) renders through
// here: GitHub-flavored markdown + highlight.js syntax highlighting, themed by
// hljs.css. Links open in a new tab; raw HTML is intentionally not enabled.
export const Md = memo(function Md({
  source,
  className,
  onWikiLink,
  onTaskLink,
  knownTaskSlugs,
}: {
  source: string
  className?: string
  // When provided, [[name]] tokens render as clickable in-app links that call
  // this instead of navigating away — used for KB/memory cross-references.
  onWikiLink?: (name: string) => void
  // Task brief/update markdown uses exact task slugs only; unresolved tokens
  // stay visible as inert text.
  onTaskLink?: (slug: string) => void
  knownTaskSlugs?: Set<string>
}) {
  const src = onTaskLink
    ? taskWikiMarkdown(source || '', knownTaskSlugs ?? new Set())
    : onWikiLink
    ? (source || '').replace(/\[\[([^\]\n]+)\]\]/g, (_m, n: string) => `[${n.trim()}](#wiki:${encodeURIComponent(n.trim())})`)
    : source || ''
  return (
    <div className={`md${className ? ' ' + className : ''}`}>
      <ReactMarkdown
        remarkPlugins={[remarkGfm]}
        rehypePlugins={[[rehypeHighlight, { detect: true, ignoreMissing: true }]]}
        components={{
          a: ({ children, href, ...props }) => {
            if (onTaskLink && href?.startsWith('#task:')) {
              const slug = decodeURIComponent(href.slice('#task:'.length))
              return (
                <button
                  type="button"
                  className="wikilink task-wikilink"
                  onClick={() => onTaskLink(slug)}
                >
                  {children}
                </button>
              )
            }
            if (onWikiLink && href?.startsWith('#wiki:')) {
              const name = decodeURIComponent(href.slice('#wiki:'.length))
              return (
                <button
                  type="button"
                  className="wikilink"
                  onClick={() => onWikiLink(name)}
                >
                  {children}
                </button>
              )
            }
            return (
              <a {...props} href={href} target="_blank" rel="noreferrer noopener">
                {children}
              </a>
            )
          },
        }}
      >
        {src}
      </ReactMarkdown>
    </div>
  )
})
