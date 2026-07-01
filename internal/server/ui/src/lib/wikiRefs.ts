// Extract the [[wiki-link]] target names referenced in a markdown body.
export function wikiRefs(content: string): string[] {
  const out: string[] = []
  const re = /\[\[([^\]\n]+)\]\]/g
  let m: RegExpExecArray | null
  while ((m = re.exec(content || '')) !== null) {
    const target = m[1].split('|')[0]?.split('#')[0]?.trim().toLowerCase()
    if (target) out.push(target)
  }
  return out
}
