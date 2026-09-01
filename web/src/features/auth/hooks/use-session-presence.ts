/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.
*/
import { useEffect } from 'react'

import { refreshAuthentication } from '@/lib/api'
import { useAuthStore } from '@/stores/auth-store'

const REFRESH_INTERVAL_MS = 10 * 60 * 1000
const RECONNECT_DELAY_MS = 5000

export function useSessionPresence() {
  const sessionId = useAuthStore((state) => state.auth.session?.sid)

  useEffect(() => {
    if (!sessionId) return

    let source: EventSource | null = null
    let reconnectTimer: number | undefined
    let stopped = false

    const connect = () => {
      if (stopped) return
      source?.close()
      source = new EventSource('/api/user/auth/presence', {
        withCredentials: true,
      })
      source.addEventListener('error', () => {
        source?.close()
        if (!stopped) {
          reconnectTimer = window.setTimeout(connect, RECONNECT_DELAY_MS)
        }
      })
    }

    connect()
    const refreshTimer = window.setInterval(() => {
      void refreshAuthentication()
    }, REFRESH_INTERVAL_MS)

    return () => {
      stopped = true
      source?.close()
      window.clearInterval(refreshTimer)
      if (reconnectTimer !== undefined) window.clearTimeout(reconnectTimer)
    }
  }, [sessionId])
}
