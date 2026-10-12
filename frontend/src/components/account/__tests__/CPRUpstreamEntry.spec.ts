// Full frontend tests; distinct from the standalone Go core test report.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import type { AdminGroup } from '@/types'
import CPRUpstreamEntry from '../CPRUpstreamEntry.vue'

const api = vi.hoisted(() => ({ list: vi.fn(), create: vi.fn(), getById: vi.fn(), update: vi.fn() }))
vi.mock('@/api/admin', () => ({ adminAPI: { accounts: api } }))
let wrapper: VueWrapper
beforeEach(() => {
  vi.resetAllMocks()
  Object.defineProperty(HTMLDialogElement.prototype, 'showModal', { configurable: true, value() { this.setAttribute('open', '') } })
  Object.defineProperty(HTMLDialogElement.prototype, 'close', { configurable: true, value() { this.removeAttribute('open') } })
  api.list.mockResolvedValue({ items: [], total: 0 })
  api.create.mockResolvedValue({ id: 101 })
  api.update.mockResolvedValue({ id: 101 })
  wrapper = mount(CPRUpstreamEntry, {
    props: { groups: [{ id: 7, name: 'CPR test', platform: 'openai' } as AdminGroup] },
    global: { stubs: { Teleport: true } }
  })
})
afterEach(() => wrapper.unmount())
async function open() { await wrapper.find('button').trigger('click') }
async function configureGroup() {
  await wrapper.find('#cpr-group-id').setValue('7')
  await wrapper.find('#cpr-owner-user-id').setValue('42')
}

describe('CPR上游 account entry', () => {
  it('creates an explicitly scoped OpenAI API-key account', async () => {
    await open()
    await configureGroup()
    await wrapper.find('input[type="url"]').setValue('https://cpr.example.test/v1')
    await wrapper.find('input[type="password"]').setValue('test-key-not-a-real-credential')
    await wrapper.find('form').trigger('submit')
    await flushPromises()
    expect(api.create).toHaveBeenCalledOnce()
    const request = api.create.mock.calls[0][0]
    expect(request.platform).toBe('openai')
    expect(request.type).toBe('apikey')
    expect(request.extra.cpr_upstream).toBe(true)
    expect(request.extra.cpr_owner_user_id).toBe(42)
    expect(request.group_ids).toEqual([7])
    expect(request.extra.openai_apikey_responses_websockets_v2_mode).toBe('off')
    expect((wrapper.find('input[type="password"]').element as HTMLInputElement).value).toBe('')
    expect(wrapper.emitted('saved')).toHaveLength(1)
  })
  it('does not create into an occupied group', async () => {
    api.list.mockResolvedValue({ items: [{ id: 8 }], total: 1 })
    await open()
    await configureGroup()
    await wrapper.find('form').trigger('submit')
    await flushPromises()
    expect(api.create).not.toHaveBeenCalled()
    expect(wrapper.find('[role="alert"]').text()).toContain('空分组')
  })
  it('rejects a base URL containing credentials', async () => {
    await open()
    await configureGroup()
    await wrapper.find('input[type="url"]').setValue('https://user:secret@cpr.example.test/v1')
    await wrapper.find('input[type="password"]').setValue('test-key')
    await wrapper.find('form').trigger('submit')
    await flushPromises()
    expect(api.create).not.toHaveBeenCalled()
    expect(wrapper.find('[role="alert"]').text()).toContain('不能带用户名')
  })
  it('enables an existing account without resending its credentials', async () => {
    api.getById.mockResolvedValue({ id: 101, platform: 'openai', type: 'apikey', extra: { existing: true } })
    await open()
    await wrapper.find('#cpr-operation').setValue('enable')
    await configureGroup()
    await wrapper.find('#cpr-existing-id').setValue('101')
    await wrapper.find('form').trigger('submit')
    await flushPromises()
    expect(api.update).toHaveBeenCalledOnce()
    const updates = api.update.mock.calls[0][1]
    expect(updates.credentials).toBeUndefined()
    expect(updates.extra.existing).toBe(true)
    expect(updates.extra.cpr_upstream).toBe(true)
  })
  it('refuses to reinterpret an OAuth account as a CPR API-key entry', async () => {
    api.getById.mockResolvedValue({ id: 101, platform: 'openai', type: 'oauth', extra: {} })
    await open()
    await wrapper.find('#cpr-operation').setValue('enable')
    await configureGroup()
    await wrapper.find('#cpr-existing-id').setValue('101')
    await wrapper.find('form').trigger('submit')
    await flushPromises()
    expect(api.update).not.toHaveBeenCalled()
  })
  it('disables the marker without deleting account credentials', async () => {
    api.getById.mockResolvedValue({ id: 101, platform: 'openai', type: 'apikey', extra: { cpr_upstream: true, existing: true } })
    await open()
    await wrapper.find('#cpr-operation').setValue('disable')
    await wrapper.find('#cpr-existing-id').setValue('101')
    await wrapper.find('form').trigger('submit')
    await flushPromises()
    expect(api.update.mock.calls[0][1]).toEqual({ extra: { cpr_upstream: false, existing: true } })
  })
})
