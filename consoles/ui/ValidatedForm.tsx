import { useLayoutEffect, useRef, type ComponentPropsWithoutRef } from 'react'
import type { Locale } from './locale'
import { sharedCopy } from './locales'
import { useLocale } from './use-locale'

type Field = HTMLInputElement | HTMLTextAreaElement | HTMLSelectElement

function isField(element: EventTarget | null): element is Field {
  return element instanceof HTMLInputElement || element instanceof HTMLTextAreaElement || element instanceof HTMLSelectElement
}

function validationMessage(field: Field, locale: Locale): string {
  const t = sharedCopy[locale]
  const state = field.validity
  if (state.badInput) return t.invalidNumber
  if (state.valueMissing) return t.requiredField
  if (state.patternMismatch) return field.title
  if (state.tooShort && 'minLength' in field) return t.minLength(field.minLength)
  if (state.tooLong && 'maxLength' in field) return t.maxLength(field.maxLength)
  if (field instanceof HTMLInputElement) {
    const bound = (raw: string) => Number(raw).toLocaleString(locale, { maximumSignificantDigits: 21 })
    if (state.rangeUnderflow) return t.minValue(bound(field.min))
    if (state.rangeOverflow) return t.maxValue(bound(field.max))
    if (state.stepMismatch) return t.stepValue(bound(field.step || '1'))
  }
  return ''
}

function localizeValidity(field: Field, locale: Locale) {
  field.setCustomValidity('')
  if (!field.validity.valid) field.setCustomValidity(validationMessage(field, locale))
}

export function ValidatedForm({ onSubmit, onInput, ...props }: Omit<ComponentPropsWithoutRef<'form'>, 'noValidate'>) {
  const { locale } = useLocale()
  const ref = useRef<HTMLFormElement>(null)
  useLayoutEffect(() => {
    // A language switch or a restored draft can change a previously reported error without an input event.
    for (const field of ref.current!.elements) {
      if (isField(field) && field.validity.customError) localizeValidity(field, locale)
    }
  })
  return <form {...props} ref={ref} noValidate onInput={event => {
    if (isField(event.target)) event.target.setCustomValidity('')
    onInput?.(event)
  }} onSubmit={event => {
    // Apply the current language before the browser focuses and reports the first invalid control.
    const form = event.currentTarget
    for (const field of form.elements) {
      if (isField(field) && field.willValidate) localizeValidity(field, locale)
    }
    if (form.reportValidity()) onSubmit?.(event)
    else event.preventDefault()
  }} />
}
