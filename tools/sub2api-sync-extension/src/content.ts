chrome.runtime.onMessage.addListener((message, _sender, sendResponse) => {
  if (message?.type === 'upstream:get-auth-session') {
    sendResponse({
      accessToken:
        localStorage.getItem('auth_token') ||
        localStorage.getItem('token') ||
        sessionStorage.getItem('auth_token') ||
        '',
      refreshToken: localStorage.getItem('refresh_token') || '',
      expiresAt: Number(localStorage.getItem('token_expires_at')) || 0,
    })
    return false
  }
  if (message?.type === 'upstream:set-auth-session') {
    if (
      !message.expectedRefreshToken ||
      localStorage.getItem('refresh_token') !== message.expectedRefreshToken
    ) {
      sendResponse({ updated: false })
      return false
    }
    localStorage.setItem('auth_token', String(message.accessToken || ''))
    localStorage.setItem('refresh_token', String(message.refreshToken || ''))
    localStorage.setItem('token_expires_at', String(message.expiresAt || 0))
    sendResponse({ updated: true })
    return false
  }
  return false
})
