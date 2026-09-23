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
import {
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import type { ReactNode } from 'react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { Dialog } from '@/components/dialog'

import { Combobox } from '../combobox'
import {
  Drawer,
  DrawerContent,
  DrawerDescription,
  DrawerTitle,
} from '../drawer'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '../select'

const options = [
  { value: 'openai', label: 'OpenAI' },
  { value: 'gemini', label: 'Google' },
]

function ProviderCombobox(props: {
  onValueChange: (value: string | null) => void
}) {
  return (
    <Combobox
      options={options}
      value='openai'
      onValueChange={props.onValueChange}
      aria-label='Provider'
    />
  )
}

function ProviderSelect(props: {
  onValueChange: (value: string | null) => void
}) {
  return (
    <Select value='openai' onValueChange={props.onValueChange}>
      <SelectTrigger aria-label='Provider'>
        <SelectValue />
      </SelectTrigger>
      <SelectContent>
        {options.map((option) => (
          <SelectItem key={option.value} value={option.value}>
            {option.label}
          </SelectItem>
        ))}
      </SelectContent>
    </Select>
  )
}

function FilterDrawer(props: { children: ReactNode }) {
  return (
    <Drawer open>
      <DrawerContent>
        <DrawerTitle>Filters</DrawerTitle>
        <DrawerDescription>Adjust the result filters</DrawerDescription>
        {props.children}
      </DrawerContent>
    </Drawer>
  )
}

function elementRect(
  left: number,
  top: number,
  width: number,
  height: number
): DOMRect {
  return {
    x: left,
    y: top,
    left,
    top,
    right: left + width,
    bottom: top + height,
    width,
    height,
    toJSON: () => ({}),
  }
}

// JSDOM does not implement the pointer-capture API used by the drawer.
const pointerCapture = Object.getOwnPropertyDescriptor(
  HTMLElement.prototype,
  'setPointerCapture'
)

beforeEach(() => {
  Object.defineProperty(HTMLElement.prototype, 'setPointerCapture', {
    configurable: true,
    value: () => undefined,
  })
})

afterEach(() => {
  if (pointerCapture) {
    Object.defineProperty(
      HTMLElement.prototype,
      'setPointerCapture',
      pointerCapture
    )
  } else {
    Reflect.deleteProperty(HTMLElement.prototype, 'setPointerCapture')
  }
})

describe('popups inside a drawer', () => {
  it('renders combobox options inside the drawer dialog and clicking one applies it without closing the drawer', async () => {
    const change = vi.fn()
    render(
      <FilterDrawer>
        <ProviderCombobox onValueChange={change} />
      </FilterDrawer>
    )
    const user = userEvent.setup()
    const dialog = screen.getByRole('dialog', { name: 'Filters' })

    await user.click(within(dialog).getByRole('combobox', { name: 'Provider' }))
    await user.click(
      await within(dialog).findByRole('option', { name: 'Google' })
    )

    expect(change).toHaveBeenCalledWith('gemini')
    expect(screen.getByRole('dialog', { name: 'Filters' })).toBeInTheDocument()
  })

  it('renders select options inside the drawer dialog and clicking one applies it without closing the drawer', async () => {
    const change = vi.fn()
    render(
      <FilterDrawer>
        <ProviderSelect onValueChange={change} />
      </FilterDrawer>
    )
    const user = userEvent.setup()
    const dialog = screen.getByRole('dialog', { name: 'Filters' })

    await user.click(within(dialog).getByRole('combobox', { name: 'Provider' }))
    await user.click(
      await within(dialog).findByRole('option', { name: 'Google' })
    )

    expect(change).toHaveBeenCalledWith('gemini', expect.anything())
    expect(screen.getByRole('dialog', { name: 'Filters' })).toBeInTheDocument()
  })
})

describe('popups inside a transformed dialog', () => {
  it('positions an editable combobox in dialog coordinates and follows scrolling', async () => {
    const change = vi.fn()
    render(
      <Dialog open title='Advanced Custom'>
        <Combobox
          options={options}
          value='openai'
          onValueChange={change}
          allowCustomValue
          aria-label='Request Model Name'
        />
      </Dialog>
    )
    const user = userEvent.setup()
    const dialog = screen.getByRole('dialog', { name: 'Advanced Custom' })
    const input = within(dialog).getByRole('combobox', {
      name: 'Request Model Name',
    })
    vi.spyOn(dialog, 'getBoundingClientRect').mockReturnValue(
      elementRect(100, 50, 800, 600)
    )
    const inputRect = vi
      .spyOn(input, 'getBoundingClientRect')
      .mockReturnValue(elementRect(260, 180, 240, 40))

    await user.click(input)
    const listbox = await within(dialog).findByRole('listbox')
    const dropdown = listbox.parentElement
    expect(dropdown).toHaveStyle({
      position: 'fixed',
      top: '174px',
      left: '160px',
      width: '240px',
    })

    inputRect.mockReturnValue(elementRect(240, 140, 240, 40))
    fireEvent.scroll(dialog)
    await waitFor(() =>
      expect(dropdown).toHaveStyle({ top: '134px', left: '140px' })
    )

    await user.click(within(listbox).getByRole('option', { name: 'Google' }))
    expect(change).toHaveBeenCalledWith('gemini')
    expect(dialog).toBeVisible()
  })

  it('normalizes coordinates measured during the dialog scale animation', async () => {
    render(
      <Dialog open title='Animated Advanced Custom'>
        <Combobox
          options={options}
          value='openai'
          onValueChange={vi.fn()}
          allowCustomValue
          aria-label='Upstream Model Name'
        />
      </Dialog>
    )
    const user = userEvent.setup()
    const dialog = screen.getByRole('dialog', {
      name: 'Animated Advanced Custom',
    })
    const input = within(dialog).getByRole('combobox', {
      name: 'Upstream Model Name',
    })
    Object.defineProperties(dialog, {
      offsetWidth: { configurable: true, value: 800 },
      offsetHeight: { configurable: true, value: 600 },
    })
    vi.spyOn(dialog, 'getBoundingClientRect').mockReturnValue(
      elementRect(260, 115, 760, 570)
    )
    vi.spyOn(input, 'getBoundingClientRect').mockReturnValue(
      elementRect(412, 267, 228, 38)
    )

    await user.click(input)
    const dropdown = (await within(dialog).findByRole('listbox')).parentElement
    expect(dropdown).toHaveStyle({
      position: 'fixed',
      top: '204px',
      left: '160px',
      width: '240px',
    })
  })
})

describe('popups outside a drawer', () => {
  it('portals combobox options to document.body outside the component subtree', async () => {
    const view = render(<ProviderCombobox onValueChange={vi.fn()} />)
    const user = userEvent.setup()

    await user.click(screen.getByRole('combobox', { name: 'Provider' }))
    const listbox = await screen.findByRole('listbox')

    expect(document.body).toContainElement(listbox)
    expect(view.container).not.toContainElement(listbox)
  })

  it('portals select options to document.body outside the component subtree', async () => {
    const view = render(<ProviderSelect onValueChange={vi.fn()} />)
    const user = userEvent.setup()

    await user.click(screen.getByRole('combobox', { name: 'Provider' }))
    const listbox = await screen.findByRole('listbox')

    expect(document.body).toContainElement(listbox)
    expect(view.container).not.toContainElement(listbox)
  })
})
