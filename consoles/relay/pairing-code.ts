export function deviceCode(value: string) {
  return value.trim().toUpperCase().replace(/^([A-Z2-9]{4})([A-Z2-9]{4})$/, '$1-$2')
}
