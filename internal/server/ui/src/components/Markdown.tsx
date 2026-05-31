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
}: {
  source: string
  className?: string
}) {
  return (
    <div className={`md${className ? ' ' + className : ''}`}>
      <ReactMarkdown
        remarkPlugins={[remarkGfm]}
        rehypePlugins={[[rehypeHighlight, { detect: true, ignoreMissing: true }]]}
        components={{
          a: ({ children, ...props }) => (
            <a {...props} target="_blank" rel="noreferrer noopener">
              {children}
            </a>
          ),
        }}
      >
        {source || ''}
      </ReactMarkdown>
    </div>
  )
})
