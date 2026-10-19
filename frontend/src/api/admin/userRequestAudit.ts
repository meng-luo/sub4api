import { apiClient } from '../client'
import type { PaginatedResponse } from '@/types'

export interface UserRequestAudit {
  id: number
  created_at: string
  updated_at: string
  expires_at?: string
  user_id?: number
  api_key_id?: number
  group_id?: number
  group_name?: string
  protocol: string
  endpoint: string
  requested_model?: string
  upstream_model?: string
  client_request_id?: string
  response_id?: string
  previous_response_id?: string
  fallback_hash?: string
  status: string
  input_usage?: Record<string, unknown>
  output_usage?: Record<string, unknown>
  cache_usage?: Record<string, unknown>
  last_error?: string
  metadata?: Record<string, unknown>
  request_chatml?: string
  response_chatml?: string
  conversation_key?: string
  user_label?: string
  request_count?: number
}
export interface UserRequestAuditMessage {
  role: string
  content: string
  reasoning?: string
  tool_name?: string
  call_id?: string
  kind: 'text' | 'think' | 'tool_call' | 'tool_result' | 'metadata' | 'error'
  request_id: number
  incomplete?: boolean
}
export interface UserRequestAuditConversation {
  id: number
  user_label: string
  request_count: number
  request_start: number
  request_end: number
  messages: UserRequestAuditMessage[]
  requests: UserRequestAudit[]
}
export interface UserRequestAuditQuery {
  page?: number; page_size?: number; user_id?: number; group_id?: number; group_name?: string; requested_model?: string; response_id?: string; client_request_id?: string; status?: string; protocol?: string; start_time?: string; end_time?: string; q?: string
}
export interface UserRequestAuditConfig {
  retention_days: number
  cleanup_interval_hours: number
  group_ids: number[]
}
export interface UserRequestAuditStatus { total_bytes: number; file_count: number; disk_total_bytes: number; disk_free_bytes: number; recording_paused: boolean; pause_reason?: string; oldest_at?: string; latest_at?: string; last_cleanup_at?: string }
export interface UserRequestAuditModelFacet { protocol: string; requested_model: string; requests: number; conversations: number; latest_at: string }
export interface UserRequestAuditGroupStat { group_id?: number; group_name?: string; count: number; completed: number; failed: number; input_total: number; output_total: number; latest_at?: string }
export type UserRequestAuditGroupStatsResponse = UserRequestAuditGroupStat[]
export type UserRequestAuditExportFormat = 'jsonl' | 'json'
export interface UserRequestAuditExportResult { blob: Blob; filename: string }
export interface UserRequestAuditExportJob {
  id: string
  status: 'queued' | 'running' | 'completed' | 'failed'
  format: UserRequestAuditExportFormat
  created_at: string
  completed_at?: string
  expires_at?: string
  filename?: string
  error?: string
  rows: number
  size_bytes?: number
}
const exportFilters = (filters: UserRequestAuditQuery): Omit<UserRequestAuditQuery, 'page' | 'page_size'> => { const { page: _page, page_size: _pageSize, ...rest } = filters; return rest }
function filenameFromDisposition(value: unknown, format: UserRequestAuditExportFormat): string {
  const fallback = `user-request-audit.${format}.gz`
  if (typeof value !== 'string') return fallback
  const match = value.match(/filename\*?=(?:UTF-8''|"?)([^";]+)"?/i)
  let candidate = ''
  try { candidate = match?.[1] ? decodeURIComponent(match[1]) : '' } catch { candidate = '' }
  candidate = Array.from(candidate, char => char.charCodeAt(0) < 32 ? '_' : char)
    .join('').replace(/[\\/:*?"<>|]/g, '_').trim()
  return candidate || fallback
}
export const userRequestAuditAPI = {
  async list(params: UserRequestAuditQuery): Promise<PaginatedResponse<UserRequestAudit>> { return (await apiClient.get('/admin/user-request-audit', { params })).data },
  async get(id: number): Promise<UserRequestAudit> { return (await apiClient.get(`/admin/user-request-audit/${id}`)).data },
  async getConversationWindow(id: number, offset: number, limit: number, signal?: AbortSignal): Promise<UserRequestAuditConversation> {
    return (await apiClient.get(`/admin/user-request-audit/${id}/conversation`, { params: { request_offset: offset, request_limit: limit }, signal })).data
  },
  async getGroupStats(params: UserRequestAuditQuery): Promise<UserRequestAuditGroupStatsResponse> { return (await apiClient.get('/admin/user-request-audit/groups', { params: exportFilters(params) })).data },
  async getModels(params: UserRequestAuditQuery, signal?: AbortSignal): Promise<UserRequestAuditModelFacet[]> { return (await apiClient.get('/admin/user-request-audit/models', { params: exportFilters(params), signal })).data },
  async getStatus(): Promise<UserRequestAuditStatus> { return (await apiClient.get('/admin/user-request-audit/status')).data },
  async getConfig(): Promise<UserRequestAuditConfig> { return (await apiClient.get('/admin/user-request-audit/config')).data },
  async updateConfig(config: Partial<UserRequestAuditConfig>): Promise<UserRequestAuditConfig> { return (await apiClient.put('/admin/user-request-audit/config', config)).data },
  async cleanup(): Promise<void> { await apiClient.post('/admin/user-request-audit/cleanup') },
  async clear(): Promise<{ message?: string }> { return (await apiClient.post('/admin/user-request-audit/clear')).data },
  async export(params: UserRequestAuditQuery, format: UserRequestAuditExportFormat, conversationIDs?: number[]): Promise<UserRequestAuditExportJob> {
    // Selection narrows the filtered set; it never discards the active filters.
    const selection = conversationIDs?.length ? { ...exportFilters(params), conversation_ids: conversationIDs } : exportFilters(params)
    return (await apiClient.post('/admin/user-request-audit/export', { format, ...selection })).data
  },
  async getExports(signal?: AbortSignal): Promise<UserRequestAuditExportJob[]> {
    return (await apiClient.get('/admin/user-request-audit/exports', { signal })).data
  },
  async downloadExport(job: UserRequestAuditExportJob): Promise<UserRequestAuditExportResult> {
    const response = await apiClient.get(`/admin/user-request-audit/exports/${encodeURIComponent(job.id)}/download`, {
      responseType: 'blob',
      timeout: 0,
      // Keep 401 on the authenticated client's refresh path; decode other blob errors here.
      validateStatus: status => (status >= 200 && status < 300) || (status >= 400 && status !== 401)
    })
    if (response.status >= 400) {
      const text = await (response.data as Blob).text()
      let message = text
      try {
        const error = JSON.parse(text)
        message = error.message || error.detail || text
      } catch { /* Plain-text server errors are already readable. */ }
      throw new Error(message || `HTTP ${response.status}`)
    }
    return { blob: response.data as Blob, filename: filenameFromDisposition(response.headers?.['content-disposition'], job.format) }
  }
}
export default userRequestAuditAPI
