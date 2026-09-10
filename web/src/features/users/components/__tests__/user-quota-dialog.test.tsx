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
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import type { ButtonHTMLAttributes, ReactNode } from 'react'
import { afterEach, describe, expect, test, vi } from 'vitest'

import { api } from '@/lib/api'

import { UserQuotaDialog } from '../user-quota-dialog'

type ButtonStubProps = ButtonHTMLAttributes<HTMLButtonElement> & {
  size?: string
  variant?: string
}

vi.mock('@/components/ui/button', () => {
  const ButtonElement = 'button'

  return {
    Button: ({ size: _size, variant: _variant, ...props }: ButtonStubProps) => (
      <ButtonElement {...props} />
    ),
  }
})

vi.mock('@/components/dialog', () => ({
  Dialog: (props: { children: ReactNode; footer?: ReactNode }) => (
    <section aria-label='Adjust Quota'>
      {props.children}
      {props.footer}
    </section>
  ),
}))

type ApiMethod = (url: string, data?: unknown) => Promise<{ data: unknown }>
type MockableApi = {
  post: ApiMethod
}

const apiClient = api as unknown as MockableApi
const originalPost = apiClient.post

function renderInSurroundingForm(onSubmit = () => undefined): void {
  render(
    <form
      onSubmit={(event) => {
        event.preventDefault()
        onSubmit()
      }}
    >
      <UserQuotaDialog
        open
        userId={7}
        currentQuota={500_000}
        onOpenChange={() => undefined}
        onSuccess={() => undefined}
      />
    </form>
  )
}

afterEach(() => {
  apiClient.post = originalPost
})

describe('UserQuotaDialog defensive form isolation', () => {
  test('declares Confirm and Cancel as non-submit buttons', () => {
    renderInSurroundingForm()

    expect({
      confirm: screen
        .getByRole('button', { name: 'Confirm' })
        .getAttribute('type'),
      cancel: screen
        .getByRole('button', { name: 'Cancel' })
        .getAttribute('type'),
    }).toEqual({
      confirm: 'button',
      cancel: 'button',
    })
  })

  test('Confirm submits one quota adjustment without submitting a surrounding form', async () => {
    const postedUrls: string[] = []
    const surroundingFormSubmit = vi.fn()
    apiClient.post = async (url) => {
      postedUrls.push(url)
      return { data: { success: true } }
    }
    renderInSurroundingForm(surroundingFormSubmit)

    fireEvent.change(screen.getByRole('spinbutton'), {
      target: { value: '1' },
    })
    fireEvent.click(screen.getByRole('button', { name: 'Confirm' }))

    await waitFor(() => {
      expect(postedUrls).toEqual(['/api/user/manage'])
    })
    expect(surroundingFormSubmit).not.toHaveBeenCalled()
  })

  test('Cancel submits neither a quota adjustment nor a surrounding form', () => {
    const postedUrls: string[] = []
    const surroundingFormSubmit = vi.fn()
    apiClient.post = async (url) => {
      postedUrls.push(url)
      return { data: { success: true } }
    }
    renderInSurroundingForm(surroundingFormSubmit)

    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }))

    expect(postedUrls).toEqual([])
    expect(surroundingFormSubmit).not.toHaveBeenCalled()
  })
})
