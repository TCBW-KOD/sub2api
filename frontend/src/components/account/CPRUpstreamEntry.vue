<template>
  <button class="btn btn-secondary" type="button" @click="openDialog">CPR上游</button>
  <Teleport to="body">
    <dialog ref="dialog" class="cpr-dialog" aria-labelledby="cpr-title" @cancel.prevent="closeDialog">
      <form class="space-y-4" @submit.prevent="save">
        <header class="flex items-center justify-between gap-4">
          <div>
            <h2 id="cpr-title" class="text-lg font-semibold">OpenAI · CPR上游</h2>
            <p class="mt-1 text-sm opacity-70">sub2api 管理鉴权、计费与路由；CPR 处理 Codex 协议。</p>
          </div>
          <button class="btn btn-secondary" type="button" :disabled="busy" aria-label="关闭" @click="closeDialog">关闭</button>
        </header>
        <div class="rounded-lg border p-3 text-sm leading-relaxed">
          <strong>HTTP/SSE 原生转发 · 开发预览</strong><br>
          保留请求正文、动态会话头和模型能力，不转换 custom/grammar 工具。<br>
          本版明确拒绝 WebSocket、Chat Completions 和旧 Compact 端点，不会隐式桥接。
          请先在隔离测试实例验证计费，不要直接替换生产环境。
        </div>
        <label class="block text-sm">操作
          <select id="cpr-operation" v-model="operation" class="input mt-1 w-full" :disabled="busy">
            <option value="create">新增 CPR 上游账号</option>
            <option value="enable">将已有 OpenAI API Key 账号设为 CPR 上游</option>
            <option value="disable">取消已有账号的 CPR 原生标记</option>
          </select>
        </label>
        <label v-if="operation !== 'create'" class="block text-sm">已有账号 ID
          <input id="cpr-existing-id" v-model.number="existingID" type="number" min="1" step="1" required class="input mt-1 w-full" :disabled="busy">
        </label>
        <template v-if="operation === 'create'">
          <label class="block text-sm">账号名称
            <input v-model.trim="name" required maxlength="100" class="input mt-1 w-full" :disabled="busy">
          </label>
          <label class="block text-sm">CPR Base URL（必须以 /v1 结尾）
            <input v-model.trim="baseURL" type="url" required placeholder="https://cpr.example.com/v1" class="input mt-1 w-full" :disabled="busy">
          </label>
          <label class="block text-sm">CPR 客户端 API Key（不是 OAuth Token）
            <input v-model="apiKey" type="password" autocomplete="new-password" required class="input mt-1 w-full" :disabled="busy" spellcheck="false">
          </label>
          <div class="grid grid-cols-2 gap-3">
            <label class="text-sm">账号并发
              <input v-model.number="concurrency" type="number" min="1" max="10000" step="1" required class="input mt-1 w-full" :disabled="busy">
            </label>
            <label class="text-sm">计费倍率
              <input v-model.number="rateMultiplier" type="number" min="0" step="0.01" required class="input mt-1 w-full" :disabled="busy">
            </label>
          </div>
        </template>
        <label v-if="operation !== 'disable'" class="block text-sm">允许使用的 sub2api 用户 ID
          <input id="cpr-owner-user-id" v-model.number="ownerUserID" type="number" min="1" step="1" required class="input mt-1 w-full" :disabled="busy">
          <span class="mt-1 block text-xs opacity-70">不是 GitHub ID。本预览版每个CPR原生分组仅允许一个可信用户，避免未经验证地取消共享会话状态的租户隔离。</span>
        </label>
        <label v-if="operation !== 'disable'" class="block text-sm">专用 OpenAI 分组
          <select id="cpr-group-id" v-model.number="groupID" required class="input mt-1 w-full" :disabled="busy">
            <option :value="0" disabled>请选择事先创建的空 OpenAI 分组</option>
            <option v-for="group in openAIGroups" :key="group.id" :value="group.id">{{ group.name }} (#{{ group.id }})</option>
          </select>
          <span class="mt-1 block text-xs opacity-70">不可与普通账号混组。启用已有账号时，会将其分组绑定替换为所选分组，凭据不改动。</span>
        </label>
        <p v-if="operation === 'disable'" class="text-sm">只取消CPR标记，不删除账号或凭据。先将账号移出CPR专用分组，避免混组；兼容性设置不会自动还原。</p>
        <p v-if="operation === 'enable'" class="text-sm">已有模型映射、推理和Fast策略若要求改写正文，原生模式会明确拒绝，不会忽略权限或偷偷改写。</p>
        <p v-if="errorMessage" role="alert" class="rounded border p-3 text-sm">{{ errorMessage }}</p>
        <footer class="flex justify-end gap-2">
          <button type="button" class="btn btn-secondary" :disabled="busy" @click="closeDialog">取消</button>
          <button type="submit" class="btn btn-primary" :disabled="busy">{{ busy ? '保存中…' : '保存' }}</button>
        </footer>
      </form>
    </dialog>
  </Teleport>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, ref } from 'vue'
import { adminAPI } from '@/api/admin'
import type { AdminGroup, CreateAccountRequest } from '@/types'

const props = defineProps<{ groups: AdminGroup[] }>()
const emit = defineEmits<{ saved: [] }>()
const dialog = ref<HTMLDialogElement | null>(null)
const operation = ref<'create' | 'enable' | 'disable'>('create')
const name = ref('CPR上游')
const baseURL = ref('')
const apiKey = ref('')
const existingID = ref<number | null>(null)
const groupID = ref(0)
const ownerUserID = ref<number | null>(null)
const concurrency = ref(5)
const rateMultiplier = ref(1)
const busy = ref(false)
const errorMessage = ref('')
const openAIGroups = computed(() => props.groups.filter(group => group.platform === 'openai'))
let opener: HTMLElement | null = null

function openDialog() {
  errorMessage.value = ''
  apiKey.value = ''
  opener = document.activeElement instanceof HTMLElement ? document.activeElement : null
  dialog.value?.showModal()
}
function closeDialog() {
  if (busy.value) return
  apiKey.value = ''
  dialog.value?.close()
  opener?.focus()
}
onBeforeUnmount(() => { apiKey.value = '' })

function nativeExtra(existing: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    ...existing,
    cpr_upstream: true,
    cpr_owner_user_id: ownerUserID.value,
    openai_passthrough: false,
    openai_responses_mode: 'force_responses',
    openai_responses_supported: true,
    openai_compact_mode: 'force_off',
    openai_apikey_responses_websockets_v2_mode: 'off',
    codex_image_generation_bridge: false,
    codex_image_generation_explicit_tool_policy: 'allow'
  }
}
function validateBaseURL(value: string): string {
  const url = new URL(value)
  if (!['http:', 'https:'].includes(url.protocol) || url.username || url.password || url.search || url.hash) {
    throw new Error('Base URL只能使用HTTP(S)，不能带用户名、密码、查询参数或片段。')
  }
  if (!url.pathname.replace(/\/+$/, '').endsWith('/v1')) throw new Error('Base URL必须以/v1结尾。')
  return value.replace(/\/+$/, '')
}
async function save() {
  if (busy.value) return
  busy.value = true
  errorMessage.value = ''
  try {
    const id = existingID.value
    if (operation.value !== 'create' && (!id || !Number.isSafeInteger(id) || id < 1)) throw new Error('请输入有效账号ID。')
    if (operation.value !== 'disable') {
      if (!ownerUserID.value || !Number.isSafeInteger(ownerUserID.value) || ownerUserID.value < 1) throw new Error('请输入允许使用的sub2api用户ID。')
      if (!openAIGroups.value.some(group => group.id === groupID.value)) throw new Error('请选择OpenAI专用分组。')
      // UI preflight only. The backend independently enforces these boundaries.
      const listing = await adminAPI.accounts.list(1, 2, { group: String(groupID.value) })
      const onlyThisAccount = listing.total === 1 && listing.items[0]?.id === id
      if (listing.total > 0 && !(operation.value === 'enable' && onlyThisAccount)) throw new Error('请选择空分组，或仅包含待启用账号的分组。')
    }
    if (operation.value === 'create') {
      const base = validateBaseURL(baseURL.value)
      if (!name.value) throw new Error('请输入账号名称。')
      if (!Number.isInteger(concurrency.value) || concurrency.value < 1 || concurrency.value > 10000) throw new Error('并发必须是1–10000的整数。')
      if (!Number.isFinite(rateMultiplier.value) || rateMultiplier.value < 0) throw new Error('计费倍率不能为负数。')
      if (!apiKey.value || /[\r\n]/.test(apiKey.value)) throw new Error('请输入有效CPR API Key。')
      const payload: CreateAccountRequest = {
        name: name.value,
        platform: 'openai',
        type: 'apikey',
        credentials: {
          base_url: base,
          api_key: apiKey.value,
          pool_mode: true,
          // Legacy name used by the text scheduler. Native ingress itself
          // exposes only /responses and /models, not /chat/completions.
          openai_capabilities: ['chat_completions']
        },
        extra: nativeExtra(),
        concurrency: concurrency.value,
        priority: 1,
        rate_multiplier: rateMultiplier.value,
        group_ids: [groupID.value]
      }
      await adminAPI.accounts.create(payload)
    } else {
      const account = await adminAPI.accounts.getById(id!)
      if (account.platform !== 'openai' || account.type !== 'apikey') throw new Error('只支持OpenAI API Key账号，不能将OAuth账号作为CPR入口。')
      if (operation.value === 'enable') {
        await adminAPI.accounts.update(id!, { extra: nativeExtra(account.extra ?? {}), group_ids: [groupID.value] })
      } else {
        await adminAPI.accounts.update(id!, { extra: { ...(account.extra ?? {}), cpr_upstream: false } })
      }
    }
    apiKey.value = ''
    busy.value = false
    closeDialog()
    emit('saved')
  } catch (error) {
    // Never log Axios request objects: they contain the CPR API key.
    errorMessage.value = error instanceof Error ? error.message : '保存失败，请检查账号与分组配置。'
  } finally {
    busy.value = false
  }
}
</script>

<style scoped>
.cpr-dialog {
  width: min(680px, calc(100vw - 32px));
  max-height: calc(100dvh - 48px);
  overflow-y: auto;
  border: 1px solid #d1d5db;
  border-radius: 16px;
  padding: 24px;
  color: #111827;
  background: #ffffff;
}
.cpr-dialog::backdrop { background: rgb(0 0 0 / 50%); }
:global(.dark) .cpr-dialog { color: #e5e7eb; background: #111827; border-color: #374151; }
</style>
