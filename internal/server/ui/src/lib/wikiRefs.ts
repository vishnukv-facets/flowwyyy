// Extract the [[wiki-link]] target names referenced in a markdown body.
export function wikiRefs(content: string): string[] {
  const out: string[] = []
  const re = /\[\[([^\]\n]+)\]\]/g
  let m: RegExpExecArray | null
  while ((m = re.exec(content || '')) !== null) out.push(m[1].trim().toLowerCase())
  return out
}
