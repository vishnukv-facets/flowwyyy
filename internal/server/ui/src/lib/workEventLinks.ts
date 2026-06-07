export interface WorkEventLinkLike {
  kind: string
  target: string
  url?: string
}

export function workEventLinkHref(link: WorkEventLinkLike): string {
  if (link.url) return link.url
  switch (link.kind) {
    case 'task':
    case 'session':
      return `/session/${encodeURIComponent(link.target)}`
    case 'project':
      return `/project/${encodeURIComponent(link.target)}`
    case 'attention':
      return attentionURL({ item: link.target })
    case 'trace':
      return attentionURL({ view: 'trace', trace: link.target })
    case 'source':
      return link.target
    default:
      return ''
  }
}

function attentionURL(params: { view?: 'trace'; item?: string; trace?: string }): string {
  const query = new URLSearchParams()
  if (params.view) query.set('view', params.view)
  if (params.item) query.set('item', params.item)
  if (params.trace) query.set('trace', params.trace)
  const suffix = query.toString()
  return suffix ? `/attention?${suffix}` : '/attention'
}
