import assert from "node:assert/strict";
import test from "node:test";

import {
  contractOutputs,
  deriveChangeDecision,
  isDocumentationPath,
  parseNameStatusZ,
  selectChangeRange,
} from "./changes.mjs";

const TARGET_SHA = "a".repeat(40);
const BASE_SHA = "b".repeat(40);
const HEAD_SHA = "c".repeat(40);

function context(overrides = {}) {
  return {
    eventName: "pull_request",
    targetSHA: TARGET_SHA,
    pullRequestBaseSHA: BASE_SHA,
    pullRequestHeadSHA: HEAD_SHA,
    pushBeforeSHA: "",
    ...overrides,
  };
}

test("pull requests use the tested base/head three-dot range", () => {
  let observedRange;
  const decision = deriveChangeDecision(context(), (range) => {
    observedRange = range;
    return ["core/session/session.go"];
  });

  assert.deepEqual(observedRange, {
    target_sha: TARGET_SHA,
    diff_base_sha: BASE_SHA,
    diff_head_sha: HEAD_SHA,
    range_kind: "three-dot",
  });
  assert.equal(decision.documentation_only, "false");
  assert.equal(decision.validation_required, "true");
});

test("pushes use push-before through the tested target SHA", () => {
  const decision = deriveChangeDecision(
    context({
      eventName: "push",
      pullRequestBaseSHA: "",
      pullRequestHeadSHA: "",
      pushBeforeSHA: BASE_SHA,
    }),
    () => ["README.md"],
  );

  assert.equal(decision.range_kind, "two-dot");
  assert.equal(decision.diff_base_sha, BASE_SHA);
  assert.equal(decision.diff_head_sha, TARGET_SHA);
  assert.equal(decision.documentation_only, "true");
  assert.equal(decision.validation_required, "false");
});

test("documentation-only recognizes docs and Markdown at any depth", () => {
  const decision = deriveChangeDecision(context(), () => [
    "docs/security.md",
    "README.md",
    "web/README.md",
  ]);

  assert.equal(decision.documentation_only, "true");
  assert.equal(decision.validation_required, "false");
  assert.equal(isDocumentationPath("docs/image.png"), true);
  assert.equal(isDocumentationPath("src/guide.md"), true);
  assert.equal(isDocumentationPath("src/guide.MD"), false);
});

test("rename parsing returns both source and destination paths", () => {
  const paths = parseNameStatusZ(
    Buffer.from("R100\0docs/old.md\0src/new.go\0M\0README.md\0"),
  );
  assert.deepEqual(paths, ["docs/old.md", "src/new.go", "README.md"]);

  const decision = deriveChangeDecision(context(), () => paths);
  assert.equal(decision.documentation_only, "false");
  assert.equal(decision.validation_required, "true");
});

test("manual dispatch always selects the full validation set", () => {
  const decision = deriveChangeDecision(
    context({ eventName: "workflow_dispatch" }),
    () => assert.fail("manual dispatch must not read a diff"),
  );

  assert.deepEqual(contractOutputs(decision), {
    target_sha: TARGET_SHA,
    diff_base_sha: TARGET_SHA,
    diff_head_sha: TARGET_SHA,
    range_kind: "all",
    documentation_only: "false",
    validation_required: "true",
  });
});

test("uncertain ranges fail closed to the full validation set", () => {
  const scenarios = [
    context({
      eventName: "push",
      pullRequestBaseSHA: "",
      pullRequestHeadSHA: "",
      pushBeforeSHA: "0".repeat(40),
    }),
    context({ eventName: "pull_request", pullRequestHeadSHA: "" }),
    context({ eventName: "schedule" }),
  ];

  for (const scenario of scenarios) {
    const decision = deriveChangeDecision(scenario, () =>
      assert.fail("an invalid range must not be diffed"),
    );
    assert.equal(decision.range_kind, "all");
    assert.equal(decision.validation_required, "true");
  }
});

test("empty, unavailable, and malformed diffs fail closed", () => {
  const emptyDecision = deriveChangeDecision(context(), () => []);
  assert.equal(emptyDecision.range_kind, "all");
  assert.equal(emptyDecision.validation_required, "true");

  const unavailableDecision = deriveChangeDecision(context(), () => {
    throw new Error("endpoint missing");
  });
  assert.equal(unavailableDecision.range_kind, "all");
  assert.match(unavailableDecision.reason, /endpoint missing/);

  assert.throws(
    () => parseNameStatusZ(Buffer.from("M\0unterminated")),
    /not NUL terminated/,
  );
  assert.throws(
    () => parseNameStatusZ(Buffer.from([0x4d, 0x00, 0xff, 0x00])),
    /encoded data was not valid|valid for encoding/i,
  );
});

test("invalid target identity fails instead of emitting ambiguous outputs", () => {
  assert.throws(
    () => selectChangeRange(context({ targetSHA: "HEAD" })),
    /exact lowercase 40-hex/,
  );
});
