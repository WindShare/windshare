# Browser application runtime

`browser-share/` owns share joining, catalog paging, and reconnect supervision.
`receive/` owns the single `ReceiveCoordinator` orchestrating offer planning,
active transfers, and retained browser downloads over `output/`.
`network/` owns built-in network resolution, availability diagnostics, and abuse reporting.
`diagnostics/` connects runtime observations to the diagnostic bundle exporter.

The dependency boundary gate keeps `application/`, `transfer/`, and `output/`
independent of UI/React, keeps `transfer/` independent of `output/`, and limits
UI imports from `output/` to pure types in `output/model/`.
