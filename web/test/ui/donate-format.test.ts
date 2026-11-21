import { describe, expect, it } from 'vitest'
import { AFDIAN_QR_MODULES, AFDIAN_QR_PATH, decodeAfdianQrMatrix } from '../../src/ui/donate/afdian-qr'
import {
  OFFICIAL_DONATE_ORIGINS,
  OFFICIAL_DONATE_PAGE_URL,
  canCreateCryptoIntent,
} from '../../src/ui/donate/config'
import {
  countCodePoints,
  formatDonationAmount,
  formatRelativeTime,
  parseCryptoAmountInput,
} from '../../src/ui/donate/format'

describe('donate pure formatting and validation helpers', () => {
  it('formats fixed-point minor amounts accurately across currencies', () => {
    expect(formatDonationAmount(1000, 'CNY', 2)).toBe('¥10')
    expect(formatDonationAmount(1250, 'CNY', 2)).toBe('¥12.50')
    expect(formatDonationAmount(500, 'USD', 2)).toBe('$5')
    expect(formatDonationAmount(100500, 'USD', 2)).toBe('$1,005')
    expect(formatDonationAmount(1000, 'JPY', 0)).toBe('¥1,000')
    expect(formatDonationAmount(12345, 'CAD', 3)).toBe('CAD 12.345')
  })

  it('validates crypto USD inputs against minimum and maximum bounds', () => {
    expect(parseCryptoAmountInput('10')).toEqual({ ok: true, normalized: '10', amountMinor: 1000 })
    expect(parseCryptoAmountInput('5.50')).toEqual({ ok: true, normalized: '5.50', amountMinor: 550 })
    expect(parseCryptoAmountInput('4.99')).toEqual({ ok: false, reason: 'below_min' })
    expect(parseCryptoAmountInput('100000.01')).toEqual({ ok: false, reason: 'above_max' })
    expect(parseCryptoAmountInput('10.123')).toEqual({ ok: false, reason: 'invalid' })
    expect(parseCryptoAmountInput('')).toEqual({ ok: false, reason: 'invalid' })
  })

  it('formats bilingual relative timestamps and counts Unicode code points', () => {
    const now = 1_700_000_000
    expect(formatRelativeTime(now - 20, now, 'zh-CN')).toBe('刚刚')
    expect(formatRelativeTime(now - 20, now, 'en')).toBe('just now')
    expect(formatRelativeTime(now - 300, now, 'zh-CN')).toBe('5 分钟前')
    expect(formatRelativeTime(now - 7200, now, 'en')).toBe('2h ago')
    expect(formatRelativeTime(now - 3 * 86_400, now, 'en')).toBe('3d ago')
    expect(formatRelativeTime(now - 40 * 86_400, now, 'en')).toMatch(/^\d{4}-\d{2}-\d{2}$/)
    expect(countCodePoints('风😀')).toBe(2)
  })

  it('decodes the bundled compile-time Afdian QR matrix with intact finder patterns', () => {
    const matrix = decodeAfdianQrMatrix()
    expect(matrix).toHaveLength(AFDIAN_QR_MODULES * AFDIAN_QR_MODULES)
    const darkCount = matrix.reduce((sum, cell) => sum + cell, 0)
    expect(darkCount).toBe(538)
    // Top-left 7x7 finder border is all dark on row 0, columns 0..6.
    for (let x = 0; x < 7; x += 1) {
      expect(matrix[x]).toBe(1)
    }
    expect(AFDIAN_QR_PATH.startsWith('M4 4h7v1h-7z')).toBe(true)
  })
})

describe('donate crypto intent origin policy', () => {
  it('allows only hosted official origins by default and rejects localhost in production builds', () => {
    expect(OFFICIAL_DONATE_PAGE_URL).toBe('https://windshare.cc/donate')
    expect([...OFFICIAL_DONATE_ORIGINS]).toEqual([
      'https://ws.ourworld.top',
      'https://windshare.cc',
    ])

    expect(canCreateCryptoIntent('https://ws.ourworld.top')).toBe(true)
    expect(canCreateCryptoIntent('https://windshare.cc')).toBe(true)
    expect(canCreateCryptoIntent('http://localhost:5173')).toBe(false)
    expect(canCreateCryptoIntent('http://127.0.0.1:4173')).toBe(false)
    expect(canCreateCryptoIntent('https://share.example.com')).toBe(false)
    expect(canCreateCryptoIntent('https://ws.ourworld.top.evil.example')).toBe(false)
    expect(canCreateCryptoIntent('not-a-url')).toBe(false)
  })

  it('allows HTTP(S) loopback origins only when explicitly enabled for local development', () => {
    expect(canCreateCryptoIntent('http://localhost:5173', { allowLocalhost: true })).toBe(true)
    expect(canCreateCryptoIntent('http://127.0.0.1:4173', { allowLocalhost: true })).toBe(true)
    expect(canCreateCryptoIntent('https://localhost:8443', { allowLocalhost: true })).toBe(true)
    expect(canCreateCryptoIntent('file://localhost', { allowLocalhost: true })).toBe(false)
    expect(canCreateCryptoIntent('https://share.example.com', { allowLocalhost: true })).toBe(false)
  })
})
