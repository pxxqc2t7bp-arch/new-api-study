chrome.runtime.onMessage.addListener((message, _sender, sendResponse) => {
  if (message?.type !== 'upstream:get-auth-token') return false
  const token =
    localStorage.getItem('auth_token') ||
    localStorage.getItem('token') ||
    sessionStorage.getItem('auth_token')
  sendResponse({ token: token || '' })
  return false
})
