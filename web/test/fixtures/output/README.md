# Browser output fixtures

These examples exercise browser receive intents, artifact choices, directory
admission, checkpoint integrity and recovery semantics. They belong to the browser
runtime and are verified by `make web`; Go does not generate or decode them.

When changing browser persistence, update these fixtures and their TypeScript tests
together. Incompatible database formats are discarded; preserving old bytes is not
a compatibility requirement. Keep wire interoperability cases in `core/testvectors/`.
