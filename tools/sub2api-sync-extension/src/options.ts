const serverInput = document.querySelector('#server-url')
const pairingInput = document.querySelector('#pairing-code')
const pairButton = document.querySelector('#pair')
const syncButton = document.querySelector('#sync')
const status = document.querySelector('#status')

function setStatus(message) {
  status.textContent = message
}

async function load() {
  const config = await chrome.storage.local.get([
    'serverUrl',
    'deviceId',
    'lastSyncAt',
    'lastSyncError',
  ])
  serverInput.value = config.serverUrl || ''
  if (config.deviceId) {
    setStatus(
      config.lastSyncError ||
        `已配对 ${config.deviceId}${config.lastSyncAt ? ` · ${new Date(config.lastSyncAt).toLocaleString()}` : ''}`
    )
  }
}

async function pair() {
  const serverUrl = String(serverInput.value || '').replace(/\/$/, '')
  const pairingCode = String(pairingInput.value || '').trim()
  if (!serverUrl || !pairingCode) {
    setStatus('请输入 New API URL 和配对码')
    return
  }
  pairButton.disabled = true
  try {
    const origin = new URL(serverUrl).origin
    const granted = await chrome.permissions.request({ origins: [`${origin}/*`] })
    if (!granted) throw new Error('未授权访问 New API')
    const response = await fetch(`${serverUrl}/api/upstream-orchestration/device/pair`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        pairing_code: pairingCode,
        device_name: `Chrome ${navigator.platform}`,
      }),
    })
    const payload = await response.json()
    if (!response.ok || !payload.success || !payload.data?.token) {
      throw new Error(payload.message || `HTTP ${response.status}`)
    }
    await chrome.storage.local.set({
      serverUrl,
      deviceId: payload.data.device_id,
      deviceToken: payload.data.token,
    })
    pairingInput.value = ''
    setStatus(`已配对 ${payload.data.device_id}`)
  } catch (error) {
    setStatus(error instanceof Error ? error.message : String(error))
  } finally {
    pairButton.disabled = false
  }
}

async function sync() {
  syncButton.disabled = true
  setStatus('同步中...')
  try {
    const response = await chrome.runtime.sendMessage({ type: 'upstream:sync-now' })
    if (!response?.ok) throw new Error(response?.error || '同步失败')
    setStatus('同步完成')
  } catch (error) {
    setStatus(error instanceof Error ? error.message : String(error))
  } finally {
    syncButton.disabled = false
  }
}

pairButton.addEventListener('click', pair)
syncButton.addEventListener('click', sync)
load()
