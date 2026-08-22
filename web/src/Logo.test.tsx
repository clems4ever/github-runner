import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import favicon from '../public/favicon.svg?raw'
import indexHtml from '../index.html?raw'
import { Logo } from './Logo'

describe('Logo', () => {
  it('names itself for anyone who cannot see it', () => {
    render(<Logo />)
    expect(screen.getByRole('img', { name: 'runner-fleet' })).toBeInTheDocument()
  })

  it('keeps quiet when it sits beside the word it draws', () => {
    // In the header the product name is already there in text; a screen reader
    // announcing "runner-fleet runner-fleet" is worse than silence.
    const { container } = render(<Logo title="" />)
    expect(screen.queryByRole('img')).not.toBeInTheDocument()
    expect(container.querySelector('title')).toBeNull()
  })

  it('does not let one instance repaint another', () => {
    // Both copies reference their gradients by id. Shared ids mean the second
    // definition wins for both, which is the kind of bug that only shows up
    // once someone puts the mark in two places.
    const { container } = render(
      <>
        <Logo />
        <Logo />
      </>,
    )
    const ids = [...container.querySelectorAll('linearGradient')].map((g) => g.id)
    expect(ids).toHaveLength(4)
    expect(new Set(ids).size).toBe(4)
    // A colon in a url() reference is legal but not universally understood.
    ids.forEach((id) => expect(id).not.toContain(':'))
  })

  it('points every gradient reference at a definition that exists', () => {
    const { container } = render(<Logo />)
    const defined = new Set([...container.querySelectorAll('linearGradient')].map((g) => g.id))
    const referenced = [...container.querySelectorAll('rect')]
      .map((el) => /^url\(#(.+)\)$/.exec(el.getAttribute('fill') ?? '')?.[1])
      .filter((id) => id !== undefined)
    expect(referenced).not.toHaveLength(0)
    referenced.forEach((id) => expect(defined).toContain(id))
  })

  it('draws the same mark as the favicon', () => {
    // The tab icon cannot be a React component, so the artwork lives twice.
    // This is what stops the two from drifting into different logos.
    const { container } = render(<Logo />)

    const paths = [...container.querySelectorAll('path')]
    expect(paths).toHaveLength(3)
    paths.forEach((path) => {
      expect(favicon).toContain(`d="${path.getAttribute('d')}"`)
      expect(favicon).toContain(`stroke-width="${path.getAttribute('stroke-width')}"`)
    })

    // And the same colours, so the tile does not change hue between the tab
    // and the header.
    ;[...container.querySelectorAll('stop')].forEach((stop) => {
      expect(favicon).toContain(`stop-color="${stop.getAttribute('stop-color')}"`)
    })
  })

  it('is linked from the page the daemon serves', () => {
    // Vite copies public/ into the embedded bundle, so a link that points
    // somewhere else would 404 through the SPA fallback and quietly serve
    // index.html as an image.
    expect(indexHtml).toContain('<link rel="icon" type="image/svg+xml" href="/favicon.svg" />')
  })
})
