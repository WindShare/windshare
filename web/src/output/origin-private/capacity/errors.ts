/** Invalid persistence must never be interpreted as zero usage or repaired by arithmetic. */
export class OriginCapacityDataError extends DOMException {
  constructor(detail: string) {
    super(`Invalid origin capacity state: ${detail}`, 'DataError')
  }
}
