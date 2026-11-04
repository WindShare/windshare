import type { ISOFile } from 'mp4box'

// This is the sole adapter for typed but undocumented mp4box 2.4.1 state.
// Revalidate mp4-range and media-preview-edit-list browser contracts on upgrades.
export function initializeKeyframeFragment(
  file: ISOFile<number, unknown>,
  trackId: number,
  sampleIndex: number,
): ArrayBuffer | undefined {
  file.setSegmentOptions(trackId, trackId, {
    nbSamples: 1,
    nbSamplesPerFragment: 1,
    rapAlignement: false,
  })
  const track = file.getTrackById(trackId)
  // A single-sample decoder must not reapply the original clip's edits. Browsers
  // disagree about those edits; the source presentation clock stays outside it.
  if (track.boxes !== undefined) track.boxes = track.boxes.filter((box) => box.type !== 'edts')
  delete track.edts
  track.edtss = []
  const initialization = file.initializeSegmentation('per-track')
    .find((candidate) => candidate.id === trackId)?.buffer
  // Time-based seek compares floating CTS and can skip an open-GOP tail entirely.
  // Position by the already selected sample, on both fragmentation cursors.
  track.nextSample = sampleIndex
  file.resetFragmentedTrackStateAfterSeek(track, sampleIndex)
  return initialization
}
