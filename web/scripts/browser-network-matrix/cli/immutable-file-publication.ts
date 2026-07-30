export interface ImmutableTextFilePublication {
  readonly path: string
  readonly encoded: string
}

/** Publication is delegated to the explicit native helper authority. */
export interface ImmutableTextFilePublisher {
  publish(path: string, encoded: string): Promise<ImmutableTextFilePublication>
}
