import { useEffect, useState } from 'react'

// Re-renders the calling component on a fixed interval so relative-time
// readouts (countdowns, "ago" labels) tick live without a network refetch.
// Returns Date.now() at each tick. Keep the caller small — only the component
// that needs the live clock should use this, so the re-render stays localized.
export function useNow(intervalMs = 1000): number {
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), intervalMs)
    return () => clearInterval(id)
  }, [intervalMs])
  return now
}
