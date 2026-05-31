import { memo } from 'react'
import ReactMarkdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import rehypeHighlight from 'rehype-highlight'

// All flow prose (briefs, updates, KB, memories, transcripts) renders through
// here: GitHub-flavored markdown + highlight.js syntax highlighting, themed by
// hljs.css. Links open in a new tab; raw HTML is intentionally not enabled.
export const Md = memo(function Md({
  source,
  className,
  onWikiLink,
}: {
  source: string
  className?: string
  // When provided, [[name]] tokens render as clickable in-app links that call
  // this instead of navigating away — used for KB/memory cross-references.
  onWikiLink?: (name: string) => void
}) {
  const src = onWikiLink
    ? (source || '').replace(/\[\[([^\]\n]+)\]\]/g, (_m, n: string) => `[${n.trim()}](#wiki:${encodeURIComponent(n.trim())})`)
    : source || ''
  return (
    <div className={`md${className ? ' ' + className : ''}`}>
      <ReactMarkdown
        remarkPlugins={[remarkGfm]}
        rehypePlugins={[[rehypeHighlight, { detect: true, ignoreMissing: true }]]}
        components={{
          a: ({ children, href, ...props }) => {
            if (onWikiLink && href?.startsWith('#wiki:')) {
              const name = decodeURIComponent(href.slice('#wiki:'.length))
              return (
                <a
                  className="wikilink"
                  href={href}
                  onClick={(e) => {
                    e.preventDefault()
                    onWikiLink(name)
                  }}
                >
                  {children}
                </a>
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
