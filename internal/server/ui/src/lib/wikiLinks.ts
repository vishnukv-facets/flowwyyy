const wikiTokenRe = /\[\[([^\]\n]+)\]\]/g

export function taskWikiMarkdown(source: string, knownTaskSlugs: Set<string>): string {
  if (!source || knownTaskSlugs.size === 0) return source || ''
  return source.replace(wikiTokenRe, (raw, targetRaw: string) => {
    const target = targetRaw.trim()
    if (!target || target.includes('|') || target.includes('/') || target.includes('\\')) return raw
    if (!knownTaskSlugs.has(target)) return raw
    return `[${target}](#task:${encodeURIComponent(target)})`
  })
}
