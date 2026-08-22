import '@testing-library/jest-dom/vitest'
import { format } from 'node:util'
import { afterEach, beforeEach, expect } from 'vitest'
import { narrowQuery } from './responsive'

// A React warning is a bug report the suite prints and then throws away. The
// release log carried a screenful of "not wrapped in act(...)" while every test
// stayed green, so a green run now means the console stayed quiet as well.
//
// A test that means to provoke one of these stubs the console itself
// (`vi.spyOn(console, 'error').mockImplementation(() => {})`), which takes it
// out of this count and says in the test that the noise was expected.
const complaints: string[] = []
let realError: typeof console.error
let realWarn: typeof console.warn

beforeEach(() => {
  complaints.length = 0
  realError = console.error
  realWarn = console.warn
  const record =
    (through: (...args: unknown[]) => void) =>
    (...args: unknown[]) => {
      // React passes its messages as a format string plus arguments, so the
      // interesting part — which prop, which component — is in the arguments.
      // Formatted here, or the failure reads "the `%s` prop".
      complaints.push(format(...args))
      // Still printed, so a failure that follows one of these reads in the
      // order it happened.
      through(...args)
    }
  console.error = record(realError) as typeof console.error
  console.warn = record(realWarn) as typeof console.warn
})

afterEach(() => {
  console.error = realError
  console.warn = realWarn
  // Taken out of the list before asserting: a failure here must not leave the
  // next test carrying this one's complaints.
  const seen = complaints.splice(0)
  expect(seen, `the console was not quiet:\n\n${seen.join('\n\n')}`).toEqual([])
})

const answering = (matches: (query: string) => boolean) => (query: string) => ({
  matches: matches(query),
  media: query,
  onchange: null,
  addListener: () => {},
  removeListener: () => {},
  addEventListener: () => {},
  removeEventListener: () => {},
  dispatchEvent: () => false,
})

// Mantine components measure the window; jsdom does not implement either of
// these, and without them every render throws. Answering false to everything is
// a wide window, which is what most of these tests are written against.
window.matchMedia = window.matchMedia || answering(() => false)

/**
 * Render the next component as a phone would see it, and put the window back.
 *
 * Which presentation a page uses — a table or a card per row — is decided by a
 * media query, and jsdom answers false to every query it is asked. A test that
 * wants the narrow one has to say so.
 */
export function pretendNarrow() {
  const wide = window.matchMedia
  window.matchMedia = answering((query) => query === narrowQuery) as typeof window.matchMedia
  return () => {
    window.matchMedia = wide
  }
}

window.ResizeObserver =
  window.ResizeObserver ||
  class {
    observe() {}
    unobserve() {}
    disconnect() {}
  }

window.scrollTo = window.scrollTo || (() => {})

// Recharts measures its container before drawing anything, and jsdom reports
// every element as zero by zero — so without these the chart renders nothing
// and every assertion about it fails for the wrong reason.
Object.defineProperty(HTMLElement.prototype, 'clientWidth', { configurable: true, value: 800 })
Object.defineProperty(HTMLElement.prototype, 'clientHeight', { configurable: true, value: 300 })
Object.defineProperty(HTMLElement.prototype, 'getBoundingClientRect', {
  configurable: true,
  value: () => ({ width: 800, height: 300, top: 0, left: 0, bottom: 300, right: 800, x: 0, y: 0 }),
})
