import { describe, expect, it } from 'vitest'

import {
  CHANNEL_TYPE_OPTIONS,
  CHANNEL_TYPE_VOLCENGINE_3D,
  MODEL_FETCHABLE_TYPES,
} from '../../constants'
import { getChannelTypeConfig } from '../channel-type-config'
import { getChannelTypeIcon } from '../channel-utils'

describe('VolcEngine 3D channel registration', () => {
  it('registers the channel type and Ark defaults', () => {
    expect(
      CHANNEL_TYPE_OPTIONS.some(
        (item) => item.value === CHANNEL_TYPE_VOLCENGINE_3D
      )
    ).toBe(true)
    expect(MODEL_FETCHABLE_TYPES.has(CHANNEL_TYPE_VOLCENGINE_3D)).toBe(true)
    expect(getChannelTypeIcon(CHANNEL_TYPE_VOLCENGINE_3D)).toBe('Doubao')
    expect(
      getChannelTypeConfig(CHANNEL_TYPE_VOLCENGINE_3D).defaultBaseUrl
    ).toBe('https://ark.cn-beijing.volces.com')
  })
})
