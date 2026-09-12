/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import { describe, expect, test } from 'vitest'

import { CHANNEL_TYPE_GEMINI } from '../../constants'
import { channelSchema } from '../../types'
import {
  CHANNEL_FORM_DEFAULT_VALUES,
  transformChannelToFormDefaults,
  transformFormDataToCreatePayload,
} from '../channel-form'

describe('Gemini Live channel capability', () => {
  test('defaults to disabled and restores an enabled channel', () => {
    expect(
      (CHANNEL_FORM_DEFAULT_VALUES as Record<string, unknown>)
        .gemini_live_enabled
    ).toBe(false)

    const channel = channelSchema.parse({
      id: 24,
      type: CHANNEL_TYPE_GEMINI,
      key: '',
      status: 1,
      name: 'Gemini Live',
      created_time: 0,
      test_time: 0,
      response_time: 0,
      balance_updated_time: 0,
      settings: '{"gemini_live_enabled":true}',
    })
    const defaults = transformChannelToFormDefaults(channel)

    expect(
      (defaults as unknown as Record<string, unknown>).gemini_live_enabled
    ).toBe(true)
  })

  test('persists only for native Gemini channels', () => {
    const geminiForm = {
      ...CHANNEL_FORM_DEFAULT_VALUES,
      name: 'Gemini Live',
      type: CHANNEL_TYPE_GEMINI,
      key: 'test-key',
      models: 'gemini-live-test',
      gemini_live_enabled: true,
    }
    const geminiPayload = transformFormDataToCreatePayload(geminiForm)
    expect(JSON.parse(String(geminiPayload.channel.settings))).toMatchObject({
      gemini_live_enabled: true,
    })

    const openAIForm = {
      ...geminiForm,
      type: 1,
    }
    const openAIPayload = transformFormDataToCreatePayload(openAIForm)
    expect(
      JSON.parse(String(openAIPayload.channel.settings))
    ).not.toHaveProperty('gemini_live_enabled')
  })
})
