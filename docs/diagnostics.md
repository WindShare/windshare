# Browser diagnostics

To capture a problem that happens when reopening a share:

1. Open WindShare and run `window.windshareDiagnostics.enable()` in the browser console.
2. Open the original, complete share link and reproduce the problem.
3. Use **Connection details → Developer diagnostics → Export diagnostics**, or run
   `copy(window.windshareDiagnostics.export())` in Chromium DevTools.
4. Run `window.windshareDiagnostics.disable()` when finished.

Enabling trace records a 30-minute activation deadline for this browser profile and site origin.
Reloads and new tabs restore capture before the receiver starts, using the original deadline.
Changing the host, port, browser profile, or private-browsing context does not share activation.
If browser storage is blocked, capture works only in the current page.

Each page has its own bounded, in-memory evidence and runtime identity. Export before leaving a
page whose evidence you need. A failure can seal that page's capture to preserve the surrounding
events; reopening within the activation window starts a fresh capture. `status()` reports the
current capture, `inspectLastFailure()` returns the last retained incident, and `clear()` removes
retained evidence without disabling activation. Manual `disable()` also prevents later pages
from restoring capture; already-open tabs retain their own capture state.

Failed lane transitions include `failure_detail`: bounded exception text with nested causes and
stack excerpts. Keep the sender trace from the same reproduction; `protocol_session_id` pairs
browser and sender events. Do not call `enable()` again before exporting, since it starts a new capture.
