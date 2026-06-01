import { rpc, type RpcFile, type RpcResponse } from './rpc'
import type { ActionRequest, ActionResponse } from './types'

export class ApiError extends Error {
  status: number
  constructor(status: number, message: string) {
    super(message)
    this.status = status
    this.name = 'ApiError'
  }
}

function errText(r: RpcResponse): string {
  if (r.error) return r.error
  if (r.json && typeof r.json === 'object' && 'error' in (r.json as object))
    return String((r.json as { error: unknown }).error)
  if (r.text) return r.text.slice(0, 300)
  return `request failed (${r.status})`
}

export async function apiGet<T>(path: string): Promise<T> {
  const r = await rpc.request({ method: 'GET', path })
  if (r.status >= 400) throw new ApiError(r.status, errText(r))
  return r.json as T
}

/** Markdown / plain-text endpoints (briefs, updates, kb files). */
export async function apiGetText(path: string): Promise<string> {
  const r = await rpc.request({ method: 'GET', path })
  if (r.status >= 400) throw new ApiError(r.status, errText(r))
  if (typeof r.text === 'string') return r.text
  if (typeof r.json === 'string') return r.json
  return ''
}

export async function apiAction(req: ActionRequest, timeoutMs?: number): Promise<ActionResponse> {
  const r = await rpc.request({ method: 'POST', path: '/api/actions', body: req, timeoutMs })
  const data = (r.json ?? {}) as ActionResponse
  if (r.status >= 400 || data.ok === false)
    throw new ApiError(r.status, data.message || errText(r))
  return data
}

export async function apiActionForm(
  form: Record<string, string>,
  files: RpcFile[],
): Promise<ActionResponse> {
  const r = await rpc.request({ method: 'POST', path: '/api/actions', form, files })
  const data = (r.json ?? {}) as ActionResponse
  if (r.status >= 400 || data.ok === false)
    throw new ApiError(r.status, data.message || errText(r))
  return data
}

export async function apiPutText(path: string, text: string, opts: { mtime?: string } = {}): Promise<void> {
  const url = opts.mtime
    ? `${path}${path.includes('?') ? '&' : '?'}mtime=${encodeURIComponent(opts.mtime)}`
    : path
  const r = await rpc.request({ method: 'PUT', path: url, text })
  if (r.status >= 400) throw new ApiError(r.status, errText(r))
}

/** Generic JSON POST (e.g. /api/fs/mkdir) over the WS-RPC channel. */
export async function apiPost<T>(path: string, body: unknown): Promise<T> {
  const r = await rpc.request({ method: 'POST', path, body })
  if (r.status >= 400) throw new ApiError(r.status, errText(r))
  return r.json as T
}

/** Read a File/Blob as a base64 RpcFile for upload over the socket. */
export function fileToRpcFile(file: File, field = 'images'): Promise<RpcFile> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader()
    reader.onerror = () => reject(reader.error)
    reader.onload = () => {
      const result = String(reader.result)
      const comma = result.indexOf(',')
      resolve({
        field,
        name: file.name || 'upload',
        content_type: file.type || 'application/octet-stream',
        data: comma >= 0 ? result.slice(comma + 1) : result,
      })
    }
    reader.readAsDataURL(file)
  })
}
