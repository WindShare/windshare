import { createHash } from "node:crypto";
var PLAYWRIGHT_OUTCOMES = Object.freeze([
	"not-started",
	"passed",
	"failed"
]);
var PION_APPLICABILITY = Object.freeze([
	"unknown",
	"applicable",
	"not-applicable"
]);
var NATIVE_INTEROP_OUTCOMES = Object.freeze([
	"not-started",
	"succeeded",
	"failed"
]);
var NATIVE_INTEROP_FAILURE_CODES = Object.freeze([
	"peer-construction",
	"negotiation",
	"datachannel",
	"interop-deadline",
	"selected-pair",
	"protocol",
	"unexpected"
]);
var DELIVERY_TERMINALS = Object.freeze(["succeeded", "failed"]);
var MAIN_TRANSFER_BYTES = 16777216;
var MAIN_TRANSFER_SHA256 = "25e349f1212bb99491944eb8e885665bb71edc5d5db49d1cd2ef1ffafac1dd5d";
var ARTIFACT_KINDS = Object.freeze([
	"trace",
	"video",
	"screenshot",
	"error-context",
	"console-log",
	"runner-stdout",
	"runner-stderr",
	"process-log",
	"attempt-evidence",
	"native-interop-evidence",
	"result-diagnostic"
]);
var TEXT_ENCODER = new TextEncoder();
var BrowserEvidenceContractError = class extends Error {
	constructor(message, options) {
		super(`browser evidence contract: ${message}`, options);
		this.name = "BrowserEvidenceContractError";
	}
};
function contractError(message) {
	throw new BrowserEvidenceContractError(message);
}
function requireRecord(value, label) {
	if (typeof value !== "object" || value === null || Array.isArray(value)) contractError(`${label} must be an object`);
	const prototype = Object.getPrototypeOf(value);
	if (prototype !== Object.prototype && prototype !== null) contractError(`${label} must be a plain JSON object`);
	for (const key of Reflect.ownKeys(value)) {
		if (typeof key !== "string") contractError(`${label} contains a non-JSON symbol field`);
		const descriptor = Object.getOwnPropertyDescriptor(value, key);
		if (descriptor === void 0 || !descriptor.enumerable || !Object.hasOwn(descriptor, "value")) contractError(`${label} field ${JSON.stringify(key)} must be an enumerable JSON data field`);
	}
	return value;
}
function requireExactKeys(value, required, optional, label) {
	const allowed = /* @__PURE__ */ new Set([...required, ...optional]);
	for (const key of Object.keys(value)) if (!allowed.has(key)) contractError(`${label} contains unknown field ${JSON.stringify(key)}`);
	for (const key of required) if (!Object.hasOwn(value, key)) contractError(`${label} is missing field ${JSON.stringify(key)}`);
}
function requireLiteral(value, expected, label) {
	if (value !== expected) contractError(`${label} must be ${JSON.stringify(expected)}`);
	return expected;
}
function requireEnum(value, allowed, label) {
	if (typeof value !== "string" || !allowed.includes(value)) contractError(`${label} is outside the frozen vocabulary`);
	return value;
}
function requireString(value, label, maximumUtf8Bytes) {
	if (typeof value !== "string" || value.length === 0 || value.normalize("NFC") !== value || TEXT_ENCODER.encode(value).byteLength > maximumUtf8Bytes) contractError(`${label} must be non-empty NFC text within ${maximumUtf8Bytes} UTF-8 bytes`);
	return value;
}
function requireSafeInteger(value, minimum, maximum, label) {
	if (!Number.isSafeInteger(value) || value < minimum || value > maximum) contractError(`${label} must be an integer in [${minimum}, ${maximum}]`);
	return value;
}
function requireBoolean(value, label) {
	if (typeof value !== "boolean") contractError(`${label} must be boolean`);
	return value;
}
function requireArray(value, label) {
	if (!Array.isArray(value)) contractError(`${label} must be an array`);
	for (const key of Reflect.ownKeys(value)) {
		if (key === "length") continue;
		if (typeof key !== "string" || !/^(0|[1-9]\d*)$/u.test(key) || Number(key) >= value.length) contractError(`${label} contains a non-JSON array field`);
		const descriptor = Object.getOwnPropertyDescriptor(value, key);
		if (descriptor === void 0 || !descriptor.enumerable || !Object.hasOwn(descriptor, "value")) contractError(`${label} index ${key} must be an enumerable JSON data field`);
	}
	for (let index = 0; index < value.length; index += 1) if (!Object.hasOwn(value, index)) contractError(`${label} must not contain sparse entries`);
	return value;
}
function requireCanonicalIdentity(value, label) {
	if (typeof value !== "string" || !/^[A-Za-z0-9_-]{21}[AQgw]$/u.test(value) || value === "AAAAAAAAAAAAAAAAAAAAAA") contractError(`${label} must be canonical unpadded base64url for a nonzero 16-byte identity`);
	return value;
}
function requireSha256(value, label) {
	if (typeof value !== "string" || !/^[0-9a-f]{64}$/u.test(value)) contractError(`${label} must be a lowercase SHA-256 digest`);
	return value;
}
function requireCheckoutSha(value, label) {
	if (typeof value !== "string" || !/^[0-9a-f]{40}$/u.test(value)) contractError(`${label} must be a lowercase 40-hex checkout SHA`);
	return value;
}
function requireDecimalUint64(value, label) {
	if (typeof value !== "string" || !/^[1-9]\d{0,19}$/u.test(value)) contractError(`${label} must be a positive canonical decimal uint64 string`);
	if (BigInt(value) > 18446744073709551615n) contractError(`${label} exceeds uint64`);
	return value;
}
function optionalField(value, key) {
	if (!Object.hasOwn(value, key)) return void 0;
	const field = value[key];
	if (field === void 0) contractError(`field ${JSON.stringify(key)} must not be undefined`);
	return field;
}
function freezeRecord(value) {
	for (const item of Object.values(value)) if (typeof item === "object" && item !== null && !Object.isFrozen(item)) Object.freeze(item);
	return Object.freeze(value);
}
var RUNNER_PROCESS_TERMINALS = Object.freeze([
	"not-started",
	"running-at-collection",
	"spawn-failed",
	"exited",
	"signaled"
]);
var RUNNER_MAXIMUM_EXIT_CODE = 4294967295;
function parseExecutionEvidence(value) {
	const evidence = requireRecord(value, "execution evidence");
	requireExactKeys(evidence, [
		"pageCrashed",
		"targetCrashed",
		"unexpectedBrowserDisconnect",
		"infrastructureFailure",
		"lifecycleCompleted",
		"runnerProcess"
	], [], "execution evidence");
	const parsed = freezeRecord({
		pageCrashed: requireBoolean(evidence.pageCrashed, "page crashed evidence"),
		targetCrashed: requireBoolean(evidence.targetCrashed, "target crashed evidence"),
		unexpectedBrowserDisconnect: requireBoolean(evidence.unexpectedBrowserDisconnect, "unexpected browser disconnect evidence"),
		infrastructureFailure: requireBoolean(evidence.infrastructureFailure, "infrastructure failure evidence"),
		lifecycleCompleted: requireBoolean(evidence.lifecycleCompleted, "execution lifecycle completion evidence"),
		runnerProcess: parseRunnerProcessEvidence(evidence.runnerProcess)
	});
	validateExecutionConsistency(parsed);
	return parsed;
}
/** Assertion failures remain a Playwright verdict; only raw lifecycle and process
* facts participate in execution classification. */
function classifyExecutionOutcome(evidence) {
	if (evidence.pageCrashed || evidence.targetCrashed || evidence.unexpectedBrowserDisconnect) return "crashed";
	if (evidence.infrastructureFailure || evidence.runnerProcess.terminal === "spawn-failed" || evidence.runnerProcess.terminal === "signaled") return "infrastructure-failed";
	if (evidence.lifecycleCompleted && evidence.runnerProcess.terminal === "exited") return "healthy";
	return "unknown";
}
function validateRunnerProcessVerdict(resultStatus, playwrightOutcome, evidence) {
	const process = evidence.runnerProcess;
	if (resultStatus === "provisional") {
		if (process.terminal !== "not-started") contractError("provisional result cannot assert runner process termination");
		return;
	}
	if (playwrightOutcome === "not-started") {
		if (process.terminal === "exited" && process.exitCode === 0) contractError("not-started Playwright outcome contradicts runner exit code zero");
		return;
	}
	if (playwrightOutcome === "passed") {
		if (process.terminal !== "exited" || process.exitCode !== 0) contractError("passed Playwright outcome requires runner exit code zero");
		return;
	}
	if (process.terminal !== "signaled" && (process.terminal !== "exited" || process.exitCode === 0)) contractError("failed Playwright outcome requires a nonzero or signaled runner terminal");
}
function parseRunnerProcessEvidence(value) {
	const process = requireRecord(value, "runner process evidence");
	const terminal = requireEnum(process.terminal, RUNNER_PROCESS_TERMINALS, "runner process terminal");
	if (terminal === "spawn-failed") {
		requireExactKeys(process, [
			"terminal",
			"errorCode",
			"errorMessage"
		], [], "runner process evidence");
		return freezeRecord({
			terminal,
			errorCode: requirePortableToken$1(process.errorCode, "runner spawn error code"),
			errorMessage: requireString(process.errorMessage, "runner spawn error message", 512)
		});
	}
	if (terminal === "exited") {
		requireExactKeys(process, ["terminal", "exitCode"], [], "runner process evidence");
		return freezeRecord({
			terminal,
			exitCode: requireSafeInteger(process.exitCode, 0, RUNNER_MAXIMUM_EXIT_CODE, "runner exit code")
		});
	}
	if (terminal === "signaled") {
		requireExactKeys(process, ["terminal", "signal"], [], "runner process evidence");
		return freezeRecord({
			terminal,
			signal: requirePortableToken$1(process.signal, "runner signal")
		});
	}
	requireExactKeys(process, ["terminal"], [], "runner process evidence");
	return freezeRecord({ terminal });
}
function validateExecutionConsistency(evidence) {
	const process = evidence.runnerProcess;
	const browserRuntimeEvidence = evidence.pageCrashed || evidence.targetCrashed || evidence.unexpectedBrowserDisconnect;
	if (process.terminal === "not-started" && (browserRuntimeEvidence || evidence.lifecycleCompleted)) contractError("runner that never started cannot carry browser lifecycle evidence");
	if (process.terminal === "running-at-collection" && evidence.lifecycleCompleted) contractError("running runner cannot claim a completed execution lifecycle");
	if (process.terminal === "spawn-failed") validateSpawnFailure(evidence, browserRuntimeEvidence);
	if (process.terminal === "signaled") validateSignalTermination(evidence, browserRuntimeEvidence);
	if (evidence.lifecycleCompleted && process.terminal !== "exited") contractError("completed execution lifecycle requires an exited runner process");
}
function validateSpawnFailure(evidence, browserRuntimeEvidence) {
	if (!evidence.infrastructureFailure) contractError("runner spawn failure must be classified as infrastructure evidence");
	if (browserRuntimeEvidence || evidence.lifecycleCompleted) contractError("runner spawn failure cannot carry browser lifecycle evidence");
}
function validateSignalTermination(evidence, browserRuntimeEvidence) {
	if (evidence.lifecycleCompleted) contractError("signaled runner cannot claim a completed execution lifecycle");
	if (!browserRuntimeEvidence && !evidence.infrastructureFailure) contractError("signal termination without browser crash evidence is infrastructure failure");
}
function requirePortableToken$1(value, label) {
	const token = requireString(value, label, 128);
	if (!/^[A-Za-z0-9._-]+$/u.test(token)) contractError(`${label} contains non-portable characters`);
	return token;
}
var JSON_WHITESPACE = /* @__PURE__ */ new Set([
	" ",
	"	",
	"\n",
	"\r"
]);
/**
* Evidence schemas contain integers only. Rejecting alternate numeric spellings
* and duplicate decoded member names keeps Go and JavaScript on one semantic
* input language instead of relying on their different JSON coercion rules.
*/
function parseCanonicalJsonText(encoded, label) {
	return new CanonicalJsonDecoder(encoded, label).decode();
}
var CanonicalJsonDecoder = class {
	#encoded;
	#label;
	#offset = 0;
	constructor(encoded, label) {
		this.#encoded = encoded;
		this.#label = label;
	}
	decode() {
		this.#skipWhitespace();
		const value = this.#value();
		this.#skipWhitespace();
		if (this.#offset !== this.#encoded.length) this.#fail("contains trailing data");
		return value;
	}
	#value() {
		const character = this.#encoded[this.#offset];
		if (character === "{") return this.#object();
		if (character === "[") return this.#array();
		if (character === "\"") return this.#string();
		if (character === "t") return this.#literal("true", true);
		if (character === "f") return this.#literal("false", false);
		if (character === "n") return this.#literal("null", null);
		return this.#integer();
	}
	#object() {
		this.#offset += 1;
		this.#skipWhitespace();
		const result = {};
		const names = /* @__PURE__ */ new Set();
		if (this.#consume("}")) return result;
		while (true) {
			if (this.#encoded[this.#offset] !== "\"") this.#fail("object member name must be a string");
			const name = this.#string();
			if (names.has(name)) this.#fail(`contains duplicate object member ${JSON.stringify(name)}`);
			names.add(name);
			this.#skipWhitespace();
			if (!this.#consume(":")) this.#fail("object member lacks a colon");
			this.#skipWhitespace();
			Object.defineProperty(result, name, {
				value: this.#value(),
				enumerable: true,
				configurable: true,
				writable: true
			});
			this.#skipWhitespace();
			if (this.#consume("}")) return result;
			if (!this.#consume(",")) this.#fail("object members must be comma-separated");
			this.#skipWhitespace();
		}
	}
	#array() {
		this.#offset += 1;
		this.#skipWhitespace();
		const result = [];
		if (this.#consume("]")) return result;
		while (true) {
			result.push(this.#value());
			this.#skipWhitespace();
			if (this.#consume("]")) return result;
			if (!this.#consume(",")) this.#fail("array entries must be comma-separated");
			this.#skipWhitespace();
		}
	}
	#string() {
		const start = this.#offset;
		this.#offset += 1;
		let escaped = false;
		while (this.#offset < this.#encoded.length) {
			const character = this.#encoded[this.#offset];
			this.#offset += 1;
			if (escaped) {
				escaped = false;
				continue;
			}
			if (character === "\\") {
				escaped = true;
				continue;
			}
			if (character === "\"") {
				const raw = this.#encoded.slice(start, this.#offset);
				let decoded;
				try {
					decoded = JSON.parse(raw);
				} catch (cause) {
					this.#fail(`contains an invalid JSON string: ${String(cause)}`);
				}
				if (!isWellFormedUnicode(decoded) || decoded.includes("�")) this.#fail("contains invalid or replacement Unicode");
				return decoded;
			}
		}
		this.#fail("contains an unterminated string");
	}
	#integer() {
		const remainder = this.#encoded.slice(this.#offset);
		const match = /^-?(0|[1-9]\d*)/u.exec(remainder);
		if (match === null) this.#fail("contains a non-canonical integer token");
		const token = match[0];
		if (token === "-0") this.#fail("contains a non-canonical integer token");
		const following = remainder[token.length];
		if (following !== void 0 && /[.eE+\d]/u.test(following)) this.#fail("contains a non-canonical integer token");
		const value = Number(token);
		if (!Number.isSafeInteger(value)) this.#fail("contains an unsafe integer");
		this.#offset += token.length;
		return value;
	}
	#literal(encoded, value) {
		if (!this.#encoded.startsWith(encoded, this.#offset)) this.#fail("contains an invalid literal");
		this.#offset += encoded.length;
		return value;
	}
	#skipWhitespace() {
		while (JSON_WHITESPACE.has(this.#encoded[this.#offset] ?? "")) this.#offset += 1;
	}
	#consume(expected) {
		if (this.#encoded[this.#offset] !== expected) return false;
		this.#offset += 1;
		return true;
	}
	#fail(reason) {
		contractError(`${this.#label} ${reason} at UTF-16 offset ${this.#offset}`);
	}
};
function isWellFormedUnicode(value) {
	for (let index = 0; index < value.length; index += 1) {
		const unit = value.charCodeAt(index);
		if (unit >= 55296 && unit <= 56319) {
			const next = value.charCodeAt(index + 1);
			if (!Number.isInteger(next) || next < 56320 || next > 57343) return false;
			index += 1;
		} else if (unit >= 56320 && unit <= 57343) return false;
	}
	return true;
}
var PR_TEST_ICE_TOPOLOGY_ID = "pr-same-host-kernel-route-ipv4";
var TEST_ICE_SOURCE_SELECTOR_ALGORITHM = "udp-connect-source-consensus-v1";
var TEST_ICE_PROBE_DESTINATIONS = Object.freeze([
	Object.freeze({
		address: "192.0.2.1",
		port: 9
	}),
	Object.freeze({
		address: "198.51.100.1",
		port: 9
	}),
	Object.freeze({
		address: "203.0.113.1",
		port: 9
	})
]);
var TEST_ICE_ADDRESS_FAMILIES = Object.freeze(["ipv4"]);
var TEST_ICE_TRANSPORT_POLICIES = Object.freeze(["all"]);
var TEST_ICE_SELECTED_PAIR_TYPES = Object.freeze(["host", "prflx"]);
var TEST_ICE_PROTOCOLS = Object.freeze(["udp"]);
var VERIFIED_TEST_ICE_TOPOLOGY_LOCKS = /* @__PURE__ */ new WeakSet();
function parseTestIceTopology(value) {
	const profile = requireRecord(value, "test ICE topology");
	requireExactKeys(profile, [
		"topologyProfileSchemaVersion",
		"topologyId",
		"sourceSelector",
		"addressFamily",
		"rtcConfiguration",
		"candidatePolicy"
	], [], "test ICE topology");
	const selector = requireRecord(profile.sourceSelector, "test ICE source selector");
	requireExactKeys(selector, ["algorithm", "probeDestinations"], [], "test ICE source selector");
	requireExactProbeDestinations(selector.probeDestinations);
	const rtc = requireRecord(profile.rtcConfiguration, "test ICE RTC configuration");
	requireExactKeys(rtc, ["iceServers", "iceTransportPolicy"], [], "test ICE RTC configuration");
	if (requireArray(rtc.iceServers, "test ICE servers").length !== 0) contractError("PR test topology cannot use STUN or TURN servers");
	const policy = requireRecord(profile.candidatePolicy, "test ICE candidate policy");
	requireExactKeys(policy, ["allowedSelectedPairTypes", "allowedProtocols"], [], "test ICE candidate policy");
	requireExactVocabulary(policy.allowedSelectedPairTypes, TEST_ICE_SELECTED_PAIR_TYPES, "allowed selected-pair types");
	requireExactVocabulary(policy.allowedProtocols, TEST_ICE_PROTOCOLS, "allowed ICE protocols");
	return freezeRecord({
		topologyProfileSchemaVersion: requireLiteral(profile.topologyProfileSchemaVersion, 1, "test ICE topology profile schema version"),
		topologyId: requireLiteral(profile.topologyId, PR_TEST_ICE_TOPOLOGY_ID, "test ICE topology ID"),
		sourceSelector: freezeRecord({
			algorithm: requireLiteral(selector.algorithm, TEST_ICE_SOURCE_SELECTOR_ALGORITHM, "test ICE source selector algorithm"),
			probeDestinations: TEST_ICE_PROBE_DESTINATIONS
		}),
		addressFamily: requireEnum(profile.addressFamily, TEST_ICE_ADDRESS_FAMILIES, "test ICE address family"),
		rtcConfiguration: freezeRecord({
			iceServers: Object.freeze([]),
			iceTransportPolicy: requireEnum(rtc.iceTransportPolicy, TEST_ICE_TRANSPORT_POLICIES, "test ICE transport policy")
		}),
		candidatePolicy: freezeRecord({
			allowedSelectedPairTypes: TEST_ICE_SELECTED_PAIR_TYPES,
			allowedProtocols: TEST_ICE_PROTOCOLS
		})
	});
}
function parseTestIceTopologyResolution(value, profile, expectedProfileSha256) {
	const parsedProfile = parseTestIceTopology(profile);
	const profileSha256 = requireSha256(expectedProfileSha256, "expected topology profile SHA-256");
	const resolution = requireRecord(value, "test ICE topology resolution");
	requireExactKeys(resolution, [
		"topologyResolutionSchemaVersion",
		"topologyId",
		"topologyProfileSha256",
		"selectorAlgorithm",
		"addressFamily",
		"probeResults",
		"interface"
	], [], "test ICE topology resolution");
	const probeResults = parseProbeResults(resolution.probeResults, parsedProfile);
	const resolvedInterface = parseResolvedInterface(resolution.interface);
	if (probeResults.some((probe) => probe.sourceAddress !== resolvedInterface.selectedAddress)) contractError("test ICE route probes do not unanimously select the resolved interface address");
	if (!resolvedInterface.eligibleAddresses.some((candidate) => candidate.address === resolvedInterface.selectedAddress)) contractError("test ICE selected address is absent from the frozen eligible address inventory");
	return freezeRecord({
		topologyResolutionSchemaVersion: requireLiteral(resolution.topologyResolutionSchemaVersion, 1, "test ICE topology resolution schema version"),
		topologyId: requireLiteral(resolution.topologyId, parsedProfile.topologyId, "test ICE topology resolution ID"),
		topologyProfileSha256: requireLiteral(resolution.topologyProfileSha256, profileSha256, "test ICE topology resolution profile SHA-256"),
		selectorAlgorithm: requireLiteral(resolution.selectorAlgorithm, parsedProfile.sourceSelector.algorithm, "test ICE topology resolution selector algorithm"),
		addressFamily: requireLiteral(resolution.addressFamily, parsedProfile.addressFamily, "test ICE topology resolution address family"),
		probeResults,
		interface: resolvedInterface
	});
}
function parseTestIceTopologyJson(encoded) {
	const profile = parseTestIceTopology(parseCanonicalJsonText(encoded, "test ICE topology"));
	if (encoded !== canonicalTestIceTopologyJson(profile)) contractError("test ICE topology bytes must equal the exact canonical profile encoding");
	return profile;
}
function parseTestIceTopologyResolutionJson(encoded, profile, expectedProfileSha256) {
	const resolution = parseTestIceTopologyResolution(parseCanonicalJsonText(encoded, "test ICE topology resolution"), profile, expectedProfileSha256);
	if (encoded !== canonicalTestIceTopologyResolutionJson(resolution, profile, expectedProfileSha256)) contractError("test ICE topology resolution bytes must equal the exact canonical encoding");
	return resolution;
}
function canonicalTestIceTopologyJson(profile) {
	return JSON.stringify(parseTestIceTopology(profile));
}
function canonicalTestIceTopologyResolutionJson(resolution, profile, expectedProfileSha256) {
	return JSON.stringify(parseTestIceTopologyResolution(resolution, profile, expectedProfileSha256));
}
async function testIceTopologySha256(profile) {
	return sha256(canonicalTestIceTopologyJson(profile));
}
async function testIceTopologyResolutionSha256(resolution, profile, expectedProfileSha256) {
	return sha256(canonicalTestIceTopologyResolutionJson(resolution, profile, expectedProfileSha256));
}
async function verifyTestIceTopologyLock(profile, resolution, expectedProfileSha256, expectedResolutionSha256) {
	const parsedProfile = parseTestIceTopology(profile);
	const profileSha256 = requireSha256(expectedProfileSha256, "expected topology profile SHA-256");
	if (await testIceTopologySha256(parsedProfile) !== profileSha256) contractError("expected topology profile SHA-256 does not match the canonical profile");
	const parsedResolution = parseTestIceTopologyResolution(resolution, parsedProfile, profileSha256);
	const resolutionSha256 = requireSha256(expectedResolutionSha256, "expected topology resolution SHA-256");
	if (await testIceTopologyResolutionSha256(parsedResolution, parsedProfile, profileSha256) !== resolutionSha256) contractError("expected topology resolution SHA-256 does not match the canonical resolution");
	const lock = freezeRecord({
		profile: parsedProfile,
		resolution: parsedResolution,
		profileSha256,
		resolutionSha256
	});
	VERIFIED_TEST_ICE_TOPOLOGY_LOCKS.add(lock);
	return lock;
}
function readVerifiedTestIceTopologyLock(value) {
	if (typeof value !== "object" || value === null || !VERIFIED_TEST_ICE_TOPOLOGY_LOCKS.has(value)) contractError("browser evidence requires a canonically verified test ICE topology lock");
	return value;
}
function selectedPairAllowedByTopology(pair, profile, resolution) {
	const parsedProfile = parseTestIceTopology(profile);
	const parsedResolution = parseTestIceTopologyResolution(resolution, parsedProfile, resolution.topologyProfileSha256);
	if (![pair.local, pair.remote].every((candidate) => parsedProfile.candidatePolicy.allowedSelectedPairTypes.includes(candidate.candidateType) && parsedProfile.candidatePolicy.allowedProtocols.includes(candidate.protocol))) return false;
	const localHasFamily = Object.hasOwn(pair.local, "addressFamily");
	const remoteHasFamily = Object.hasOwn(pair.remote, "addressFamily");
	if (!localHasFamily && !remoteHasFamily) return true;
	if (!localHasFamily || !remoteHasFamily) return false;
	const pion = pair;
	const eligible = new Set(parsedResolution.interface.eligibleAddresses.map(({ address }) => address));
	return pion.local.addressFamily === parsedProfile.addressFamily && pion.remote.addressFamily === parsedProfile.addressFamily && pion.local.address === parsedResolution.interface.selectedAddress && eligible.has(pion.remote.address);
}
function parseProbeResults(value, profile) {
	const results = requireArray(value, "test ICE probe results");
	if (results.length !== profile.sourceSelector.probeDestinations.length) contractError("test ICE resolution must record every frozen route probe exactly once");
	return Object.freeze(results.map((value, index) => {
		const result = requireRecord(value, `test ICE probe result ${index}`);
		requireExactKeys(result, [
			"destinationAddress",
			"destinationPort",
			"sourceAddress"
		], [], `test ICE probe result ${index}`);
		const expected = profile.sourceSelector.probeDestinations[index];
		if (expected === void 0) contractError("test ICE probe result exceeds the frozen probe registry");
		return freezeRecord({
			destinationAddress: requireLiteral(result.destinationAddress, expected.address, `test ICE probe result ${index} destination`),
			destinationPort: requireLiteral(result.destinationPort, expected.port, `test ICE probe result ${index} port`),
			sourceAddress: requireOperationalIPv4(result.sourceAddress, `test ICE probe result ${index} source address`)
		});
	}));
}
function parseResolvedInterface(value) {
	const resolved = requireRecord(value, "test ICE resolved interface");
	requireExactKeys(resolved, [
		"index",
		"name",
		"selectedAddress",
		"eligibleAddresses"
	], [], "test ICE resolved interface");
	const eligibleAddresses = requireArray(resolved.eligibleAddresses, "test ICE eligible addresses").map((value, index) => {
		const candidate = requireRecord(value, `test ICE eligible address ${index}`);
		requireExactKeys(candidate, ["address", "prefixLength"], [], `test ICE eligible address ${index}`);
		return freezeRecord({
			address: requireOperationalIPv4(candidate.address, `test ICE eligible address ${index}`),
			prefixLength: requireSafeInteger(candidate.prefixLength, 1, 32, `test ICE eligible address ${index} prefix length`)
		});
	});
	if (new Set(eligibleAddresses.map(({ address }) => address)).size !== eligibleAddresses.length) contractError("test ICE eligible address inventory contains duplicates");
	const ordered = [...eligibleAddresses].sort((left, right) => ipv4Number(left.address) - ipv4Number(right.address) || left.prefixLength - right.prefixLength);
	if (eligibleAddresses.some((candidate, index) => candidate !== ordered[index])) contractError("test ICE eligible address inventory must use canonical numeric ordering");
	return freezeRecord({
		index: requireSafeInteger(resolved.index, 1, 4294967295, "test ICE interface index"),
		name: requireString(resolved.name, "test ICE interface name", 255),
		selectedAddress: requireOperationalIPv4(resolved.selectedAddress, "test ICE selected interface address"),
		eligibleAddresses: Object.freeze(eligibleAddresses)
	});
}
function requireExactProbeDestinations(value) {
	const probes = requireArray(value, "test ICE source selector probes");
	if (probes.length !== TEST_ICE_PROBE_DESTINATIONS.length) contractError("test ICE source selector must use the frozen probe registry");
	probes.forEach((value, index) => {
		const probe = requireRecord(value, `test ICE source selector probe ${index}`);
		requireExactKeys(probe, ["address", "port"], [], `test ICE source selector probe ${index}`);
		const expected = TEST_ICE_PROBE_DESTINATIONS[index];
		if (expected === void 0 || probe.address !== expected.address || probe.port !== expected.port) contractError("test ICE source selector probes must equal the frozen ordered registry");
	});
}
function requireExactVocabulary(value, expected, label) {
	const items = requireArray(value, label);
	if (items.length !== expected.length || items.some((item, index) => item !== expected[index])) contractError(`${label} must equal the frozen ${expected.join(",")} policy`);
}
function requireOperationalIPv4(value, label) {
	const address = requireString(value, label, 15);
	if (!isOperationalIPv4Unicast(address)) contractError(`${label} must be operational non-loopback IPv4 unicast`);
	return address;
}
function isOperationalIPv4Unicast(address) {
	const parts = address.split(".");
	if (parts.length !== 4 || parts.some((part) => !/^(0|[1-9]\d{0,2})$/u.test(part) || Number(part) > 255)) return false;
	const octets = parts.map(Number);
	const first = octets[0];
	const second = octets[1];
	return first !== void 0 && second !== void 0 && first !== 0 && first !== 127 && first < 224 && !(first === 169 && second === 254);
}
function ipv4Number(address) {
	return address.split(".").reduce((result, octet) => result * 256 + Number(octet), 0);
}
async function sha256(value) {
	const bytes = new TextEncoder().encode(value);
	return [...new Uint8Array(await globalThis.crypto.subtle.digest("SHA-256", bytes))].map((item) => item.toString(16).padStart(2, "0")).join("");
}
function validateMainAcceptance(result, topologyLock) {
	const { profile: topology, resolution } = readVerifiedTestIceTopologyLock(topologyLock);
	if (result.resultStatus !== "final-valid" || result.executionOutcome !== "healthy" || result.playwrightOutcome !== "passed" || result.deliveryOutcome !== "succeeded") contractError("main acceptance requires valid, healthy, passed, successful delivery evidence");
	if (result.rtcCapability === "available") {
		if (result.peerAttemptOutcome !== "admitted" || result.routeEvidence?.mode !== "hot-switch" || !hasDirectSelectedPairProof(result.attempts, topology, resolution)) contractError("available RTC acceptance requires admission, direct pair proof, and hot-switch fence proof");
		return;
	}
	if (result.rtcCapability === "unavailable") {
		if (result.peerAttemptOutcome !== "not-started" || result.attempts.length !== 0 || result.routeEvidence?.mode !== "relay-only") contractError("unavailable RTC acceptance requires attempt-free exact relay fallback");
		return;
	}
	contractError("unknown or unusable RTC capability is never an accepted main result");
}
function validatePionAcceptance(result) {
	if (result.resultStatus !== "final-valid" || result.executionOutcome !== "healthy" || result.playwrightOutcome !== "passed") contractError("Pion acceptance requires valid, healthy, passed execution evidence");
	if (result.rtcCapability === "available") {
		if (result.applicability !== "applicable" || result.nativeInteropOutcome !== "succeeded") contractError("available RTC Pion acceptance requires successful applicable native interop");
		return "accepted";
	}
	if (result.rtcCapability === "unavailable") {
		if (result.applicability !== "not-applicable" || result.nativeInteropOutcome !== "not-started") contractError("unavailable RTC Pion evidence must be explicitly not-applicable");
		return "requires-main-relay-fallback";
	}
	contractError("unknown or unusable RTC capability is never accepted by the Pion suite");
}
/** Runtime admission remains an observed fact when pair proof is absent. This
* predicate gives the later verdict one explicit authority for rejecting that
* otherwise admitted sample without rewriting its peer outcome. */
function hasDirectSelectedPairProof(attempts, topology, resolution) {
	const admitted = attempts.filter((attempt) => attempt.outcome === "admitted");
	return admitted.length > 0 && admitted.every((attempt) => {
		const browser = attempt.events.find(({ evidence }) => evidence.side === "browser" && evidence.stage === "admitted")?.evidence;
		const sender = attempt.events.find(({ evidence }) => evidence.side === "sender" && evidence.stage === "admitted")?.evidence;
		if (browser?.side !== "browser" || browser.stage !== "admitted" || sender?.side !== "sender" || sender.stage !== "admitted" || browser.selectedPair === null || sender.selectedPair === null) return false;
		return selectedPairAllowedByTopology(browser.selectedPair, topology, resolution) && selectedPairAllowedByTopology(sender.selectedPair, topology, resolution) && selectedPairsCorrelate(browser.selectedPair, sender.selectedPair);
	});
}
function validateHotSwitchAttemptCorrelation(route, attempts) {
	const admission = route.observations.find((observation) => observation.kind === "peer-admitted");
	if (admission === void 0) contractError("hot-switch route evidence lacks peer admission");
	const matches = attempts.filter((attempt) => attempt.sessionId === admission.sessionId && attempt.peerPathId === admission.peerPathId && attempt.attemptId === admission.attemptId);
	if (matches.length !== 1 || matches[0]?.outcome !== "admitted") contractError("hot-switch route admission does not identify one admitted logical attempt");
	const browserAdmission = matches[0].events.find(({ evidence }) => evidence.side === "browser" && evidence.stage === "admitted")?.evidence;
	if (browserAdmission?.side !== "browser" || browserAdmission.stage !== "admitted" || browserAdmission.lane.laneId !== admission.lane.laneId || browserAdmission.lane.laneEpoch !== admission.lane.laneEpoch) contractError("hot-switch route admission lane differs from attempt admission");
}
function selectedPairsCorrelate(browser, pion) {
	return browserLocalEndpointMatchesPionRemote(browser.local, pion.remote) && browserRemoteEndpointMatchesPionLocal(browser.remote, pion.local);
}
function browserLocalEndpointMatchesPionRemote(browser, pion) {
	if (browser.protocol !== pion.protocol) return false;
	if (browser.port !== void 0 && browser.port !== pion.port) return false;
	if (browser.address === void 0) return browser.candidateType === "host";
	if (isIpLiteral(browser.address)) return browser.address === pion.address;
	return browser.candidateType === "host" && isMdnsHostname(browser.address);
}
function browserRemoteEndpointMatchesPionLocal(browser, pion) {
	return browser.address !== void 0 && isIpLiteral(browser.address) && browser.address === pion.address && browser.port === pion.port && browser.protocol === pion.protocol;
}
function isIpLiteral(address) {
	return address.includes(":") || /^(?:0|[1-9]\d{0,2})(?:\.(?:0|[1-9]\d{0,2})){3}$/u.test(address);
}
function isMdnsHostname(address) {
	return /^(?=.{1,253}\.?$)(?:[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?\.)+local\.?$/u.test(address);
}
var BROWSER_ENGINES = Object.freeze([
	"chromium",
	"firefox",
	"webkit"
]);
var BROWSER_SUITES = Object.freeze(["main", "pion"]);
var RESULT_STATUSES = Object.freeze([
	"provisional",
	"final-valid",
	"final-invalid"
]);
var RTC_CAPABILITIES = Object.freeze([
	"unknown",
	"unavailable",
	"unusable",
	"available"
]);
var PEER_ATTEMPT_OUTCOMES = Object.freeze([
	"not-started",
	"admitted",
	"failed"
]);
var DELIVERY_OUTCOMES = Object.freeze([
	"not-started",
	"succeeded",
	"failed"
]);
var EXECUTION_OUTCOMES = Object.freeze([
	"healthy",
	"crashed",
	"infrastructure-failed",
	"unknown"
]);
var ATTEMPT_SIDES = Object.freeze(["browser", "sender"]);
var BROWSER_ATTEMPT_STAGES = Object.freeze([
	"started",
	"offer-created",
	"offer-sent",
	"answer-received",
	"datachannel-open",
	"lane-granted",
	"lane-attached",
	"admitted",
	"failed"
]);
var SENDER_ATTEMPT_STAGES = Object.freeze([
	"started",
	"offer-received",
	"answer-created",
	"answer-sent",
	"datachannel-open",
	"lane-admission-started",
	"admitted",
	"failed"
]);
var ATTEMPT_TERMINAL_STAGES = Object.freeze(["admitted", "failed"]);
var ATTEMPT_FAILURE_SCOPES = Object.freeze(["attempt", "session"]);
var TYPED_PEER_ERROR_CODES = Object.freeze([
	"peer-negotiation",
	"peer-timeout",
	"peer-candidates",
	"peer-admission",
	"signaling-contract",
	"attempt-cancelled",
	"runtime-stopped",
	"unexpected"
]);
var PEER_OPERATION_CODES = Object.freeze({
	negotiation: 20481,
	timeout: 20482,
	candidates: 20483,
	admission: 20484
});
var PEER_OPERATION_TYPED_ERRORS = Object.freeze({
	[PEER_OPERATION_CODES.negotiation]: "peer-negotiation",
	[PEER_OPERATION_CODES.timeout]: "peer-timeout",
	[PEER_OPERATION_CODES.candidates]: "peer-candidates",
	[PEER_OPERATION_CODES.admission]: "peer-admission"
});
var PEER_OPERATION_ERROR_REGISTRY = Object.freeze([
	Object.freeze({
		code: PEER_OPERATION_CODES.negotiation,
		typedErrorCode: "peer-negotiation"
	}),
	Object.freeze({
		code: PEER_OPERATION_CODES.timeout,
		typedErrorCode: "peer-timeout"
	}),
	Object.freeze({
		code: PEER_OPERATION_CODES.candidates,
		typedErrorCode: "peer-candidates"
	}),
	Object.freeze({
		code: PEER_OPERATION_CODES.admission,
		typedErrorCode: "peer-admission"
	})
]);
var ICE_CANDIDATE_TYPES = Object.freeze([
	"host",
	"prflx",
	"srflx",
	"relay"
]);
var ICE_PROTOCOLS = Object.freeze(["udp", "tcp"]);
var IP_ADDRESS_FAMILIES = Object.freeze(["ipv4", "ipv6"]);
Object.freeze({
	schemaVersion: 1,
	browserEngines: BROWSER_ENGINES,
	suites: BROWSER_SUITES,
	resultStatuses: RESULT_STATUSES,
	rtcCapabilities: RTC_CAPABILITIES,
	peerAttemptOutcomes: PEER_ATTEMPT_OUTCOMES,
	deliveryOutcomes: DELIVERY_OUTCOMES,
	executionOutcomes: EXECUTION_OUTCOMES,
	attemptSides: ATTEMPT_SIDES,
	browserStages: BROWSER_ATTEMPT_STAGES,
	senderStages: SENDER_ATTEMPT_STAGES,
	terminalStages: ATTEMPT_TERMINAL_STAGES,
	failureScopes: ATTEMPT_FAILURE_SCOPES,
	typedPeerErrorCodes: TYPED_PEER_ERROR_CODES,
	peerOperationCodeMapping: PEER_OPERATION_ERROR_REGISTRY,
	iceCandidateTypes: ICE_CANDIDATE_TYPES,
	iceProtocols: ICE_PROTOCOLS,
	ipAddressFamilies: IP_ADDRESS_FAMILIES
});
function typedErrorForPeerOperationCode(code) {
	if (!Object.hasOwn(PEER_OPERATION_TYPED_ERRORS, code)) return void 0;
	return PEER_OPERATION_TYPED_ERRORS[code];
}
var COMMON_FIELDS = Object.freeze([
	"schemaVersion",
	"sessionId",
	"peerPathId",
	"attemptId",
	"side",
	"sideSequence",
	"attemptElapsedMs",
	"stage"
]);
var MAXIMUM_COUNTER = 4294967295;
var MAXIMUM_DIAGNOSTIC_TEXT_BYTES = 512;
function parseAttemptEvidence(value) {
	const record = requireRecord(value, "attempt evidence");
	const side = requireEnum(record.side, ATTEMPT_SIDES, "attempt side");
	const stage = parseStage(record.stage, side);
	requireAttemptKeys(record, side, stage);
	const envelope = {
		schemaVersion: requireLiteral(record.schemaVersion, 1, "attempt evidence schema version"),
		sessionId: requireCanonicalIdentity(record.sessionId, "protocol session ID"),
		peerPathId: requireCanonicalIdentity(record.peerPathId, "peer path ID"),
		attemptId: requireCanonicalIdentity(record.attemptId, "peer attempt ID"),
		side,
		sideSequence: requireSafeInteger(record.sideSequence, 1, Number.MAX_SAFE_INTEGER, "attempt side sequence"),
		attemptElapsedMs: requireSafeInteger(record.attemptElapsedMs, 0, Number.MAX_SAFE_INTEGER, "attempt elapsed milliseconds"),
		stage,
		...side === "sender" && optionalField(record, "localGeneration") !== void 0 ? { localGeneration: requireDecimalUint64(record.localGeneration, "sender local generation") } : {}
	};
	const payload = parseStagePayload(record, side, stage);
	return freezeRecord({
		...envelope,
		...payload
	});
}
function parseBrowserSelectedPair(value) {
	const pair = requireRecord(value, "browser selected pair");
	requireExactKeys(pair, [
		"candidatePairId",
		"local",
		"remote"
	], [], "browser selected pair");
	return freezeRecord({
		candidatePairId: requireString(pair.candidatePairId, "browser candidate pair ID", 256),
		local: parseBrowserCandidate(pair.local, "browser local selected candidate"),
		remote: parseBrowserCandidate(pair.remote, "browser remote selected candidate")
	});
}
function parsePionSelectedPair(value) {
	const pair = requireRecord(value, "Pion selected pair");
	requireExactKeys(pair, ["local", "remote"], ["candidatePairId"], "Pion selected pair");
	const pairId = optionalField(pair, "candidatePairId");
	const local = parsePionCandidate(pair.local, "Pion local selected candidate");
	const remote = parsePionCandidate(pair.remote, "Pion remote selected candidate");
	if (local.addressFamily === remote.addressFamily && local.address === remote.address && local.port === remote.port && local.protocol === remote.protocol) contractError("Pion selected pair must identify distinct local and remote transport endpoints");
	return freezeRecord({
		...pairId === void 0 ? {} : { candidatePairId: requireString(pairId, "Pion candidate pair ID", 256) },
		local,
		remote
	});
}
function parseStage(value, side) {
	return side === "browser" ? requireEnum(value, BROWSER_ATTEMPT_STAGES, "browser attempt stage") : requireEnum(value, SENDER_ATTEMPT_STAGES, "sender attempt stage");
}
function requireAttemptKeys(record, side, stage) {
	const required = [...COMMON_FIELDS];
	const optional = side === "sender" ? ["localGeneration"] : [];
	if (stage === "failed") {
		required.push("failedAtStage", "failureScope", "typedErrorCode", "failureMessage");
		optional.push("candidateCounts", "lane", "selectedPair", "authenticatedSenderOperationFailure");
	} else if (stage === "admitted") required.push("candidateCounts", "lane", "selectedPair");
	else if (candidateCountsRequired(side, stage)) {
		required.push("candidateCounts");
		if (laneRequired(side, stage)) required.push("lane");
	}
	requireExactKeys(record, required, optional, `${side} ${stage} attempt evidence`);
}
function candidateCountsRequired(side, stage) {
	if (side === "browser") return stage !== "started";
	return stage !== "started" && stage !== "offer-received";
}
function laneRequired(side, stage) {
	return side === "browser" ? stage === "lane-granted" || stage === "lane-attached" : stage === "lane-admission-started";
}
function parseStagePayload(record, side, stage) {
	if (stage === "started" || side === "sender" && stage === "offer-received") return {};
	if (stage === "failed") return parseFailurePayload(record, side);
	const candidateCounts = parseCandidateCounts(record.candidateCounts);
	if (stage === "admitted") return {
		candidateCounts,
		lane: parseLaneIdentity(record.lane),
		selectedPair: parseNullableSelectedPair(record.selectedPair, side)
	};
	if (laneRequired(side, stage)) return {
		candidateCounts,
		lane: parseLaneIdentity(record.lane)
	};
	return { candidateCounts };
}
function parseFailurePayload(record, side) {
	const failedAtStage = parseFailureStage(record.failedAtStage, side);
	const typedErrorCode = requireEnum(record.typedErrorCode, TYPED_PEER_ERROR_CODES, "typed peer error code");
	const candidateCountsValue = optionalField(record, "candidateCounts");
	const laneValue = optionalField(record, "lane");
	const selectedPairValue = optionalField(record, "selectedPair");
	const authenticatedOperationValue = optionalField(record, "authenticatedSenderOperationFailure");
	const authenticatedSenderOperationFailure = authenticatedOperationValue === void 0 ? void 0 : parseAuthenticatedSenderOperationFailure(authenticatedOperationValue, typedErrorCode);
	const failureScope = requireEnum(record.failureScope, ATTEMPT_FAILURE_SCOPES, "attempt failure scope");
	const failureMessage = requireString(record.failureMessage, "attempt failure message", MAXIMUM_DIAGNOSTIC_TEXT_BYTES);
	validateFailureFieldCausality({
		side,
		failedAtStage,
		failureScope,
		typedErrorCode,
		failureMessage,
		candidateCountsValue,
		laneValue,
		selectedPairValue,
		authenticatedSenderOperationFailure
	});
	return buildFailurePayload({
		side,
		failedAtStage,
		failureScope,
		typedErrorCode,
		failureMessage,
		candidateCountsValue,
		laneValue,
		selectedPairValue,
		authenticatedSenderOperationFailure
	});
}
function validateFailureFieldCausality(parts) {
	const { side, failedAtStage, failureScope, failureMessage, candidateCountsValue, laneValue, selectedPairValue, authenticatedSenderOperationFailure } = parts;
	if (candidateCountsValue !== void 0 && !failureCanCarryCandidateCounts(side, failedAtStage)) contractError(`${side} failure cannot carry candidate counts before their first completed milestone`);
	if (laneValue !== void 0 && !failureCanCarryKnownLane(side, failedAtStage)) contractError(`${side} failure cannot carry a lane before the lane milestone is known`);
	if (selectedPairValue !== void 0 && failedAtStage !== "admitted") contractError(`${side} failure can carry selected-pair evidence only while admission fails`);
	if (authenticatedSenderOperationFailure !== void 0) {
		if (side !== "browser" || failedAtStage === "offer-created" || failedAtStage === "offer-sent") contractError("authenticated sender operation failure requires a browser stream after offer dispatch");
		if (failureScope !== "attempt") contractError("authenticated sender peer operation failure must remain attempt-scoped");
		if (failureMessage !== authenticatedSenderOperationFailure.message) contractError("authenticated sender operation message must be preserved losslessly");
	}
}
function buildFailurePayload(parts) {
	const result = {
		failedAtStage: parts.failedAtStage,
		failureScope: parts.failureScope,
		typedErrorCode: parts.typedErrorCode,
		failureMessage: parts.failureMessage
	};
	if (parts.candidateCountsValue !== void 0) result.candidateCounts = parseCandidateCounts(parts.candidateCountsValue);
	if (parts.laneValue !== void 0) result.lane = parseLaneIdentity(parts.laneValue);
	if (parts.selectedPairValue !== void 0) result.selectedPair = parseNullableSelectedPair(parts.selectedPairValue, parts.side);
	if (parts.authenticatedSenderOperationFailure !== void 0) result.authenticatedSenderOperationFailure = parts.authenticatedSenderOperationFailure;
	return result;
}
function parseFailureStage(value, side) {
	const stage = requireEnum(value, side === "browser" ? BROWSER_ATTEMPT_STAGES : SENDER_ATTEMPT_STAGES, `${side} failed-at stage`);
	if (stage === "started" || stage === "failed") contractError(`${side} failed-at stage must name the milestone that could not complete`);
	return stage;
}
function parseCandidateCounts(value) {
	const counts = requireRecord(value, "candidate counts");
	requireExactKeys(counts, ["localEmitted", "remoteAccepted"], [], "candidate counts");
	return freezeRecord({
		localEmitted: requireSafeInteger(counts.localEmitted, 0, MAXIMUM_COUNTER, "local emitted candidate count"),
		remoteAccepted: requireSafeInteger(counts.remoteAccepted, 0, MAXIMUM_COUNTER, "remote accepted candidate count")
	});
}
function parseLaneIdentity(value) {
	const lane = requireRecord(value, "lane identity");
	requireExactKeys(lane, ["laneId", "laneEpoch"], [], "lane identity");
	return freezeRecord({
		laneId: requireSafeInteger(lane.laneId, 1, MAXIMUM_COUNTER, "lane ID"),
		laneEpoch: requireSafeInteger(lane.laneEpoch, 1, MAXIMUM_COUNTER, "lane epoch")
	});
}
function parseNullableSelectedPair(value, side) {
	if (value === null) return null;
	return side === "browser" ? parseBrowserSelectedPair(value) : parsePionSelectedPair(value);
}
function parseBrowserCandidate(value, label) {
	const candidate = requireRecord(value, label);
	requireExactKeys(candidate, [
		"candidateId",
		"candidateType",
		"protocol"
	], ["address", "port"], label);
	const address = optionalField(candidate, "address");
	const port = optionalField(candidate, "port");
	return freezeRecord({
		candidateId: requireString(candidate.candidateId, `${label} ID`, 256),
		candidateType: requireEnum(candidate.candidateType, ICE_CANDIDATE_TYPES, `${label} type`),
		protocol: requireEnum(candidate.protocol, ICE_PROTOCOLS, `${label} protocol`),
		...address === void 0 ? {} : { address: requireString(address, `${label} address`, 255) },
		...port === void 0 ? {} : { port: requireSafeInteger(port, 1, 65535, `${label} port`) }
	});
}
function parsePionCandidate(value, label) {
	const candidate = requireRecord(value, label);
	requireExactKeys(candidate, [
		"candidateType",
		"protocol",
		"address",
		"port",
		"addressFamily"
	], ["candidateId"], label);
	const addressFamily = requireEnum(candidate.addressFamily, IP_ADDRESS_FAMILIES, `${label} address family`);
	const address = requireString(candidate.address, `${label} address`, 255);
	if (!isOperationalUnicastAddress(address, addressFamily)) contractError(`${label} address must be operational non-loopback ${addressFamily} unicast`);
	const candidateId = optionalField(candidate, "candidateId");
	return freezeRecord({
		...candidateId === void 0 ? {} : { candidateId: requireString(candidateId, `${label} ID`, 256) },
		candidateType: requireEnum(candidate.candidateType, ICE_CANDIDATE_TYPES, `${label} type`),
		protocol: requireEnum(candidate.protocol, ICE_PROTOCOLS, `${label} protocol`),
		address,
		port: requireSafeInteger(candidate.port, 1, 65535, `${label} port`),
		addressFamily
	});
}
function parseAuthenticatedSenderOperationFailure(value, typedErrorCode) {
	const failure = requireRecord(value, "authenticated sender operation failure");
	requireExactKeys(failure, [
		"scope",
		"code",
		"message"
	], [], "authenticated sender operation failure");
	const code = requireSafeInteger(failure.code, 0, 65535, "authenticated peer operation code");
	const mapped = typedErrorForPeerOperationCode(code);
	if (mapped === void 0 || mapped !== typedErrorCode) contractError("authenticated peer operation code does not match the typed peer error code");
	return freezeRecord({
		scope: requireLiteral(failure.scope, "peer", "authenticated operation scope"),
		code,
		message: requireString(failure.message, "authenticated peer operation message", MAXIMUM_DIAGNOSTIC_TEXT_BYTES)
	});
}
function failureCanCarryCandidateCounts(side, failedAtStage) {
	if (side === "browser") return failedAtStage !== "offer-created";
	return failedAtStage !== "offer-received" && failedAtStage !== "answer-created";
}
function failureCanCarryKnownLane(side, failedAtStage) {
	return side === "browser" ? failedAtStage === "lane-attached" || failedAtStage === "admitted" : failedAtStage === "admitted";
}
function isOperationalUnicastAddress(address, family) {
	if (family === "ipv4") {
		const parts = address.split(".");
		if (parts.length !== 4 || !parts.every((part) => {
			if (!/^(0|[1-9]\d{0,2})$/u.test(part)) return false;
			return Number(part) <= 255;
		})) return false;
		const octets = parts.map(Number);
		const first = octets[0];
		const second = octets[1];
		return first !== void 0 && second !== void 0 && first !== 0 && first !== 127 && first < 224 && !(first === 169 && second === 254);
	}
	if (!address.includes(":") || !/^[0-9a-fA-F:.]+$/u.test(address)) return false;
	const normalized = address.toLowerCase();
	const firstGroup = normalized.split(":")[0];
	if (normalized === "::" || normalized === "::1" || normalized.startsWith("ff") || firstGroup !== void 0 && /^fe[89ab]/u.test(firstGroup)) return false;
	try {
		return new URL(`http://[${address}]/`).hostname.length > 2;
	} catch {
		return false;
	}
}
var BROWSER_SUCCESS_CHAIN = BROWSER_ATTEMPT_STAGES.filter((stage) => stage !== "failed");
var SENDER_SUCCESS_CHAIN = SENDER_ATTEMPT_STAGES.filter((stage) => stage !== "failed");
/**
* Validation deliberately happens before reduction. A missing terminal is lost
* evidence, not a peer failure, and coercing it into a fixed outcome would make
* the browser gate claim a runtime fact it never observed.
*/
var AttemptCollector = class {
	#attempts = /* @__PURE__ */ new Map();
	#laneAuthorityBySession = /* @__PURE__ */ new Map();
	#receiveSequence = 0;
	#finalized = false;
	#rejectedEvidence = false;
	ingest(value) {
		if (this.#finalized) contractError("attempt collector is already finalized");
		if (this.#rejectedEvidence) contractError("attempt collector previously rejected evidence");
		try {
			const evidence = parseAttemptEvidence(value);
			const key = attemptKey(evidence);
			const attempt = cloneAttemptState(this.#attempts.get(key), evidence);
			const laneAuthority = cloneSessionLaneAuthority(this.#laneAuthorityBySession.get(evidence.sessionId));
			validateStreamEvent(ensureSideStream(attempt, evidence.side), attempt, evidence);
			reserveObservedLane(laneAuthority, attempt);
			const receiveSequence = this.#receiveSequence + 1;
			const received = freezeRecord({
				receiveSequence,
				evidence
			});
			attempt.events.push(received);
			this.#attempts.set(key, attempt);
			this.#laneAuthorityBySession.set(evidence.sessionId, laneAuthority);
			this.#receiveSequence = receiveSequence;
			return received;
		} catch (cause) {
			this.#rejectedEvidence = true;
			throw cause;
		}
	}
	finalize() {
		if (this.#finalized) contractError("attempt collector can only be finalized once");
		this.#finalized = true;
		if (this.#rejectedEvidence) contractError("attempt collector cannot finalize after rejected evidence");
		const attempts = [...this.#attempts.values()].sort((left, right) => compareAttemptKeys(attemptKey(left), attemptKey(right))).map((attempt) => finalizeAttempt(attempt));
		return Object.freeze(attempts);
	}
	finalizePreservingCompleted() {
		if (this.#finalized) contractError("attempt collector can only be finalized once");
		this.#finalized = true;
		const attempts = [];
		const violations = [];
		for (const attempt of [...this.#attempts.values()].sort((left, right) => compareAttemptKeys(attemptKey(left), attemptKey(right)))) try {
			attempts.push(finalizeAttempt(attempt));
		} catch (cause) {
			violations.push(errorMessage(cause));
		}
		if (this.#rejectedEvidence && violations.length === 0) violations.push("attempt collector rejected evidence after its last valid state");
		return freezeRecord({
			attempts: Object.freeze(attempts),
			integrityViolations: Object.freeze([...new Set(violations)].sort(compareAttemptKeys))
		});
	}
};
function cloneAttemptState(current, evidence) {
	if (current === void 0) return {
		sessionId: evidence.sessionId,
		peerPathId: evidence.peerPathId,
		attemptId: evidence.attemptId,
		streams: /* @__PURE__ */ new Map(),
		events: [],
		lane: void 0
	};
	return {
		sessionId: current.sessionId,
		peerPathId: current.peerPathId,
		attemptId: current.attemptId,
		streams: new Map([...current.streams].map(([side, stream]) => [side, { ...stream }])),
		events: [...current.events],
		lane: current.lane
	};
}
function cloneSessionLaneAuthority(current) {
	return {
		laneIdOwners: new Map(current?.laneIdOwners),
		laneEpochOwners: new Map(current?.laneEpochOwners)
	};
}
function reserveObservedLane(authority, attempt) {
	const lane = attempt.lane;
	if (lane === void 0) return;
	const owner = attemptKey(attempt);
	const laneIdOwner = authority.laneIdOwners.get(lane.laneId);
	const laneEpochOwner = authority.laneEpochOwners.get(lane.laneEpoch);
	if (laneIdOwner !== void 0 && laneIdOwner !== owner) contractError(`lane ID ${lane.laneId} is reused within ProtocolSession ${attempt.sessionId}`);
	if (laneEpochOwner !== void 0 && laneEpochOwner !== owner) contractError(`lane epoch ${lane.laneEpoch} is reused within ProtocolSession ${attempt.sessionId}`);
	authority.laneIdOwners.set(lane.laneId, owner);
	authority.laneEpochOwners.set(lane.laneEpoch, owner);
}
function ensureSideStream(attempt, side) {
	let stream = attempt.streams.get(side);
	if (stream === void 0) {
		stream = {
			side,
			nextSuccessIndex: 0,
			lastSequence: 0,
			lastElapsedMs: 0,
			terminal: void 0,
			candidateCounts: void 0,
			lane: void 0,
			localGeneration: void 0
		};
		attempt.streams.set(side, stream);
	}
	return stream;
}
function reducePeerAttemptOutcome(attempts) {
	if (attempts.length === 0) return "not-started";
	const identities = /* @__PURE__ */ new Set();
	let admitted = false;
	let failed = false;
	for (const attempt of attempts) {
		const key = attemptKey(attempt);
		if (identities.has(key)) contractError(`logical attempt ${key} appears more than once`);
		identities.add(key);
		if (attempt.outcome === "failed") failed = true;
		else admitted = true;
	}
	if (failed) return "failed";
	if (!admitted) contractError("non-empty logical attempt set has no terminal outcome");
	return "admitted";
}
function parseLogicalAttempts(value, allowReceiveSequenceGaps = false) {
	const normalized = requireArray(value, "logical attempts").map((item, index) => parseLogicalAttemptRecord(item, index));
	const events = normalized.flatMap((attempt) => attempt.events).sort((left, right) => left.receiveSequence - right.receiveSequence);
	const collector = new AttemptCollector();
	let previousReceiveSequence = 0;
	for (let index = 0; index < events.length; index += 1) {
		const received = events[index];
		if (received === void 0 || (allowReceiveSequenceGaps ? received.receiveSequence <= previousReceiveSequence : received.receiveSequence !== index + 1)) contractError("collector receive sequence must be contiguous from one");
		previousReceiveSequence = received.receiveSequence;
		const replayed = collector.ingest(received.evidence);
		if (!allowReceiveSequenceGaps && replayed.receiveSequence !== received.receiveSequence) contractError("collector receive sequence does not reproduce");
	}
	const replayed = collector.finalize();
	const rankByReceiveSequence = new Map(events.map((event, index) => [event.receiveSequence, index + 1]));
	const normalizedForReplay = normalized.map((attempt) => freezeRecord({
		...attempt,
		events: Object.freeze(attempt.events.map((event) => freezeRecord({
			...event,
			receiveSequence: rankByReceiveSequence.get(event.receiveSequence)
		})))
	}));
	if (JSON.stringify(replayed) !== JSON.stringify(normalizedForReplay)) contractError("serialized logical attempts do not match their producer evidence");
	return allowReceiveSequenceGaps ? Object.freeze(normalized) : replayed;
}
function validateStreamEvent(stream, attempt, evidence) {
	if (stream.terminal !== void 0) contractError(`${evidence.stage === "admitted" || evidence.stage === "failed" ? "duplicate terminal" : "post-terminal event"} for ${attemptKey(attempt)}/${stream.side}`);
	if (evidence.sideSequence !== stream.lastSequence + 1) contractError(`side sequence is not contiguous for ${attemptKey(attempt)}/${stream.side}`);
	if (stream.lastSequence === 0 && evidence.stage !== "started") contractError(`side stream does not begin with started for ${attemptKey(attempt)}/${stream.side}`);
	if (evidence.attemptElapsedMs < stream.lastElapsedMs) contractError(`attempt elapsed time regressed for ${attemptKey(attempt)}/${stream.side}`);
	const expectedStage = successChain(stream.side)[stream.nextSuccessIndex];
	if (evidence.stage === "failed") {
		if (evidence.failedAtStage !== expectedStage) contractError(`failed-at stage does not name the next milestone for ${attemptKey(attempt)}/${stream.side}`);
		stream.terminal = "failed";
	} else {
		if (evidence.stage !== expectedStage) contractError(`attempt stage is out of order for ${attemptKey(attempt)}/${stream.side}`);
		stream.nextSuccessIndex += 1;
		if (evidence.stage === "admitted") stream.terminal = "admitted";
	}
	validateCandidateCounts(stream, evidence, attempt);
	validateSelectedPair(stream, evidence, attempt);
	validateLane(stream, attempt, evidence);
	validateLocalGeneration(stream, evidence, attempt);
	stream.lastSequence = evidence.sideSequence;
	stream.lastElapsedMs = evidence.attemptElapsedMs;
}
function validateCandidateCounts(stream, evidence, attempt) {
	const counts = Object.hasOwn(evidence, "candidateCounts") ? evidence.candidateCounts : void 0;
	if (stream.candidateCounts !== void 0 && counts === void 0) contractError(`candidate counts disappeared for ${attemptKey(attempt)}/${stream.side}`);
	if (counts !== void 0 && stream.candidateCounts !== void 0 && (counts.localEmitted < stream.candidateCounts.localEmitted || counts.remoteAccepted < stream.candidateCounts.remoteAccepted)) contractError(`cumulative candidate counts regressed for ${attemptKey(attempt)}/${stream.side}`);
	if (counts !== void 0) stream.candidateCounts = counts;
}
function validateLane(stream, attempt, evidence) {
	const lane = Object.hasOwn(evidence, "lane") ? evidence.lane : void 0;
	if (stream.lane !== void 0 && lane === void 0) contractError(`known lane identity disappeared for ${attemptKey(attempt)}/${stream.side}`);
	if (lane === void 0) return;
	if (evidence.stage === "failed" && stream.lane === void 0) contractError(`failed evidence invents a lane before its milestone for ${attemptKey(attempt)}/${stream.side}`);
	if (stream.lane !== void 0 && !sameLane$1(stream.lane, lane)) contractError(`lane identity changed within ${attemptKey(attempt)}/${stream.side}`);
	if (attempt.lane !== void 0 && !sameLane$1(attempt.lane, lane)) contractError(`browser and sender lane identities differ for ${attemptKey(attempt)}`);
	stream.lane = lane;
	attempt.lane = lane;
}
function validateSelectedPair(stream, evidence, attempt) {
	if (!Object.hasOwn(evidence, "selectedPair")) return;
	if (evidence.stage === "failed" && stream.lane === void 0) contractError(`failed evidence invents selected-pair proof before lane admission for ${attemptKey(attempt)}/${stream.side}`);
}
function validateLocalGeneration(stream, evidence, attempt) {
	if (evidence.side !== "sender") return;
	const generation = evidence.localGeneration;
	if (stream.localGeneration !== void 0 && generation === void 0) contractError(`known local generation disappeared for ${attemptKey(attempt)}/sender`);
	if (generation !== void 0 && stream.localGeneration !== void 0 && generation !== stream.localGeneration) contractError(`local generation changed for ${attemptKey(attempt)}/sender`);
	if (generation !== void 0) stream.localGeneration = generation;
}
function finalizeAttempt(attempt) {
	const browser = attempt.streams.get("browser");
	if (browser === void 0) contractError(`logical attempt ${attemptKey(attempt)} has no browser authority stream`);
	for (const stream of attempt.streams.values()) if (stream.terminal === void 0) contractError(`side stream ${attemptKey(attempt)}/${stream.side} has no terminal`);
	const failed = [...attempt.streams.values()].some((stream) => stream.terminal === "failed");
	const answerReceivedIndex = BROWSER_SUCCESS_CHAIN.indexOf("answer-received");
	const authenticatedBrowserFailure = attempt.events.find(({ evidence }) => evidence.side === "browser" && evidence.stage === "failed" && Object.hasOwn(evidence, "authenticatedSenderOperationFailure"))?.evidence;
	const authenticatedSenderFailure = authenticatedBrowserFailure !== void 0;
	const sender = attempt.streams.get("sender");
	if (failed && (browser.nextSuccessIndex > answerReceivedIndex || authenticatedSenderFailure) && sender === void 0) contractError(`failed attempt ${attemptKey(attempt)} observed sender participation but has no sender stream`);
	if (authenticatedSenderFailure && (sender === void 0 || !stageCompleted(sender, "offer-received"))) contractError(`authenticated sender failure ${attemptKey(attempt)} lacks sender offer reception`);
	validateAuthenticatedSenderFailure(attempt, sender, authenticatedBrowserFailure);
	validateCrossSideReachability(attempt, browser);
	if (!failed) {
		if (browser.terminal !== "admitted" || sender?.terminal !== "admitted") contractError(`admitted attempt ${attemptKey(attempt)} lacks both admitted side streams`);
		if (attempt.lane === void 0) contractError(`admitted attempt ${attemptKey(attempt)} has no authoritative lane identity`);
	}
	return freezeRecord({
		sessionId: attempt.sessionId,
		peerPathId: attempt.peerPathId,
		attemptId: attempt.attemptId,
		outcome: failed ? "failed" : "admitted",
		events: Object.freeze([...attempt.events])
	});
}
function validateAuthenticatedSenderFailure(attempt, sender, browserEvidence) {
	if (browserEvidence === void 0) return;
	if (browserEvidence.side !== "browser" || browserEvidence.stage !== "failed") contractError(`authenticated sender failure ${attemptKey(attempt)} has invalid browser authority`);
	const operation = browserEvidence.authenticatedSenderOperationFailure;
	const senderEvidence = attempt.events.find(({ evidence }) => evidence.side === "sender" && evidence.stage === "failed")?.evidence;
	if (operation === void 0 || sender?.terminal !== "failed" || senderEvidence?.side !== "sender" || senderEvidence.stage !== "failed") contractError(`authenticated sender failure ${attemptKey(attempt)} requires a failed sender terminal`);
	if (senderEvidence.typedErrorCode !== browserEvidence.typedErrorCode || senderEvidence.failureScope !== browserEvidence.failureScope || senderEvidence.failureMessage !== operation.message) contractError(`authenticated sender failure ${attemptKey(attempt)} differs across producer streams`);
}
function validateCrossSideReachability(attempt, browser) {
	const sender = attempt.streams.get("sender");
	if (sender === void 0) return;
	if (!stageCompleted(browser, "offer-sent")) contractError(`sender stream ${attemptKey(attempt)} exists before browser offer dispatch`);
	for (const [browserStage, senderStage] of [
		["answer-received", "answer-sent"],
		["datachannel-open", "datachannel-open"],
		["lane-granted", "lane-admission-started"]
	]) if (stageCompleted(browser, browserStage) && !stageCompleted(sender, senderStage)) contractError(`browser ${browserStage} in ${attemptKey(attempt)} lacks sender ${senderStage}`);
	if (stageCompleted(browser, "lane-attached") && sender.terminal !== "admitted") contractError(`browser lane attachment in ${attemptKey(attempt)} lacks sender admission`);
	if (stageCompleted(sender, "datachannel-open") && !stageCompleted(browser, "answer-received")) contractError(`sender datachannel in ${attemptKey(attempt)} precedes browser answer receipt`);
	if ((stageCompleted(sender, "lane-admission-started") || sender.terminal === "admitted") && !stageCompleted(browser, "datachannel-open")) contractError(`sender lane admission in ${attemptKey(attempt)} lacks browser datachannel`);
}
function stageCompleted(stream, stage) {
	const stageIndex = successChain(stream.side).indexOf(stage);
	return stageIndex >= 0 && stream.nextSuccessIndex > stageIndex;
}
function parseLogicalAttemptRecord(value, index) {
	const record = requireRecord(value, `logical attempt ${index}`);
	requireExactKeys(record, [
		"sessionId",
		"peerPathId",
		"attemptId",
		"outcome",
		"events"
	], [], `logical attempt ${index}`);
	const sessionId = requireCanonicalIdentity(record.sessionId, `logical attempt ${index} session ID`);
	const peerPathId = requireCanonicalIdentity(record.peerPathId, `logical attempt ${index} path ID`);
	const attemptId = requireCanonicalIdentity(record.attemptId, `logical attempt ${index} attempt ID`);
	const events = requireArray(record.events, `logical attempt ${index} events`).map((event, eventIndex) => {
		const wrapper = requireRecord(event, `logical attempt ${index} event ${eventIndex}`);
		requireExactKeys(wrapper, ["receiveSequence", "evidence"], [], `logical attempt ${index} event ${eventIndex}`);
		const evidence = parseAttemptEvidence(wrapper.evidence);
		if (evidence.sessionId !== sessionId || evidence.peerPathId !== peerPathId || evidence.attemptId !== attemptId) contractError(`logical attempt ${index} contains evidence for another identity`);
		return freezeRecord({
			receiveSequence: requireSafeInteger(wrapper.receiveSequence, 1, Number.MAX_SAFE_INTEGER, `logical attempt ${index} receive sequence`),
			evidence
		});
	});
	return freezeRecord({
		sessionId,
		peerPathId,
		attemptId,
		outcome: requireEnum(record.outcome, PEER_ATTEMPT_OUTCOMES.filter((outcome) => outcome !== "not-started"), `logical attempt ${index} outcome`),
		events: Object.freeze(events)
	});
}
function successChain(side) {
	return side === "browser" ? BROWSER_SUCCESS_CHAIN : SENDER_SUCCESS_CHAIN;
}
function attemptKey(identity) {
	return `${identity.sessionId}/${identity.peerPathId}/${identity.attemptId}`;
}
function sameLane$1(left, right) {
	return left.laneId === right.laneId && left.laneEpoch === right.laneEpoch;
}
function compareAttemptKeys(left, right) {
	if (left === right) return 0;
	return left < right ? -1 : 1;
}
function errorMessage(cause) {
	return cause instanceof Error ? cause.message : String(cause);
}
var WINDOWS_DEVICE_SEGMENT = /^(?:aux|clock\$|com(?:[1-9¹²³])|con|conin\$|conout\$|lpt(?:[1-9¹²³])|nul|prn)(?:\..*)?$/iu;
var WINDOWS_FORBIDDEN_CHARACTER = /[<>"|?*]/u;
var PORTABLE_PATH_MAXIMUM_BYTES = 4096;
function requirePortableRelativePath(value, label, maximumBytes = PORTABLE_PATH_MAXIMUM_BYTES) {
	if (typeof value !== "string" || value.length === 0 || !hasOnlyUnicodeScalars(value)) throw new Error(`${label} must be non-empty Unicode scalar text`);
	if (value !== value.normalize("NFC")) throw new Error(`${label} must use canonical Unicode NFC`);
	const segments = value.split("/");
	if (Buffer.byteLength(value, "utf8") > maximumBytes || segments.length > 64 || value.includes("\\") || value.includes(":") || value.startsWith("/") || containsControlCharacter(value) || WINDOWS_FORBIDDEN_CHARACTER.test(value) || segments.some((segment) => segment === "" || segment === "." || segment === ".." || Buffer.byteLength(segment, "utf8") > 255 || segment.endsWith(".") || segment.endsWith(" ") || WINDOWS_DEVICE_SEGMENT.test(segment) || containsNonAsciiCasedScalar(segment))) throw new Error(`${label} must be a portable normalized relative POSIX path`);
	return value;
}
function portablePathCollisionKey(path) {
	return path.replace(/[A-Z]/gu, (character) => character.toLowerCase());
}
function comparePortablePaths(left, right) {
	return Buffer.compare(Buffer.from(left, "utf8"), Buffer.from(right, "utf8"));
}
function containsControlCharacter(value) {
	return [...value].some((scalar) => {
		const codePoint = scalar.codePointAt(0) ?? 0;
		return codePoint <= 31 || codePoint === 127;
	});
}
function containsNonAsciiCasedScalar(value) {
	return [...value].some((scalar) => (scalar.codePointAt(0) ?? 0) > 127 && scalar.toUpperCase() !== scalar.toLowerCase());
}
function hasOnlyUnicodeScalars(value) {
	for (let index = 0; index < value.length; index += 1) {
		const current = value.charCodeAt(index);
		if (current >= 55296 && current <= 56319) {
			const following = value.charCodeAt(index + 1);
			if (following < 56320 || following > 57343) return false;
			index += 1;
		} else if (current >= 56320 && current <= 57343) return false;
	}
	return true;
}
var ARTIFACT_MANIFEST_ID_SCHEMA_VERSION = 1;
var ARTIFACT_MANIFEST_SET_SCHEMA_VERSION = 1;
/**
* Guard authorization must follow exact bytes and metadata across process
* boundaries. Content-addressing the full manifest prevents a path-stable file
* replacement from borrowing another sample's guard result.
*/
function artifactIdForManifest(artifact) {
	const encoded = JSON.stringify({
		schemaVersion: ARTIFACT_MANIFEST_ID_SCHEMA_VERSION,
		kind: artifact.kind,
		relativePath: artifact.relativePath,
		mediaType: artifact.mediaType,
		byteLength: artifact.byteLength,
		sha256: artifact.sha256
	});
	return `artifact-${createHash("sha256").update(encoded, "utf8").digest("hex")}`;
}
function artifactManifestSha256(artifacts) {
	const canonical = [...artifacts].map((artifact) => ({
		artifactId: artifact.artifactId,
		kind: artifact.kind,
		relativePath: artifact.relativePath,
		mediaType: artifact.mediaType,
		byteLength: artifact.byteLength,
		sha256: artifact.sha256
	})).sort((left, right) => comparePortablePaths(left.relativePath, right.relativePath) || compareStrings$2(left.artifactId, right.artifactId));
	return sha256Bytes(Buffer.from(JSON.stringify({
		schemaVersion: ARTIFACT_MANIFEST_SET_SCHEMA_VERSION,
		artifacts: canonical
	}), "utf8"));
}
function sha256Bytes(value) {
	return createHash("sha256").update(value).digest("hex");
}
function compareStrings$2(left, right) {
	if (left === right) return 0;
	return left < right ? -1 : 1;
}
var CAPABILITY_PROBE_DEADLINE_MS = 5e3;
var RTC_API_PRESENCE = Object.freeze([
	"unknown",
	"absent",
	"present"
]);
var CAPABILITY_PROBE_OUTCOMES = Object.freeze([
	"not-started",
	"succeeded",
	"failed"
]);
var CAPABILITY_PROBE_FAILURE_CODES = Object.freeze([
	"peer-construction",
	"datachannel-construction",
	"offer-creation",
	"local-description",
	"probe-deadline",
	"unexpected"
]);
/**
* The probe proves only that a native PeerConnection can retain a non-empty
* local offer after creating the WindShare-shaped DataChannel. Waiting for ICE
* or a remote peer here would collapse runtime capability into topology health.
*/
function classifyRtcCapability(evidence) {
	validateCapabilityCombination(evidence);
	if (evidence.apiPresence === "unknown") return "unknown";
	if (evidence.apiPresence === "absent") return "unavailable";
	if (evidence.probeOutcome === "not-started") return "unknown";
	return evidence.probeOutcome === "succeeded" ? "available" : "unusable";
}
function parseCapabilityEvidence(value) {
	const record = requireRecord(value, "capability evidence");
	requireExactKeys(record, [
		"schemaVersion",
		"apiPresence",
		"probeOutcome",
		"probeDeadlineMs"
	], ["failureCode", "failureMessage"], "capability evidence");
	const failureCodeValue = optionalField(record, "failureCode");
	const failureMessageValue = optionalField(record, "failureMessage");
	const result = {
		schemaVersion: requireLiteral(record.schemaVersion, 1, "capability schema version"),
		apiPresence: requireEnum(record.apiPresence, RTC_API_PRESENCE, "RTC API presence"),
		probeOutcome: requireEnum(record.probeOutcome, CAPABILITY_PROBE_OUTCOMES, "capability probe outcome"),
		probeDeadlineMs: requireLiteral(record.probeDeadlineMs, CAPABILITY_PROBE_DEADLINE_MS, "capability probe deadline"),
		...failureCodeValue === void 0 ? {} : { failureCode: requireEnum(failureCodeValue, CAPABILITY_PROBE_FAILURE_CODES, "capability probe failure code") },
		...failureMessageValue === void 0 ? {} : { failureMessage: requireString(failureMessageValue, "capability failure message", 512) }
	};
	validateCapabilityCombination(result);
	return freezeRecord(result);
}
function validateCapabilityCombination(evidence) {
	const failed = evidence.probeOutcome === "failed";
	if (failed !== (evidence.failureCode !== void 0)) contractError("only a failed capability probe may carry a failure code, and it must carry one");
	if (evidence.failureMessage !== void 0 !== failed) contractError("only a failed capability probe may carry a failure message, and it must carry one");
	if (evidence.apiPresence !== "present" && evidence.probeOutcome !== "not-started") contractError("a capability probe cannot run before native RTC API presence is proved");
}
var MAIN_ROUTE_MODES = Object.freeze(["relay-only", "hot-switch"]);
var DISPATCH_ROUTES = Object.freeze(["relay", "peer"]);
function parseMainRouteEvidence(value) {
	if (value === null) return null;
	const evidence = requireRecord(value, "main route evidence");
	requireExactKeys(evidence, ["mode", "observations"], [], "main route evidence");
	const mode = requireEnum(evidence.mode, MAIN_ROUTE_MODES, "main route mode");
	const observations = Object.freeze(requireArray(evidence.observations, "main route observations").map((item, index) => parseRouteObservation(item, index)));
	validateObservationSequences(observations);
	if (mode === "relay-only") validateRelayOnly(observations);
	else validateHotSwitch(observations);
	return freezeRecord({
		mode,
		observations
	});
}
function parseRouteObservation(value, index) {
	const observation = requireRecord(value, `route observation ${index}`);
	const kind = requireEnum(observation.kind, [
		"dispatch",
		"peer-admitted",
		"relay-cut-fence"
	], `route observation ${index} kind`);
	const observationSequence = requireSafeInteger(observation.observationSequence, 1, Number.MAX_SAFE_INTEGER, `route observation ${index} sequence`);
	if (kind === "dispatch") {
		requireExactKeys(observation, [
			"observationSequence",
			"kind",
			"dispatchSequence",
			"route",
			"lane"
		], [], `route dispatch observation ${index}`);
		const route = requireEnum(observation.route, DISPATCH_ROUTES, `route dispatch ${index} route`);
		return freezeRecord({
			observationSequence,
			kind,
			dispatchSequence: requireSafeInteger(observation.dispatchSequence, 1, Number.MAX_SAFE_INTEGER, `route dispatch ${index} sequence`),
			route,
			lane: parseLane(observation.lane, `route dispatch ${index} lane`, route === "relay" ? "relay" : "peer")
		});
	}
	if (kind === "peer-admitted") {
		requireExactKeys(observation, [
			"observationSequence",
			"kind",
			"sessionId",
			"peerPathId",
			"attemptId",
			"lane"
		], [], `peer admission observation ${index}`);
		return freezeRecord({
			observationSequence,
			kind,
			sessionId: requireCanonicalIdentity(observation.sessionId, "route admission session ID"),
			peerPathId: requireCanonicalIdentity(observation.peerPathId, "route admission peer path ID"),
			attemptId: requireCanonicalIdentity(observation.attemptId, "route admission attempt ID"),
			lane: parseLane(observation.lane, "route admission lane", "peer")
		});
	}
	requireExactKeys(observation, [
		"observationSequence",
		"kind",
		"dispatchSequenceBoundary",
		"proxyAccepting",
		"receiverRelayEligible"
	], [], `relay cut fence observation ${index}`);
	if (requireBoolean(observation.proxyAccepting, "relay cut proxy accepting") || requireBoolean(observation.receiverRelayEligible, "receiver relay eligibility")) contractError("completed relay cut fence must stop proxy admission and receiver relay eligibility");
	return freezeRecord({
		observationSequence,
		kind,
		dispatchSequenceBoundary: requireSafeInteger(observation.dispatchSequenceBoundary, 1, Number.MAX_SAFE_INTEGER, "relay cut dispatch sequence boundary"),
		proxyAccepting: false,
		receiverRelayEligible: false
	});
}
function validateObservationSequences(observations) {
	let expectedDispatchSequence = 1;
	for (let index = 0; index < observations.length; index += 1) {
		const observation = observations[index];
		if (observation === void 0 || observation.observationSequence !== index + 1) contractError("route observation sequence must be contiguous from one");
		if (observation.kind === "dispatch") {
			if (observation.dispatchSequence !== expectedDispatchSequence) contractError("route dispatch sequence must be contiguous from one");
			expectedDispatchSequence += 1;
		}
	}
}
function validateRelayOnly(observations) {
	if (observations.length === 0 || observations.some((observation) => observation.kind !== "dispatch" || observation.route !== "relay")) contractError("relay-only evidence must contain only relay dispatch observations");
}
function validateHotSwitch(observations) {
	const admissions = observations.filter((observation) => observation.kind === "peer-admitted");
	const fences = observations.filter((observation) => observation.kind === "relay-cut-fence");
	if (admissions.length !== 1 || fences.length !== 1) contractError("hot-switch evidence requires exactly one peer admission and relay cut fence");
	const admission = admissions[0];
	const fence = fences[0];
	if (admission === void 0 || fence === void 0 || admission.observationSequence >= fence.observationSequence) contractError("hot-switch peer admission must precede the relay cut fence");
	const dispatches = observations.filter((observation) => observation.kind === "dispatch");
	const relayBeforeAdmission = dispatches.some((dispatch) => dispatch.route === "relay" && dispatch.observationSequence < admission.observationSequence);
	const peerBeforeAdmission = dispatches.some((dispatch) => dispatch.route === "peer" && dispatch.observationSequence < admission.observationSequence);
	const peerOnUnadmittedLane = dispatches.some((dispatch) => dispatch.route === "peer" && !sameLane(dispatch.lane, admission.lane));
	const lastBeforeFence = dispatches.filter((dispatch) => dispatch.observationSequence < fence.observationSequence).at(-1);
	const peerAfterFence = dispatches.some((dispatch) => dispatch.route === "peer" && dispatch.observationSequence > fence.observationSequence && dispatch.dispatchSequence > fence.dispatchSequenceBoundary && sameLane(dispatch.lane, admission.lane));
	const relayAfterFence = dispatches.some((dispatch) => dispatch.route === "relay" && dispatch.observationSequence > fence.observationSequence);
	if (!relayBeforeAdmission || peerBeforeAdmission || peerOnUnadmittedLane || lastBeforeFence === void 0 || lastBeforeFence.dispatchSequence !== fence.dispatchSequenceBoundary || !peerAfterFence || relayAfterFence) contractError("hot-switch evidence does not prove relay, admission, cut fence, and post-fence peer dispatch");
}
function parseLane(value, label, authority) {
	const lane = requireRecord(value, label);
	requireExactKeys(lane, ["laneId", "laneEpoch"], [], label);
	return freezeRecord({
		laneId: requireSafeInteger(lane.laneId, 1, 4294967295, `${label} ID`),
		laneEpoch: requireSafeInteger(lane.laneEpoch, authority === "relay" ? 0 : 1, authority === "relay" ? 0 : 4294967295, `${label} epoch`)
	});
}
function sameLane(left, right) {
	return left.laneId === right.laneId && left.laneEpoch === right.laneEpoch;
}
var BROWSER_RUN_POLICY_IDS = Object.freeze([
	"blocking",
	"closure",
	"stability"
]);
var POLICY_SAMPLE_COUNTS = Object.freeze({
	blocking: 1,
	closure: 3,
	stability: 5
});
var POLICIES = Object.freeze(Object.fromEntries(BROWSER_RUN_POLICY_IDS.map((policyId) => [policyId, freezeRecord({
	schemaVersion: 1,
	policyId,
	policyVersion: 1,
	sampleCount: POLICY_SAMPLE_COUNTS[policyId]
})])));
function browserRunPolicy(policyId) {
	return POLICIES[policyId];
}
function parseBrowserRunPolicy(value, label = "browser run policy") {
	const record = requireRecord(value, label);
	requireExactKeys(record, [
		"schemaVersion",
		"policyId",
		"policyVersion",
		"sampleCount"
	], [], label);
	const canonical = browserRunPolicy(requireEnum(record.policyId, BROWSER_RUN_POLICY_IDS, `${label} identity`));
	requireLiteral(record.schemaVersion, 1, `${label} schema version`);
	requireLiteral(record.policyVersion, 1, `${label} version`);
	requireLiteral(record.sampleCount, canonical.sampleCount, `${label} sample count`);
	return canonical;
}
function validatePolicySampleIndex(sampleIndex, policy, label = "sample index") {
	if (!Number.isSafeInteger(sampleIndex) || sampleIndex < 1 || sampleIndex > policy.sampleCount) throw new Error(`${label} must be in [1, ${policy.sampleCount}] for ${policy.policyId}@${policy.policyVersion}`);
	return sampleIndex;
}
function parseBrowserSampleResult(value, topologyLock) {
	const { profile: topology, resolution, profileSha256: expectedProfileSha256, resolutionSha256: expectedResolutionSha256 } = readVerifiedTestIceTopologyLock(topologyLock);
	const record = requireRecord(value, "browser sample result");
	return requireEnum(record.suite, ["main", "pion"], "browser sample suite") === "main" ? parseMainResult(record, topology, resolution, expectedProfileSha256, expectedResolutionSha256) : parsePionResult(record, topology, resolution, expectedProfileSha256, expectedResolutionSha256);
}
function parseMainResult(record, topology, resolution, expectedProfileSha256, expectedResolutionSha256) {
	requireExactKeys(record, [
		...commonResultFields(),
		"suite",
		"peerAttemptOutcome",
		"deliveryOutcome",
		"attempts",
		"deliveryEvidence",
		"routeEvidence"
	], [], "main browser sample result");
	const common = parseCommonResult(record, topology, resolution, expectedProfileSha256, expectedResolutionSha256);
	const peerAttemptOutcome = requireEnum(record.peerAttemptOutcome, PEER_ATTEMPT_OUTCOMES, "peer attempt outcome");
	const deliveryOutcome = requireEnum(record.deliveryOutcome, DELIVERY_OUTCOMES, "delivery outcome");
	const attempts = common.resultStatus === "provisional" ? parseProvisionalAttempts(record.attempts) : parseLogicalAttempts(record.attempts, common.resultStatus === "final-invalid");
	const deliveryEvidence = parseDeliveryEvidence(record.deliveryEvidence, deliveryOutcome);
	const routeEvidence = parseMainRouteEvidence(record.routeEvidence);
	if (common.resultStatus !== "provisional" && reducePeerAttemptOutcome(attempts) !== peerAttemptOutcome) contractError("peer attempt outcome does not match the failed-dominant reducer");
	if (common.resultStatus === "provisional" && peerAttemptOutcome !== "not-started") contractError("provisional main result must not assert a peer terminal");
	if (common.resultStatus === "provisional" && deliveryOutcome !== "not-started") contractError("provisional main result must not assert a delivery terminal");
	if (common.resultStatus === "provisional" && routeEvidence !== null) contractError("provisional main result cannot assert route evidence");
	if (common.resultStatus !== "provisional" && routeEvidence?.mode === "hot-switch") validateHotSwitchAttemptCorrelation(routeEvidence, attempts);
	return freezeRecord({
		...common,
		suite: requireLiteral(record.suite, "main", "main result suite"),
		peerAttemptOutcome,
		deliveryOutcome,
		attempts,
		deliveryEvidence,
		routeEvidence
	});
}
function parsePionResult(record, topology, resolution, expectedProfileSha256, expectedResolutionSha256) {
	requireExactKeys(record, [
		...commonResultFields(),
		"suite",
		"applicability",
		"nativeInteropOutcome",
		"nativeInteropEvidence"
	], [], "Pion browser sample result");
	const common = parseCommonResult(record, topology, resolution, expectedProfileSha256, expectedResolutionSha256);
	const applicability = requireEnum(record.applicability, PION_APPLICABILITY, "Pion suite applicability");
	const nativeInteropOutcome = requireEnum(record.nativeInteropOutcome, NATIVE_INTEROP_OUTCOMES, "native Pion interop outcome");
	const nativeInteropEvidence = parseNativeInteropEvidence(record.nativeInteropEvidence, nativeInteropOutcome, topology, resolution);
	validatePionCombination(common, applicability, nativeInteropOutcome, nativeInteropEvidence);
	return freezeRecord({
		...common,
		suite: requireLiteral(record.suite, "pion", "Pion result suite"),
		applicability,
		nativeInteropOutcome,
		nativeInteropEvidence
	});
}
function parseCommonResult(record, topology, resolution, expectedProfileSha256, expectedResolutionSha256) {
	const parsedTopology = parseTestIceTopology(topology);
	parseTestIceTopologyResolution(resolution, parsedTopology, expectedProfileSha256);
	const resultStatus = requireEnum(record.resultStatus, RESULT_STATUSES, "result status");
	const capabilityEvidence = parseCapabilityEvidence(record.capabilityEvidence);
	const rtcCapability = requireEnum(record.rtcCapability, RTC_CAPABILITIES, "RTC capability");
	if (classifyRtcCapability(capabilityEvidence) !== rtcCapability) contractError("RTC capability does not match capability evidence");
	const executionEvidence = parseExecutionEvidence(record.executionEvidence);
	const executionOutcome = requireEnum(record.executionOutcome, EXECUTION_OUTCOMES, "execution outcome");
	if (classifyExecutionOutcome(executionEvidence) !== executionOutcome) contractError("execution outcome does not match execution evidence");
	const integrityViolations = Object.freeze(requireArray(record.integrityViolations, "result integrity violations").map((item, index) => requireString(item, `integrity violation ${index}`, 1024)).sort(compareStrings$1));
	if (new Set(integrityViolations).size !== integrityViolations.length) contractError("result integrity violations contain duplicates");
	if (resultStatus === "final-invalid" ? integrityViolations.length === 0 : integrityViolations.length !== 0) contractError("only a final-invalid result must carry integrity violations");
	const playwrightOutcome = requireEnum(record.playwrightOutcome, PLAYWRIGHT_OUTCOMES, "Playwright outcome");
	validateRunnerProcessVerdict(resultStatus, playwrightOutcome, executionEvidence);
	const artifacts = parseArtifacts(record.artifacts);
	if (resultStatus === "provisional") {
		if (rtcCapability !== "unknown" || executionOutcome !== "unknown" || playwrightOutcome !== "not-started" || capabilityEvidence.apiPresence !== "unknown" || artifacts.length !== 0) contractError("provisional result must retain unknown/not-started outcomes");
	}
	if (resultStatus === "final-valid" && (executionOutcome === "unknown" || playwrightOutcome === "not-started")) contractError("final-valid result requires terminal execution and Playwright evidence");
	const topologyProfileSha256 = requireSha256(record.topologyProfileSha256, "result topology profile SHA-256");
	if (topologyProfileSha256 !== requireSha256(expectedProfileSha256, "expected topology profile SHA-256")) contractError("result topology profile SHA-256 does not match the selected profile");
	const topologyResolutionSha256 = requireSha256(record.topologyResolutionSha256, "result topology resolution SHA-256");
	if (topologyResolutionSha256 !== requireSha256(expectedResolutionSha256, "expected topology resolution SHA-256")) contractError("result topology resolution SHA-256 does not match the selected resolution");
	const runPolicy = parseBrowserRunPolicy(record.runPolicy, "result run policy");
	const sampleIndex = requireSafeInteger(record.sampleIndex, 1, runPolicy.sampleCount, "sample index");
	validatePolicySampleIndex(sampleIndex, runPolicy);
	return freezeRecord({
		schemaVersion: requireLiteral(record.schemaVersion, 1, "browser result schema version"),
		resultStatus,
		runId: requireRunId(record.runId),
		runPolicy,
		browser: requireEnum(record.browser, BROWSER_ENGINES, "browser engine"),
		sampleIndex,
		checkoutSha: requireCheckoutSha(record.checkoutSha, "checkout SHA"),
		topologyId: requireLiteral(record.topologyId, parsedTopology.topologyId, "result topology ID"),
		topologyProfileSha256,
		topologyResolutionSha256,
		rtcCapability,
		capabilityEvidence,
		executionOutcome,
		executionEvidence,
		playwrightOutcome,
		artifacts,
		integrityViolations
	});
}
function parseDeliveryEvidence(value, outcome) {
	if (value === null) {
		if (outcome !== "not-started") contractError("started delivery outcome lacks delivery evidence");
		return null;
	}
	if (outcome === "not-started") contractError("not-started delivery cannot carry delivery evidence");
	const evidence = requireRecord(value, "delivery evidence");
	requireExactKeys(evidence, [
		"expectedBytes",
		"receivedBytes",
		"expectedSha256",
		"receivedSha256",
		"terminal"
	], [], "delivery evidence");
	const receivedDigest = evidence.receivedSha256 === null ? null : requireSha256(evidence.receivedSha256, "received delivery SHA-256");
	const result = freezeRecord({
		expectedBytes: requireSafeInteger(evidence.expectedBytes, MAIN_TRANSFER_BYTES, MAIN_TRANSFER_BYTES, "expected delivery bytes"),
		receivedBytes: requireSafeInteger(evidence.receivedBytes, 0, MAIN_TRANSFER_BYTES, "received delivery bytes"),
		expectedSha256: requireLiteral(evidence.expectedSha256, MAIN_TRANSFER_SHA256, "expected delivery SHA-256"),
		receivedSha256: receivedDigest,
		terminal: requireEnum(evidence.terminal, DELIVERY_TERMINALS, "delivery terminal")
	});
	if (result.terminal !== outcome) contractError("delivery outcome does not match its terminal evidence");
	if (outcome === "succeeded" && (result.receivedBytes !== 16777216 || result.receivedSha256 !== "25e349f1212bb99491944eb8e885665bb71edc5d5db49d1cd2ef1ffafac1dd5d")) contractError("succeeded delivery does not prove exact bytes and SHA-256");
	return result;
}
function parseNativeInteropEvidence(value, outcome, topology, resolution) {
	if (value === null) {
		if (outcome !== "not-started") contractError("native interop terminal lacks evidence");
		return null;
	}
	if (outcome === "not-started") contractError("not-started native interop cannot carry evidence");
	const evidence = requireRecord(value, "native interop evidence");
	requireExactKeys(evidence, ["browser", "pion"], ["failureCode", "failureMessage"], "native interop evidence");
	const browser = parseNativeInteropSide(evidence.browser, "browser", parseBrowserSelectedPair);
	const pion = parseNativeInteropSide(evidence.pion, "Pion", parsePionSelectedPair);
	if (browser.attemptId !== pion.attemptId) contractError("native browser and Pion evidence identify different attempts");
	const failureCodeValue = optionalField(evidence, "failureCode");
	const failureMessageValue = optionalField(evidence, "failureMessage");
	if (outcome === "failed" ? failureCodeValue === void 0 || failureMessageValue === void 0 : failureCodeValue !== void 0 || failureMessageValue !== void 0) contractError("only failed native interop must carry failure code and message");
	if (outcome === "succeeded" && (browser.selectedPair === null || pion.selectedPair === null || !selectedPairAllowedByTopology(browser.selectedPair, topology, resolution) || !selectedPairAllowedByTopology(pion.selectedPair, topology, resolution) || !selectedPairsCorrelate(browser.selectedPair, pion.selectedPair))) contractError("succeeded native interop lacks a correlated direct browser/Pion selected pair");
	return freezeRecord({
		browser,
		pion,
		...failureCodeValue === void 0 ? {} : { failureCode: requireEnum(failureCodeValue, NATIVE_INTEROP_FAILURE_CODES, "native interop failure code") },
		...failureMessageValue === void 0 ? {} : { failureMessage: requireString(failureMessageValue, "native interop failure message", 512) }
	});
}
function validatePionCombination(common, applicability, outcome, evidence) {
	if (common.resultStatus === "provisional") {
		if (applicability !== "unknown" || outcome !== "not-started" || evidence !== null) contractError("provisional Pion result must retain unknown/not-started applicability");
		return;
	}
	if (applicability !== applicabilityForApiPresence(common.capabilityEvidence.apiPresence)) contractError("Pion applicability must be derived from authoritative RTC API presence");
	if (applicability !== "applicable") {
		if (outcome !== "not-started" || evidence !== null) contractError("unknown or absent RTC API cannot carry native Pion attempt evidence");
		return;
	}
	if (common.resultStatus === "final-valid" && (outcome === "not-started" || evidence === null)) contractError("final API-present Pion result requires a native interop terminal");
}
function applicabilityForApiPresence(presence) {
	if (presence === "unknown") return "unknown";
	return presence === "absent" ? "not-applicable" : "applicable";
}
function parseNativeInteropSide(value, side, parseSelectedPair) {
	const evidence = requireRecord(value, `${side} native interop evidence`);
	requireExactKeys(evidence, ["attemptId", "selectedPair"], [], `${side} native interop evidence`);
	return freezeRecord({
		attemptId: requireAttemptIdentity(evidence.attemptId),
		selectedPair: evidence.selectedPair === null ? null : parseSelectedPair(evidence.selectedPair)
	});
}
function parseArtifacts(value) {
	const identities = /* @__PURE__ */ new Set();
	const paths = /* @__PURE__ */ new Set();
	const portablePaths = /* @__PURE__ */ new Set();
	const artifacts = requireArray(value, "artifact index").map((item, index) => {
		const artifact = requireRecord(item, `artifact ${index}`);
		requireExactKeys(artifact, [
			"artifactId",
			"kind",
			"relativePath",
			"mediaType",
			"byteLength",
			"sha256"
		], [], `artifact ${index}`);
		const relativePath = requireRelativeArtifactPath(artifact.relativePath, index);
		const parsed = freezeRecord({
			artifactId: requireArtifactId(artifact.artifactId, index),
			kind: requireEnum(artifact.kind, ARTIFACT_KINDS, `artifact ${index} kind`),
			relativePath,
			mediaType: requireString(artifact.mediaType, `artifact ${index} media type`, 128),
			byteLength: requireSafeInteger(artifact.byteLength, 0, Number.MAX_SAFE_INTEGER, `artifact ${index} byte length`),
			sha256: requireSha256(artifact.sha256, `artifact ${index} SHA-256`)
		});
		if (parsed.artifactId !== artifactIdForManifest(parsed)) contractError(`artifact ${index} ID does not bind its exact manifest`);
		const artifactId = parsed.artifactId;
		const portablePath = portablePathCollisionKey(relativePath);
		if (identities.has(artifactId)) contractError(`artifact ID ${artifactId} appears more than once`);
		if (paths.has(relativePath)) contractError(`artifact path ${relativePath} appears more than once`);
		if (portablePaths.has(portablePath)) contractError(`artifact path ${relativePath} collides on a portable filesystem`);
		identities.add(artifactId);
		paths.add(relativePath);
		portablePaths.add(portablePath);
		return parsed;
	}).sort((left, right) => comparePortablePaths(left.relativePath, right.relativePath) || compareStrings$1(left.artifactId, right.artifactId));
	return Object.freeze(artifacts);
}
function parseProvisionalAttempts(value) {
	if (requireArray(value, "provisional attempts").length !== 0) contractError("provisional result cannot assert peer attempt evidence");
	return Object.freeze([]);
}
function commonResultFields() {
	return [
		"schemaVersion",
		"resultStatus",
		"runId",
		"runPolicy",
		"browser",
		"sampleIndex",
		"checkoutSha",
		"topologyId",
		"topologyProfileSha256",
		"topologyResolutionSha256",
		"rtcCapability",
		"capabilityEvidence",
		"executionOutcome",
		"executionEvidence",
		"playwrightOutcome",
		"artifacts",
		"integrityViolations"
	];
}
function requireRunId(value) {
	const runId = requireString(value, "browser run ID", 128);
	if (!/^[A-Za-z0-9._-]+$/u.test(runId)) contractError("browser run ID contains non-portable characters");
	return runId;
}
function requireAttemptIdentity(value) {
	return requireCanonicalIdentity(value, "native attempt ID");
}
function requireArtifactId(value, index) {
	const artifactId = requireString(value, `artifact ${index} ID`, 128);
	if (!/^[A-Za-z0-9._-]+$/u.test(artifactId)) contractError(`artifact ${index} ID contains non-portable characters`);
	return artifactId;
}
function requireRelativeArtifactPath(value, index) {
	try {
		return requirePortableRelativePath(value, `artifact ${index} relative path`);
	} catch (cause) {
		contractError(cause instanceof Error ? cause.message : String(cause));
	}
}
function compareStrings$1(left, right) {
	if (left === right) return 0;
	return left < right ? -1 : 1;
}
Object.freeze([
	"not-started",
	"passed",
	"quarantined",
	"failed"
]);
Object.freeze([
	"not-started",
	"completed",
	"failed"
]);
Object.freeze([
	"scanner-crashed",
	"invalid-archive",
	"archive-byte-limit",
	"archive-entry-limit",
	"archive-expanded-byte-limit",
	"archive-nesting-limit",
	"archive-path",
	"contract",
	"unexpected"
]);
Object.freeze(["file", "archive-entry"]);
Object.freeze(["explicit-secret", "github-token-pattern"]);
var GUARD_MAXIMUM_ARTIFACT_FILE_BYTES = 536870912;
var GUARD_UPLOAD_TOPOLOGY_PROFILE_PATH = "topology/profile.json";
var GUARD_UPLOAD_TOPOLOGY_RESOLUTION_PATH = "topology/resolution.json";
var MAXIMUM_SAMPLE_RESULT_BYTES = 16 * 1024 * 1024;
var MAXIMUM_GUARD_RESULT_BYTES = 1 * 1024 * 1024;
var MAXIMUM_TOPOLOGY_BYTES = 1 * 1024 * 1024;
function parseGuardUploadManifest(encoded) {
	const record = requireRecord(parseCanonicalJsonText(encoded, "guard upload manifest"), "guard upload manifest");
	requireExactKeys(record, [
		"schemaVersion",
		"runId",
		"runPolicy",
		"suite",
		"checkoutSha",
		"topology",
		"samples"
	], [], "guard upload manifest");
	const runPolicy = parseBrowserRunPolicy(record.runPolicy, "guard upload run policy");
	const manifest = freezeRecord({
		schemaVersion: requireLiteral(record.schemaVersion, 2, "guard upload manifest schema version"),
		runId: requirePortableToken(record.runId, "guard upload run ID"),
		runPolicy,
		suite: requireEnum(record.suite, BROWSER_SUITES, "guard upload suite"),
		checkoutSha: requireCheckoutSha(record.checkoutSha, "guard upload checkout SHA"),
		topology: parseTopologyManifest(record.topology),
		samples: parseSampleManifests(record.samples, runPolicy)
	});
	requireExactSampleSlots(manifest.samples, runPolicy);
	if (JSON.stringify(manifest) !== encoded) throw new Error("guard upload manifest is not canonical JSON");
	return manifest;
}
function parseTopologyManifest(value) {
	const record = requireRecord(value, "guard upload topology");
	requireExactKeys(record, ["profile", "resolution"], [], "guard upload topology");
	return freezeRecord({
		profile: parseFileAuthority(record.profile, GUARD_UPLOAD_TOPOLOGY_PROFILE_PATH, MAXIMUM_TOPOLOGY_BYTES, "guard topology profile"),
		resolution: parseFileAuthority(record.resolution, GUARD_UPLOAD_TOPOLOGY_RESOLUTION_PATH, MAXIMUM_TOPOLOGY_BYTES, "guard topology resolution")
	});
}
function parseSampleManifests(value, runPolicy) {
	const samples = requireArray(value, "guard upload samples").map((item, index) => {
		const record = requireRecord(item, `guard upload sample ${index}`);
		requireExactKeys(record, [
			"browser",
			"sampleIndex",
			"sampleResultByteLength",
			"sampleResultSha256",
			"guardResultByteLength",
			"guardResultSha256",
			"artifactManifestSha256",
			"artifacts"
		], [], `guard upload sample ${index}`);
		const artifacts = parseArtifactManifests(record.artifacts);
		const sample = freezeRecord({
			browser: requireEnum(record.browser, BROWSER_ENGINES, `guard upload sample ${index} browser`),
			sampleIndex: validatePolicySampleIndex(requireSafeInteger(record.sampleIndex, 1, runPolicy.sampleCount, `guard upload sample ${index} index`), runPolicy, `guard upload sample ${index} index`),
			sampleResultByteLength: requireDecimal(record.sampleResultByteLength, MAXIMUM_SAMPLE_RESULT_BYTES, `guard upload sample ${index} result byte length`),
			sampleResultSha256: requireSha256(record.sampleResultSha256, `guard upload sample ${index} result SHA-256`),
			guardResultByteLength: requireDecimal(record.guardResultByteLength, MAXIMUM_GUARD_RESULT_BYTES, `guard upload sample ${index} guard byte length`),
			guardResultSha256: requireSha256(record.guardResultSha256, `guard upload sample ${index} guard SHA-256`),
			artifactManifestSha256: requireSha256(record.artifactManifestSha256, `guard upload sample ${index} artifact manifest SHA-256`),
			artifacts
		});
		if (sample.artifactManifestSha256 !== artifactManifestSha256(artifacts.map(numericArtifactManifest))) throw new Error(`guard upload sample ${index} does not bind its exact artifact index`);
		return sample;
	});
	const canonical = [...samples].sort(compareSampleManifests);
	if (samples.some((sample, index) => sample !== canonical[index])) throw new Error("guard upload samples are not canonically ordered");
	requireExactSampleSlots(samples, runPolicy);
	return Object.freeze(samples);
}
function parseArtifactManifests(value) {
	const values = requireArray(value, "guard upload artifact index");
	if (values.length > 1e4) throw new Error("guard upload artifact index exceeds the frozen file-count limit");
	let totalBytes = 0;
	const identities = /* @__PURE__ */ new Set();
	const paths = /* @__PURE__ */ new Set();
	const portablePaths = /* @__PURE__ */ new Set();
	const artifacts = values.map((item, index) => {
		const record = requireRecord(item, `guard upload artifact ${index}`);
		requireExactKeys(record, [
			"artifactId",
			"kind",
			"relativePath",
			"mediaType",
			"byteLength",
			"sha256"
		], [], `guard upload artifact ${index}`);
		const relativePath = requirePortableRelativePath(record.relativePath, `guard upload artifact ${index} relative path`);
		const artifact = freezeRecord({
			artifactId: requirePortableToken(record.artifactId, `guard upload artifact ${index} ID`),
			kind: requireEnum(record.kind, ARTIFACT_KINDS, `guard upload artifact ${index} kind`),
			relativePath,
			mediaType: requireString(record.mediaType, `guard upload artifact ${index} media type`, 128),
			byteLength: requireDecimal(record.byteLength, GUARD_MAXIMUM_ARTIFACT_FILE_BYTES, `guard upload artifact ${index} byte length`),
			sha256: requireSha256(record.sha256, `guard upload artifact ${index} SHA-256`)
		});
		const numeric = numericArtifactManifest(artifact);
		if (artifact.artifactId !== artifactIdForManifest(numeric)) throw new Error(`guard upload artifact ${index} ID does not bind its exact manifest`);
		totalBytes += numeric.byteLength;
		if (!Number.isSafeInteger(totalBytes) || totalBytes > 2147483648) throw new Error("guard upload artifact index exceeds the frozen total-byte limit");
		const portablePath = portablePathCollisionKey(relativePath);
		if (identities.has(artifact.artifactId) || paths.has(relativePath) || portablePaths.has(portablePath)) throw new Error("guard upload artifact index contains duplicate or colliding authority");
		identities.add(artifact.artifactId);
		paths.add(relativePath);
		portablePaths.add(portablePath);
		return artifact;
	});
	const canonical = [...artifacts].sort(compareArtifactManifests);
	if (artifacts.some((artifact, index) => artifact !== canonical[index])) throw new Error("guard upload artifact index is not canonically ordered");
	return Object.freeze(artifacts);
}
function numericArtifactManifest(artifact) {
	return freezeRecord({
		...artifact,
		byteLength: Number(artifact.byteLength)
	});
}
function parseFileAuthority(value, expectedPath, maximumBytes, label) {
	const record = requireRecord(value, label);
	requireExactKeys(record, [
		"relativePath",
		"byteLength",
		"sha256"
	], [], label);
	const relativePath = requirePortableRelativePath(record.relativePath, `${label} path`);
	if (relativePath !== expectedPath) throw new Error(`${label} path is not canonical`);
	return freezeRecord({
		relativePath,
		byteLength: requireDecimal(record.byteLength, maximumBytes, `${label} byte length`),
		sha256: requireSha256(record.sha256, `${label} SHA-256`)
	});
}
function requireExactSampleSlots(samples, runPolicy) {
	const expected = BROWSER_ENGINES.flatMap((browser) => Array.from({ length: runPolicy.sampleCount }, (_, offset) => `${browser}/${offset + 1}`));
	if (!sameOrderedStrings(samples.map(({ browser, sampleIndex }) => `${browser}/${sampleIndex}`), expected)) throw new Error("guard upload does not contain every canonical browser/sample slot exactly once");
}
function requirePortableToken(value, label) {
	const token = requireString(value, label, 128);
	if (!/^[A-Za-z0-9._-]+$/u.test(token)) throw new Error(`${label} contains non-portable characters`);
	return token;
}
function requireDecimal(value, maximum, label) {
	const encoded = requireString(value, label, 32);
	if (!/^(?:0|[1-9]\d*)$/u.test(encoded)) throw new Error(`${label} is not canonical unsigned decimal`);
	const numeric = Number(encoded);
	if (!Number.isSafeInteger(numeric) || numeric > maximum) throw new Error(`${label} exceeds its byte authority`);
	return encoded;
}
function compareArtifactManifests(left, right) {
	return comparePortablePaths(left.relativePath, right.relativePath) || compareStrings(left.artifactId, right.artifactId);
}
function compareSampleManifests(left, right) {
	return compareSampleSlots(left, right);
}
function compareSampleSlots(left, right) {
	return compareStrings(left.browser, right.browser) || left.sampleIndex - right.sampleIndex;
}
function compareStrings(left, right) {
	if (left === right) return 0;
	return left < right ? -1 : 1;
}
function sameOrderedStrings(left, right) {
	return left.length === right.length && left.every((value, index) => value === right[index]);
}
/** The dependency-free workflow bundle consumes the producer's exact v2 parser. */
function parseFinalGuardUploadManifest(encoded) {
	return parseGuardUploadManifest(encoded);
}
/**
* Both the guard-side adapter and the dependency-free workflow reducer call
* this exact boundary. Bundling this module commits one semantic authority
* while retaining the typed producer parsers as its source of truth.
*/
async function evaluateFinalBrowserSample(input) {
	const profile = parseTestIceTopologyJson(input.topologyProfileJson);
	const topologyLock = await verifyTestIceTopologyLock(profile, parseTestIceTopologyResolutionJson(input.topologyResolutionJson, profile, input.topologyProfileSha256), input.topologyProfileSha256, input.topologyResolutionSha256);
	const result = parseBrowserSampleResult(input.result, topologyLock);
	const disposition = result.suite === "main" ? (validateMainAcceptance(result, topologyLock), "accepted") : validatePionAcceptance(result);
	return Object.freeze({
		result,
		disposition
	});
}
export { evaluateFinalBrowserSample, parseFinalGuardUploadManifest };
