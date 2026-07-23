package protocolcontract

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/fxamacker/cbor/v2"
)

// This package is deliberately test-only. R0 needs one executable source for
// normative bytes without exposing a provisional production codec before R1.
var update = flag.Bool("update", false, "regenerate R0 v2 contract vectors")

const (
	vectorsDir = "../../testvectors"

	wireVersion                  = 2
	suite                        = 2
	chunkSize                    = 1 << 20
	minChunkSize                 = 1 << 10
	maxChunkSize                 = 4 << 20
	segmentBytes                 = 16 << 30
	maxFrameBytes                = 64 << 10
	maxOperationPlaintext        = 65492
	maxFragmentPayload           = 65440
	maxBlockRecordBytes          = 4194816
	maxCatalogPageBytes          = 60 << 10
	maxCatalogPageEntries        = 256
	maxDirectoryEntries          = 1 << 20
	maxSelectedRoots             = 4096
	maxSelectedRootNameBytes     = 1 << 20
	maxDescriptorBytes           = 16 << 10
	maxFileBytes                 = 1<<53 - 1
	leaseTTLSeconds              = 120
	leaseRenewWindowSeconds      = 60
	leaseMaximumSeconds          = 2 * 60 * 60
	revisionGraceSeconds         = 120
	fragmentTimeoutSeconds       = 15
	fragmentTombstoneSeconds     = 30
	maxOpenBatch                 = 64
	maxInitialRangesPerFile      = 256
	maxInitialRangesPerOpen      = 1024
	maxBlockRequestIndices       = 256
	scanConcurrencySession       = 4
	scanConcurrencyShare         = 16
	scanConcurrencyProcess       = 64
	scanWorkSession              = 1 << 20
	scanWorkShare                = 4 << 20
	scanWorkProcess              = 16 << 20
	committedEntriesShare        = 4 << 20
	committedEntriesProcess      = 16 << 20
	catalogMemorySession         = 16 << 20
	catalogMemoryShare           = 64 << 20
	catalogMemoryProcess         = 512 << 20
	catalogSpillShare            = 2 << 30
	catalogSpillProcess          = 16 << 30
	activeLeasesSession          = 32
	activeLeasesShare            = 256
	activeLeasesProcess          = 1024
	activeLanesSession           = 16
	activeLanesShare             = 256
	activeLanesProcess           = 1024
	sealedCacheShare             = 256 << 20
	sealedCacheProcess           = 2 << 30
	receiverCacheSession         = 128 << 20
	receiverCacheProcess         = 1 << 30
	reassemblySession            = 64 << 20
	reassemblyShare              = 256 << 20
	reassemblyProcess            = 1 << 30
	reassemblyRecordsSession     = 16
	reassemblyRecordsShare       = 64
	reassemblyRecordsProcess     = 256
	controlQueueFrames           = 256
	controlQueueBytes            = 4 << 20
	dataQueueFrames              = 1024
	dataQueueBytes               = 64 << 20
	maxDataFairnessBurst         = 8
	senderCrashGraceSeconds      = 60
	relayChallengeSeconds        = 30
	joinStartingSeconds          = 5
	clientHelloReplaySeconds     = 300
	operationTombstoneSeconds    = 30
	applicationRelaySeconds      = 8
	relaySessionTombstoneSeconds = 60
	maxOpaqueCiphertextBytes     = 64 << 10
	opfsStagingJobBytes          = 8 << 30
	opfsStagingProcessBytes      = 16 << 30
	opfsMinimumReserveBytes      = 512 << 20
	outputOpenTransactions       = 32
	sessionCodeSenderStopped     = 0x1008
	v2RelayWebSocketPath         = "/v2/ws"

	domainSenderKey        = "windshare/v2 sender-key\x00"
	domainShareID          = "windshare/v2 share-id\x00"
	domainDescriptor       = "windshare/v2 object/descriptor"
	domainCatalogPage      = "windshare/v2 object/catalog-page"
	domainDirectoryError   = "windshare/v2 object/directory-error"
	domainRevision         = "windshare/v2 object/file-revision"
	domainBlockRecord      = "windshare/v2 object/block-record"
	domainOfflineCommit    = "windshare/v2 object/offline-commit"
	domainClientHello      = "windshare/v2 client-hello\x00"
	domainServerHello      = "windshare/v2 server-hello\x00"
	domainProtocolSession  = "windshare/v2 protocol-session\x00"
	domainHandshake        = "windshare/v2 handshake"
	domainOperation        = "windshare/v2 operation-envelope\x00"
	domainControlOperation = "windshare/v2 control/operation"
	domainTerminal         = "windshare/v2 control/session-terminal"
	domainLaneAttach       = "windshare/v2 control/lane-attach"
	domainLaneHello        = "windshare/v2 lane-hello\x00"
	domainLaneAccept       = "windshare/v2 lane-accept\x00"
	domainLaneReject       = "windshare/v2 lane-reject\x00"
	domainRegister         = "windshare/v2 relay-register\x00"
	domainResume           = "windshare/v2 relay-resume\x00"
	domainStop             = "windshare/v2 relay-stop\x00"
	domainRelayIdentity    = "windshare/v2 relay-identity\x00"

	labelDescriptor  = "windshare/v2 descriptor"
	labelCatalog     = "windshare/v2 catalog"
	labelFileObject  = "windshare/v2 file-object"
	labelRevision    = "windshare/v2 file-revision"
	labelFileSegment = "windshare/v2 file-segment"
	labelSessionAuth = "windshare/v2 session-auth"
	labelTrafficR2S  = "windshare/v2 traffic/receiver-to-sender"
	labelTrafficS2R  = "windshare/v2 traffic/sender-to-receiver"
)

type vectorFile struct {
	Version     int    `json:"version"`
	Kind        string `json:"kind"`
	Description string `json:"description"`
	Cases       []any  `json:"cases"`
}

type fixture struct {
	readSecret     []byte
	edSeed         []byte
	edPrivate      ed25519.PrivateKey
	edPublic       ed25519.PublicKey
	pkHash         []byte
	shareInstance  []byte
	shareIDRaw     []byte
	shareID        string
	syntheticRoot  []byte
	directoryID    []byte
	fileID         []byte
	generation     []byte
	fileRevision   []byte
	operationID    []byte
	receiverID     []byte
	descriptorKey  []byte
	catalogKey     []byte
	fileObjectKey  []byte
	revisionKey    []byte
	fileSegmentKey []byte
}

type senderObjectVector struct {
	Name                 string `json:"name"`
	Domain               string `json:"domain"`
	ContextB64           string `json:"contextB64"`
	ContextHashB64       string `json:"contextHashB64"`
	KeyB64               string `json:"keyB64"`
	NonceB64             string `json:"nonceB64"`
	CanonicalCBORB64     string `json:"canonicalCborB64"`
	AADB64               string `json:"aadB64"`
	SignaturePreimageB64 string `json:"signaturePreimageB64"`
	ObjectB64            string `json:"objectB64"`
}

type sealedObject struct {
	domain            string
	context           []byte
	key               []byte
	nonce             []byte
	plain             []byte
	aad               []byte
	signaturePreimage []byte
	encoded           []byte
}

func TestR0VectorFilesUpToDate(t *testing.T) {
	files := buildVectorFiles(t)
	for _, file := range files {
		encoded, err := json.MarshalIndent(file, "", "  ")
		if err != nil {
			t.Fatalf("marshal %s: %v", file.Kind, err)
		}
		encoded = append(encoded, '\n')
		path := filepath.Join(vectorsDir, file.Kind+".json")
		if *update {
			if err := os.WriteFile(path, encoded, 0o644); err != nil {
				t.Fatalf("write %s: %v", path, err)
			}
			continue
		}
		committed, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s (run go test ./internal/protocolcontract -update): %v", path, err)
		}
		if !bytes.Equal(committed, encoded) {
			t.Fatalf("%s is stale; run go test ./internal/protocolcontract -update", path)
		}
	}
}

func TestR0ContractArithmetic(t *testing.T) {
	f := newFixture(t)
	if len(base64.RawURLEncoding.EncodeToString(slices.Concat([]byte{suite}, f.readSecret, f.pkHash))) != 44 || len(f.shareID) != 16 {
		t.Fatal("suite-02 fragment/share ID text widths changed")
	}
	if 28+aes.BlockSize+maxOperationPlaintext != maxFrameBytes {
		t.Fatalf("operation envelope limit no longer fills MaxFrameSize")
	}
	if 28+aes.BlockSize+52+maxFragmentPayload != maxFrameBytes {
		t.Fatalf("fragment payload limit no longer fills MaxFrameSize")
	}
	if maxCatalogPageEntries > maxDirectoryEntries {
		t.Fatal("one page cannot exceed the directory entry budget")
	}
	if maxSelectedRoots > maxDirectoryEntries || maxSelectedRootNameBytes < maxSelectedRoots {
		t.Fatal("selected-root transaction limits are internally inconsistent")
	}
	if leaseRenewWindowSeconds >= leaseTTLSeconds || revisionGraceSeconds < leaseTTLSeconds {
		t.Fatal("lease timing contract is internally inconsistent")
	}
	if (maxBlockRecordBytes+maxFragmentPayload-1)/maxFragmentPayload != 65 {
		t.Fatal("maximum block record no longer has the frozen fragment count")
	}
}

func TestCanonicalCBORHostileVariantsRejected(t *testing.T) {
	strictOptions := cbor.DecOptions{
		DupMapKey:   cbor.DupMapKeyEnforcedAPF,
		IndefLength: cbor.IndefLengthForbidden,
		TagsMd:      cbor.TagsForbidden,
	}
	mode, err := strictOptions.DecMode()
	if err != nil {
		t.Fatal(err)
	}
	accepts := func(encoded []byte) bool {
		var decoded any
		if err := mode.Unmarshal(encoded, &decoded); err != nil {
			return false
		}
		return bytes.Equal(canonical(t, decoded), encoded)
	}
	if !accepts([]byte{0xa1, 0x00, 0x01}) {
		t.Fatal("reference decoder rejected canonical map")
	}
	for name, hostile := range map[string][]byte{
		"non-minimal key": {0xa1, 0x18, 0x00, 0x01},
		"duplicate key":   {0xa2, 0x00, 0x01, 0x00, 0x02},
		"indefinite map":  {0xbf, 0x00, 0x01, 0xff},
		"tagged map":      {0xc0, 0xa1, 0x00, 0x01},
		"trailing byte":   {0xa1, 0x00, 0x01, 0x00},
	} {
		t.Run(name, func(t *testing.T) {
			if accepts(hostile) {
				t.Fatal("hostile CBOR was accepted as canonical")
			}
		})
	}
}

func buildVectorFiles(t *testing.T) []vectorFile {
	f := newFixture(t)
	objects := buildSenderObjects(t, f)
	objectCases := make([]any, 0, len(objects))
	for _, object := range objects {
		objectCases = append(objectCases, senderObjectVector{
			Name:                 object.domain,
			Domain:               object.domain,
			ContextB64:           b64(object.context),
			ContextHashB64:       b64(hash(object.context)),
			KeyB64:               b64(object.key),
			NonceB64:             b64(object.nonce),
			CanonicalCBORB64:     b64(object.plain),
			AADB64:               b64(object.aad),
			SignaturePreimageB64: b64(object.signaturePreimage),
			ObjectB64:            b64(object.encoded),
		})
	}
	return []vectorFile{
		{Version: 1, Kind: "v2-identity", Description: "Suite 0x02 link identity and domain-separated HKDF contract.", Cases: []any{identityCase(t, f)}},
		{Version: 1, Kind: "v2-sender-objects", Description: "Canonical CBOR, AES-GCM and Ed25519 transport-neutral sender objects.", Cases: objectCases},
		{Version: 1, Kind: "v2-session", Description: "Canonical v2 relay endpoint/identity, purpose-bound register/resume/stop proofs, WS2U/D/O/F, WS2A/B/N, X25519 transcript, traffic keys and sender controls.", Cases: sessionCases(t, f, objects)},
		{Version: 1, Kind: "v2-fragment", Description: "Authenticated BLOCK_FRAGMENT fixed layout and allocation limits.", Cases: fragmentCases(t, f, objects)},
		{Version: 1, Kind: "v2-semantics", Description: "R0 resource, operation-final, selection/timing, ZIP member, lifecycle and crash-commit state contracts.", Cases: semanticCases()},
	}
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	sequence := func(first byte, n int) []byte {
		out := make([]byte, n)
		for i := range out {
			out[i] = first + byte(i)
		}
		return out
	}
	seed := sequence(0x20, ed25519.SeedSize)
	private := ed25519.NewKeyFromSeed(seed)
	public := slices.Clone(private.Public().(ed25519.PublicKey))
	pkHash := first(hash(append([]byte(domainSenderKey), public...)), 16)
	shareInstance := sequence(0x40, 16)
	shareIDRaw := first(hash(slices.Concat([]byte(domainShareID), pkHash)), 12)
	readSecret := sequence(0x00, 16)
	fileID := sequence(0x70, 16)
	revision := sequence(0x90, 16)
	fileObjectKey := derive(t, readSecret, labelFileObject, slices.Concat(shareInstance, fileID))
	revisionKey := derive(t, fileObjectKey, labelRevision, revision)
	var segment [8]byte
	return &fixture{
		readSecret:     readSecret,
		edSeed:         seed,
		edPrivate:      private,
		edPublic:       public,
		pkHash:         pkHash,
		shareInstance:  shareInstance,
		shareIDRaw:     shareIDRaw,
		shareID:        base64.RawURLEncoding.EncodeToString(shareIDRaw),
		syntheticRoot:  sequence(0x50, 16),
		directoryID:    sequence(0x60, 16),
		fileID:         fileID,
		generation:     sequence(0x80, 16),
		fileRevision:   revision,
		operationID:    sequence(0xb0, 16),
		receiverID:     sequence(0xc0, 16),
		descriptorKey:  derive(t, readSecret, labelDescriptor, pkHash),
		catalogKey:     derive(t, readSecret, labelCatalog, shareInstance),
		fileObjectKey:  fileObjectKey,
		revisionKey:    revisionKey,
		fileSegmentKey: derive(t, revisionKey, labelFileSegment, segment[:]),
	}
}

func identityCase(t *testing.T, f *fixture) any {
	t.Helper()
	keyRaw := slices.Concat([]byte{suite}, f.readSecret, f.pkHash)
	return map[string]any{
		"name":               "suite-02-link-and-keys",
		"suite":              suite,
		"readSecretB64":      b64(f.readSecret),
		"senderSeedB64":      b64(f.edSeed),
		"senderPublicKeyB64": b64(f.edPublic),
		"pkHashB64":          b64(f.pkHash),
		"shareInstanceB64":   b64(f.shareInstance),
		"shareIdRawB64":      b64(f.shareIDRaw),
		"shareId":            f.shareID,
		"keyString":          base64.RawURLEncoding.EncodeToString(keyRaw),
		"descriptorKeyB64":   b64(f.descriptorKey),
		"catalogKeyB64":      b64(f.catalogKey),
		"fileIdB64":          b64(f.fileID),
		"fileObjectKeyB64":   b64(f.fileObjectKey),
		"fileRevisionB64":    b64(f.fileRevision),
		"revisionKeyB64":     b64(f.revisionKey),
		"segment":            "0",
		"fileSegmentKeyB64":  b64(f.fileSegmentKey),
	}
}

func buildSenderObjects(t *testing.T, f *fixture) []sealedObject {
	t.Helper()
	descriptor := map[uint64]any{
		0: uint64(1), 1: uint64(2), 2: uint64(2), 3: f.shareInstance,
		4: f.syntheticRoot, 5: uint64(chunkSize), 6: uint64(0x07),
		7: []byte(f.edPublic), 8: uint64(1700000000), 9: "windshare/path/v1-unicode-15.0.0",
	}
	descriptorContext := slices.Concat([]byte{suite}, f.pkHash, f.shareIDRaw)

	entries := []any{
		[]any{uint64(1), f.directoryID, "photos", nil, int64(1700000100), uint64(0), uint64(1)},
		[]any{uint64(2), f.fileID, "readme.txt", uint64(2097175), int64(1700000200), uint64(123000000), uint64(2)},
	}
	page := map[uint64]any{
		0: uint64(1), 1: f.shareInstance, 2: f.syntheticRoot, 3: f.generation,
		4: uint64(0), 5: true, 6: make([]byte, 32), 7: entries, 8: uint64(0),
	}
	pageContext := slices.Concat(f.shareInstance, f.syntheticRoot, u32(0))
	directoryError := map[uint64]any{
		0: uint64(1), 1: f.shareInstance, 2: f.directoryID, 3: fixed(0xa0, 16),
		4: uint64(0x2007), 5: true, 6: uint64(1000),
	}
	directoryErrorContext := slices.Concat(f.shareInstance, f.directoryID)

	revision := map[uint64]any{
		0: uint64(1), 1: f.shareInstance, 2: f.fileID, 3: f.fileRevision,
		4: uint64(2097175), 5: int64(1700000200), 6: uint64(123000000), 7: uint64(2),
	}
	revisionContext := slices.Concat(f.shareInstance, f.fileID)

	data := []byte("WindShare v2 last block")
	block := map[uint64]any{
		0: uint64(1), 1: f.shareInstance, 2: f.fileID, 3: f.fileRevision,
		4: uint64(2), 5: data,
	}
	blockContext := slices.Concat(f.shareInstance, f.fileID, f.fileRevision, u64(2), u32(uint32(len(data))))
	objects := []sealedObject{
		sealObject(t, f, domainDescriptor, descriptorContext, f.descriptorKey, fixed(0xd0, 12), canonical(t, descriptor)),
		sealObject(t, f, domainCatalogPage, pageContext, f.catalogKey, fixed(0xdc, 12), canonical(t, page)),
		sealObject(t, f, domainDirectoryError, directoryErrorContext, f.catalogKey, fixed(0x08, 12), canonical(t, directoryError)),
		sealObject(t, f, domainRevision, revisionContext, f.fileObjectKey, fixed(0xe8, 12), canonical(t, revision)),
		sealObject(t, f, domainBlockRecord, blockContext, f.fileSegmentKey, fixed(0xf4, 12), canonical(t, block)),
	}
	digests := make([][]byte, 0, len(objects))
	var totalCipherBytes uint64
	for _, object := range objects {
		digests = append(digests, hash(object.encoded))
		totalCipherBytes += uint64(len(object.encoded))
	}
	offlineCommit := map[uint64]any{
		0: uint64(1), 1: f.shareInstance, 2: offlineMerkleRoot(digests), 3: uint64(1700003600),
		4: fixed(0x30, 32), 5: uint64(len(objects)), 6: totalCipherBytes,
	}
	return append(objects, sealObject(
		t, f, domainOfflineCommit, slices.Clone(f.shareInstance), f.descriptorKey,
		fixed(0x14, 12), canonical(t, offlineCommit),
	))
}

func offlineMerkleRoot(objectDigests [][]byte) []byte {
	digests := make([][]byte, len(objectDigests))
	for index, digest := range objectDigests {
		digests[index] = slices.Clone(digest)
	}
	slices.SortFunc(digests, bytes.Compare)
	nodes := make([][]byte, len(digests))
	for index, digest := range digests {
		nodes[index] = hash(slices.Concat([]byte{0}, digest))
	}
	for len(nodes) > 1 {
		next := make([][]byte, 0, (len(nodes)+1)/2)
		for index := 0; index < len(nodes); index += 2 {
			right := nodes[index]
			if index+1 < len(nodes) {
				right = nodes[index+1]
			}
			next = append(next, hash(slices.Concat([]byte{1}, nodes[index], right)))
		}
		nodes = next
	}
	return slices.Clone(nodes[0])
}

func sealObject(t *testing.T, f *fixture, domain string, context, key, nonce, plain []byte) sealedObject {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	header := make([]byte, 8)
	header[0] = wireVersion
	binary.BigEndian.PutUint32(header[4:], uint32(len(plain)+aead.Overhead()))
	contextHash := hash(context)
	aad := slices.Concat([]byte(domain), []byte{0}, contextHash, header)
	sealed := aead.Seal(nil, nonce, plain, aad)
	prefix := slices.Concat(header, nonce, sealed)
	preimage := slices.Concat([]byte(domain), []byte{0}, contextHash, prefix)
	signature := ed25519.Sign(f.edPrivate, preimage)
	return sealedObject{
		domain: domain, context: slices.Clone(context), key: slices.Clone(key), nonce: slices.Clone(nonce),
		plain: slices.Clone(plain), aad: aad, signaturePreimage: preimage,
		encoded: slices.Concat(prefix, signature),
	}
}

func sessionCases(t *testing.T, f *fixture, objects []sealedObject) []any {
	t.Helper()
	receiverPrivate := fixed(0x11, 32)
	senderPrivate := fixed(0x51, 32)
	curve := ecdh.X25519()
	receiverKey, err := curve.NewPrivateKey(receiverPrivate)
	if err != nil {
		t.Fatal(err)
	}
	senderKey, err := curve.NewPrivateKey(senderPrivate)
	if err != nil {
		t.Fatal(err)
	}
	sessionAuth := derive(t, f.readSecret, labelSessionAuth, f.shareInstance)
	clientBody := slices.Concat(
		[]byte("WS2C"), []byte{wireVersion}, f.shareInstance, f.receiverID,
		fixed(0x91, 32), receiverKey.PublicKey().Bytes(),
	)
	clientProof := hmacSHA256(sessionAuth, slices.Concat([]byte(domainClientHello), hash(clientBody)))
	clientHello := slices.Concat(clientBody, clientProof)
	laneID := uint32(0x01020304)
	serverBody := slices.Concat(
		[]byte("WS2S"), []byte{wireVersion}, hash(clientHello), fixed(0x71, 32),
		senderKey.PublicKey().Bytes(), u32(laneID), u32(0),
	)
	serverSignature := ed25519.Sign(f.edPrivate, slices.Concat([]byte(domainServerHello), hash(serverBody)))
	serverHello := slices.Concat(serverBody, serverSignature)
	transcriptHash := hash(slices.Concat(clientHello, serverHello))
	protocolSessionID := first(hash(slices.Concat([]byte(domainProtocolSession), transcriptHash)), 16)
	shared, err := receiverKey.ECDH(senderKey.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	handshakeInfo := string(slices.Concat([]byte(domainHandshake), []byte{0}, transcriptHash))
	handshake, err := hkdf.Key(sha256.New, shared, sessionAuth, handshakeInfo, 32)
	if err != nil {
		t.Fatal(err)
	}
	r2s := derive(t, handshake, labelTrafficR2S, transcriptHash)
	s2r := derive(t, handshake, labelTrafficS2R, transcriptHash)

	terminalUnsigned := map[uint64]any{0: uint64(1), 1: uint64(sessionCodeSenderStopped), 2: "Sender stopped"}
	terminalUnsignedBytes := canonical(t, terminalUnsigned)
	terminalUnsignedWrapper := unsignedControlWrapper(t, terminalUnsignedBytes)
	controlPreimage := controlSignaturePreimage(
		domainTerminal, f.shareInstance, protocolSessionID, laneID, 0, 1, 2, 11, nil, terminalUnsignedWrapper,
	)
	terminalSignature := ed25519.Sign(f.edPrivate, controlPreimage)
	terminalBody := signedControlWrapper(t, terminalUnsignedBytes, terminalSignature)
	terminalPlain := canonical(t, map[uint64]any{0: uint64(11), 1: nil, 2: cbor.RawMessage(terminalBody)})
	envelopeNonce := fixed(0x31, 12)
	envelope, envelopeAAD := sealEnvelope(t, s2r, f.shareInstance, protocolSessionID, laneID, 0, 1, 2, envelopeNonce, terminalPlain)

	operationErrorUnsigned := map[uint64]any{
		0: uint64(1), 1: uint64(3), 2: uint64(0x3007), 3: false, 4: nil, 5: "Source revision drifted",
	}
	operationErrorUnsignedBytes := canonical(t, operationErrorUnsigned)
	operationErrorUnsignedWrapper := unsignedControlWrapper(t, operationErrorUnsignedBytes)
	operationErrorPreimage := controlSignaturePreimage(
		domainControlOperation, f.shareInstance, protocolSessionID, laneID, 0, 1, 0, 10, f.operationID, operationErrorUnsignedWrapper,
	)
	operationErrorSignature := ed25519.Sign(f.edPrivate, operationErrorPreimage)
	operationErrorBody := signedControlWrapper(t, operationErrorUnsignedBytes, operationErrorSignature)
	operationErrorPlain := canonical(t, map[uint64]any{0: uint64(10), 1: f.operationID, 2: cbor.RawMessage(operationErrorBody)})
	operationErrorNonce := fixed(0x41, 12)
	operationErrorEnvelope, operationErrorAAD := sealEnvelope(
		t, s2r, f.shareInstance, protocolSessionID, laneID, 0, 1, 0, operationErrorNonce, operationErrorPlain,
	)

	grantOperationID := fixed(0xd1, 16)
	attachedLaneID := uint32(0x05060708)
	attachedLaneEpoch := uint32(1)
	attachNonce := fixed(0x61, 16)
	grantUnsigned := map[uint64]any{
		0: uint64(1), 1: uint64(1), 2: uint64(attachedLaneID), 3: uint64(attachedLaneEpoch), 4: attachNonce,
	}
	grantUnsignedBytes := canonical(t, grantUnsigned)
	grantUnsignedWrapper := unsignedControlWrapper(t, grantUnsignedBytes)
	grantPreimage := controlSignaturePreimage(
		domainLaneAttach, f.shareInstance, protocolSessionID, laneID, 0, 1, 1, 12, grantOperationID, grantUnsignedWrapper,
	)
	grantSignature := ed25519.Sign(f.edPrivate, grantPreimage)
	grantBody := signedControlWrapper(t, grantUnsignedBytes, grantSignature)
	grantPlain := canonical(t, map[uint64]any{0: uint64(12), 1: grantOperationID, 2: cbor.RawMessage(grantBody)})
	grantEnvelope, grantAAD := sealEnvelope(
		t, s2r, f.shareInstance, protocolSessionID, laneID, 0, 1, 1, fixed(0x51, 12), grantPlain,
	)
	laneHelloBase := slices.Concat(
		[]byte("WS2A"), []byte{wireVersion}, f.shareInstance, protocolSessionID,
		u32(attachedLaneID), u32(attachedLaneEpoch), grantOperationID, attachNonce,
	)
	laneHelloProof := hmacSHA256(r2s, slices.Concat([]byte(domainLaneHello), laneHelloBase))
	laneHello := slices.Concat(laneHelloBase, laneHelloProof)
	laneAckBody := slices.Concat([]byte("WS2B"), []byte{wireVersion}, hash(laneHello), fixed(0x81, 16))
	laneAckSignature := ed25519.Sign(f.edPrivate, slices.Concat([]byte(domainLaneAccept), hash(laneAckBody)))
	laneAck := slices.Concat(laneAckBody, laneAckSignature)
	laneRejectCode := byte(5)
	laneRejectRetryMilliseconds := uint32(1000)
	laneRejectBody := slices.Concat(
		[]byte("WS2N"), []byte{wireVersion, laneRejectCode, 0, 0}, hash(laneHello),
		u32(laneRejectRetryMilliseconds),
	)
	laneRejectSignature := ed25519.Sign(f.edPrivate, slices.Concat([]byte(domainLaneReject), hash(laneRejectBody)))
	laneReject := slices.Concat(laneRejectBody, laneRejectSignature)

	var revisionObject []byte
	for _, candidate := range objects {
		if candidate.domain == domainRevision {
			revisionObject = candidate.encoded
			break
		}
	}
	if revisionObject == nil {
		t.Fatal("file-revision vector object is missing")
	}
	openOperationID := fixed(0xe1, 16)
	leaseID := fixed(0xb1, 16)
	openResultsUnsigned := map[uint64]any{
		0: uint64(1), 1: []any{[]any{f.fileID, uint64(0), revisionObject, leaseID, uint64(leaseTTLSeconds * 1000), uint64(leaseRenewWindowSeconds * 1000)}},
	}
	openResultsUnsignedBytes := canonical(t, openResultsUnsigned)
	openResultsUnsignedWrapper := unsignedControlWrapper(t, openResultsUnsignedBytes)
	openResultsPreimage := controlSignaturePreimage(
		domainControlOperation, f.shareInstance, protocolSessionID, attachedLaneID, attachedLaneEpoch,
		1, 0, 4, openOperationID, openResultsUnsignedWrapper,
	)
	openResultsSignature := ed25519.Sign(f.edPrivate, openResultsPreimage)
	openResultsBody := signedControlWrapper(t, openResultsUnsignedBytes, openResultsSignature)
	openResultsPlain := canonical(t, map[uint64]any{0: uint64(4), 1: openOperationID, 2: cbor.RawMessage(openResultsBody)})
	openResultsEnvelope, openResultsAAD := sealEnvelope(
		t, s2r, f.shareInstance, protocolSessionID, attachedLaneID, attachedLaneEpoch, 1, 0, fixed(0x91, 12), openResultsPlain,
	)

	renewOperationID := fixed(0xf1, 16)
	leaseResultUnsigned := map[uint64]any{
		0: uint64(1), 1: leaseID, 2: uint64(leaseTTLSeconds * 1000), 3: uint64(leaseRenewWindowSeconds * 1000),
	}
	leaseResultUnsignedBytes := canonical(t, leaseResultUnsigned)
	leaseResultUnsignedWrapper := unsignedControlWrapper(t, leaseResultUnsignedBytes)
	leaseResultPreimage := controlSignaturePreimage(
		domainControlOperation, f.shareInstance, protocolSessionID, attachedLaneID, attachedLaneEpoch,
		1, 1, 15, renewOperationID, leaseResultUnsignedWrapper,
	)
	leaseResultSignature := ed25519.Sign(f.edPrivate, leaseResultPreimage)
	leaseResultBody := signedControlWrapper(t, leaseResultUnsignedBytes, leaseResultSignature)
	leaseResultPlain := canonical(t, map[uint64]any{0: uint64(15), 1: renewOperationID, 2: cbor.RawMessage(leaseResultBody)})
	leaseResultEnvelope, leaseResultAAD := sealEnvelope(
		t, s2r, f.shareInstance, protocolSessionID, attachedLaneID, attachedLaneEpoch,
		1, 1, fixed(0xa1, 12), leaseResultPlain,
	)

	descriptorDigest := hash(objects[0].encoded)
	resumeToken := fixed(0xa1, 32)
	resumeHash := hash(resumeToken)
	relayBaseURL := "HTTPS://RELAY.EXAMPLE:443/base/?token=a%20b"
	relayDialEndpoint, relayIdentityEndpoint, err := canonicalV2RelayEndpoints(relayBaseURL)
	if err != nil {
		t.Fatalf("canonicalize relay fixture: %v", err)
	}
	relayIdentity := hash(slices.Concat([]byte(domainRelayIdentity), []byte(relayIdentityEndpoint)))
	expires := uint64(1700000030)
	registerPreimage := slices.Concat(
		[]byte(domainRegister), f.shareIDRaw, f.shareInstance, f.pkHash, descriptorDigest,
		resumeHash, relayIdentity, fixed(0x01, 16), fixed(0x21, 32), u64(expires),
	)
	registerSignature := ed25519.Sign(f.edPrivate, registerPreimage)
	registerInit := slices.Concat(
		[]byte("WS2R"), []byte{wireVersion, 0, 0, 0}, f.shareIDRaw, f.shareInstance,
		f.pkHash, descriptorDigest, resumeHash,
	)
	registerChallenge := slices.Concat(
		[]byte("WS2Q"), []byte{wireVersion, 0, 0, 0}, fixed(0x01, 16), fixed(0x21, 32), u64(expires),
	)
	registerProof := slices.Concat(
		[]byte("WS2P"), []byte{wireVersion, 0, 0, 0}, f.edPublic, registerSignature,
	)
	registered := slices.Concat(
		[]byte("WS2K"), []byte{wireVersion, 0, 0, 0}, f.shareIDRaw, f.shareInstance, descriptorDigest,
	)
	join := slices.Concat([]byte("WS2J"), []byte{wireVersion, 0, 0, 0}, f.shareIDRaw)
	descriptorUpload := encodeDescriptorUpload(objects[0].encoded)
	descriptorDelivery := encodeDescriptorDelivery(fixed(0xe1, 8), objects[0].encoded)
	resumeChallengeID := fixed(0x02, 16)
	resumeChallengeNonce := fixed(0x42, 32)
	resumeExpires := expires + 1
	resumeInit := slices.Concat(
		[]byte("WS2R"), []byte{wireVersion, 1, 0, 0}, f.shareIDRaw, f.shareInstance,
		f.pkHash, descriptorDigest, resumeHash,
	)
	resumeChallenge := slices.Concat(
		[]byte("WS2Q"), []byte{wireVersion, 1, 0, 0}, resumeChallengeID,
		resumeChallengeNonce, u64(resumeExpires),
	)
	resumePreimage := slices.Concat(
		[]byte(domainResume), f.shareIDRaw, f.shareInstance, f.pkHash, descriptorDigest,
		resumeHash, relayIdentity, resumeChallengeID, resumeChallengeNonce, u64(resumeExpires),
	)
	resumeSignature := ed25519.Sign(f.edPrivate, resumePreimage)
	resumeProof := slices.Concat(
		[]byte("WS2P"), []byte{wireVersion, 1, 0, 0}, f.edPublic, resumeSignature,
	)
	resumeCredential := slices.Concat([]byte("WS2T"), []byte{wireVersion, 0, 0, 0}, resumeToken)
	stopID := fixed(0x62, 16)
	stopChallengeID := fixed(0x72, 16)
	stopChallengeNonce := fixed(0x82, 32)
	stopExpires := expires + 2
	stopInit := slices.Concat(
		[]byte("WS2X"), []byte{wireVersion, 0, 0, 0}, f.shareIDRaw, f.shareInstance,
		f.pkHash, relayIdentity, stopID,
	)
	stopChallenge := slices.Concat(
		[]byte("WS2Q"), []byte{wireVersion, 2, 0, 0}, stopChallengeID,
		stopChallengeNonce, u64(stopExpires),
	)
	stopPreimage := slices.Concat(
		[]byte(domainStop), f.shareIDRaw, f.shareInstance, f.pkHash, relayIdentity, stopID,
		stopChallengeID, stopChallengeNonce, u64(stopExpires),
	)
	stopSignature := ed25519.Sign(f.edPrivate, stopPreimage)
	stopProof := slices.Concat(
		[]byte("WS2V"), []byte{wireVersion, 0, 0, 0}, f.edPublic, stopSignature,
	)
	stopped := slices.Concat([]byte("WS2Y"), []byte{wireVersion, 0, 0, 0}, stopID)
	opaqueRelaySessionID := fixed(0xe1, 8)
	opaqueCiphertext := fixed(0xc1, 37)
	opaqueRoute := slices.Concat(
		[]byte("WS2O"), []byte{wireVersion, 0, 0, 0}, opaqueRelaySessionID,
		u32(uint32(len(opaqueCiphertext))), opaqueCiphertext,
	)
	sessionRetired := slices.Concat(
		[]byte("WS2F"), []byte{wireVersion, 0, 0, 0}, opaqueRelaySessionID,
	)
	stoppedError := slices.Concat(
		[]byte("WS2E"), []byte{wireVersion, 0}, u16(11), u32(0),
	)

	return []any{
		map[string]any{
			"name": "v2-relay-endpoint-normalization", "cases": v2RelayEndpointCases(),
		},
		map[string]any{
			"name":               "sender-authenticated-x25519-transcript",
			"receiverPrivateB64": b64(receiverPrivate), "receiverPublicB64": b64(receiverKey.PublicKey().Bytes()),
			"senderPrivateB64": b64(senderPrivate), "senderPublicB64": b64(senderKey.PublicKey().Bytes()),
			"sessionAuthKeyB64": b64(sessionAuth), "clientBodyB64": b64(clientBody), "clientProofB64": b64(clientProof),
			"clientHelloB64": b64(clientHello), "serverBodyB64": b64(serverBody), "serverSignatureB64": b64(serverSignature),
			"serverHelloB64": b64(serverHello), "transcriptHashB64": b64(transcriptHash),
			"protocolSessionIdB64": b64(protocolSessionID), "sharedSecretB64": b64(shared),
			"handshakeSecretB64": b64(handshake), "receiverToSenderKeyB64": b64(r2s), "senderToReceiverKeyB64": b64(s2r),
			"initialLaneId": laneID, "initialLaneEpoch": uint32(0),
		},
		map[string]any{
			"name": "fresh-relay-registration-proof", "relayBaseUrl": relayBaseURL,
			"dialEndpoint": relayDialEndpoint, "relayIdentityEndpoint": relayIdentityEndpoint,
			"descriptorDigestB64": b64(descriptorDigest), "resumeTokenB64": b64(resumeToken),
			"resumeTokenHashB64": b64(resumeHash), "relayIdentityB64": b64(relayIdentity),
			"challengeIdB64": b64(fixed(0x01, 16)), "challengeNonceB64": b64(fixed(0x21, 32)),
			"expiresAt": fmt.Sprint(expires), "preimageB64": b64(registerPreimage), "signatureB64": b64(registerSignature),
			"registerInitB64": b64(registerInit), "registerChallengeB64": b64(registerChallenge),
			"registerProofB64": b64(registerProof), "descriptorUploadB64": b64(descriptorUpload),
			"registeredB64": b64(registered), "joinB64": b64(join),
			"relaySessionIdB64": b64(fixed(0xe1, 8)), "descriptorDeliveryB64": b64(descriptorDelivery),
			"resumeChallengeIdB64": b64(resumeChallengeID), "resumeChallengeNonceB64": b64(resumeChallengeNonce),
			"resumeExpiresAt": fmt.Sprint(resumeExpires), "resumeInitB64": b64(resumeInit),
			"resumeChallengeB64": b64(resumeChallenge), "resumePreimageB64": b64(resumePreimage),
			"resumeSignatureB64": b64(resumeSignature), "resumeProofB64": b64(resumeProof),
			"resumeCredentialB64": b64(resumeCredential), "stopIdB64": b64(stopID),
			"stopChallengeIdB64": b64(stopChallengeID), "stopChallengeNonceB64": b64(stopChallengeNonce),
			"stopExpiresAt": fmt.Sprint(stopExpires), "stopInitB64": b64(stopInit),
			"stopChallengeB64": b64(stopChallenge), "stopPreimageB64": b64(stopPreimage),
			"stopSignatureB64": b64(stopSignature), "stopProofB64": b64(stopProof), "stoppedB64": b64(stopped),
			"opaqueRelaySessionIdB64": b64(opaqueRelaySessionID), "opaqueCiphertextB64": b64(opaqueCiphertext),
			"opaqueRouteB64": b64(opaqueRoute), "sessionRetiredB64": b64(sessionRetired),
			"sessionRetiredRelaySessionIdB64": b64(opaqueRelaySessionID), "stoppedErrorB64": b64(stoppedError),
		},
		map[string]any{
			"name": "sender-signed-operation-error", "shareInstanceB64": b64(f.shareInstance),
			"protocolSessionIdB64": b64(protocolSessionID), "laneId": laneID, "laneEpoch": uint32(0), "sequence": "0",
			"operationIdB64": b64(f.operationID), "trafficKeyB64": b64(s2r),
			"semanticBodyCborB64": b64(operationErrorUnsignedBytes), "unsignedControlCborB64": b64(operationErrorUnsignedWrapper),
			"controlPreimageB64": b64(operationErrorPreimage), "controlSignatureB64": b64(operationErrorSignature),
			"signedControlCborB64": b64(operationErrorBody),
			"plaintextB64":         b64(operationErrorPlain), "nonceB64": b64(operationErrorNonce), "aadB64": b64(operationErrorAAD), "envelopeB64": b64(operationErrorEnvelope),
		},
		map[string]any{
			"name": "sender-granted-lane-attach", "shareInstanceB64": b64(f.shareInstance),
			"protocolSessionIdB64": b64(protocolSessionID), "grantLaneId": laneID, "grantLaneEpoch": uint32(0), "grantSequence": "1",
			"attachedLaneId": attachedLaneID, "attachedLaneEpoch": attachedLaneEpoch, "grantOperationIdB64": b64(grantOperationID),
			"attachNonceB64": b64(attachNonce), "receiverToSenderKeyB64": b64(r2s), "senderToReceiverKeyB64": b64(s2r),
			"grantSemanticBodyCborB64": b64(grantUnsignedBytes), "unsignedGrantCborB64": b64(grantUnsignedWrapper),
			"grantPreimageB64": b64(grantPreimage), "grantSignatureB64": b64(grantSignature), "grantSignedBodyB64": b64(grantBody),
			"grantPlaintextB64": b64(grantPlain), "grantAadB64": b64(grantAAD), "grantEnvelopeB64": b64(grantEnvelope),
			"laneHelloBaseB64": b64(laneHelloBase), "laneHelloProofB64": b64(laneHelloProof), "laneHelloB64": b64(laneHello),
			"laneAckBodyB64": b64(laneAckBody), "laneAckSignatureB64": b64(laneAckSignature), "laneAckB64": b64(laneAck),
			"laneRejectCode": laneRejectCode, "laneRejectRetryAfterMilliseconds": laneRejectRetryMilliseconds,
			"laneRejectBodyB64": b64(laneRejectBody), "laneRejectSignatureB64": b64(laneRejectSignature),
			"laneRejectB64": b64(laneReject),
		},
		map[string]any{
			"name": "relative-lease-open-results", "shareInstanceB64": b64(f.shareInstance),
			"protocolSessionIdB64": b64(protocolSessionID), "laneId": attachedLaneID, "laneEpoch": attachedLaneEpoch, "sequence": "0",
			"operationIdB64": b64(openOperationID), "fileIdB64": b64(f.fileID), "revisionObjectB64": b64(revisionObject),
			"leaseIdB64": b64(leaseID), "leaseTtlMilliseconds": fmt.Sprint(leaseTTLSeconds * 1000), "renewAfterMilliseconds": fmt.Sprint(leaseRenewWindowSeconds * 1000),
			"trafficKeyB64": b64(s2r), "semanticBodyCborB64": b64(openResultsUnsignedBytes),
			"unsignedControlCborB64": b64(openResultsUnsignedWrapper),
			"controlPreimageB64":     b64(openResultsPreimage), "controlSignatureB64": b64(openResultsSignature),
			"signedControlCborB64": b64(openResultsBody),
			"plaintextB64":         b64(openResultsPlain), "nonceB64": b64(fixed(0x91, 12)), "aadB64": b64(openResultsAAD), "envelopeB64": b64(openResultsEnvelope),
		},
		map[string]any{
			"name": "renew-lease-result", "shareInstanceB64": b64(f.shareInstance),
			"protocolSessionIdB64": b64(protocolSessionID), "laneId": attachedLaneID,
			"laneEpoch": attachedLaneEpoch, "sequence": "1", "operationIdB64": b64(renewOperationID),
			"leaseIdB64": b64(leaseID), "leaseTtlMilliseconds": fmt.Sprint(leaseTTLSeconds * 1000),
			"renewAfterMilliseconds": fmt.Sprint(leaseRenewWindowSeconds * 1000),
			"trafficKeyB64":          b64(s2r), "semanticBodyCborB64": b64(leaseResultUnsignedBytes),
			"unsignedControlCborB64": b64(leaseResultUnsignedWrapper),
			"controlPreimageB64":     b64(leaseResultPreimage), "controlSignatureB64": b64(leaseResultSignature),
			"signedControlCborB64": b64(leaseResultBody),
			"plaintextB64":         b64(leaseResultPlain), "nonceB64": b64(fixed(0xa1, 12)),
			"aadB64": b64(leaseResultAAD), "envelopeB64": b64(leaseResultEnvelope),
		},
		map[string]any{
			"name": "sender-signed-session-terminal", "shareInstanceB64": b64(f.shareInstance),
			"protocolSessionIdB64": b64(protocolSessionID), "laneId": laneID, "laneEpoch": uint32(0), "sequence": "2",
			"trafficKeyB64": b64(s2r), "semanticBodyCborB64": b64(terminalUnsignedBytes),
			"unsignedControlCborB64": b64(terminalUnsignedWrapper),
			"controlPreimageB64":     b64(controlPreimage), "controlSignatureB64": b64(terminalSignature),
			"signedControlCborB64": b64(terminalBody),
			"plaintextB64":         b64(terminalPlain), "nonceB64": b64(envelopeNonce), "aadB64": b64(envelopeAAD), "envelopeB64": b64(envelope),
		},
	}
}

func sealEnvelope(t *testing.T, key, shareInstance, protocolSessionID []byte, laneID, epoch uint32, direction byte, sequence uint64, nonce, plain []byte) ([]byte, []byte) {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	ctLen := uint32(len(plain) + aead.Overhead())
	header := make([]byte, 16)
	header[0], header[1] = wireVersion, direction
	binary.BigEndian.PutUint64(header[4:12], sequence)
	binary.BigEndian.PutUint32(header[12:16], ctLen)
	aad := operationAAD(shareInstance, protocolSessionID, laneID, epoch, direction, sequence, ctLen)
	ciphertext := aead.Seal(nil, nonce, plain, aad)
	return slices.Concat(header, nonce, ciphertext), aad
}

func fragmentCases(t *testing.T, f *fixture, objects []sealedObject) []any {
	return []any{singleFragmentCase(t, f, objects), maximumFragmentCase(f)}
}

func singleFragmentCase(t *testing.T, f *fixture, objects []sealedObject) any {
	t.Helper()
	var object []byte
	for _, candidate := range objects {
		if candidate.domain == domainBlockRecord {
			object = candidate.encoded
			break
		}
	}
	if object == nil {
		t.Fatal("block-record vector object is missing")
	}
	recordID := first(hash(object), 16)
	fragment := encodeSingleFragment(f.operationID, recordID, object)
	return map[string]any{
		"name": "single-fragment-block-record", "operationIdB64": b64(f.operationID), "recordIdB64": b64(recordID),
		"fragmentIndex": uint32(0), "fragmentCount": uint32(1), "totalLength": fmt.Sprint(len(object)),
		"payloadLength": fmt.Sprint(len(object)), "fragmentPlaintextB64": b64(fragment),
		"maxFrameBytes": maxFrameBytes, "maxOperationPlaintextBytes": maxOperationPlaintext,
		"maxFragmentPayloadBytes": maxFragmentPayload, "maxBlockRecordBytes": maxBlockRecordBytes,
		"fragmentTimeoutSeconds": fragmentTimeoutSeconds, "tombstoneSeconds": fragmentTombstoneSeconds,
		"mutationsRejected": []string{"operation-id", "record-id", "index", "count", "total-length", "payload-length", "last-flag", "conflicting-duplicate"},
	}
}

func maximumFragmentCase(f *fixture) any {
	record := make([]byte, maxBlockRecordBytes)
	for index := range record {
		record[index] = byte(index*31 + 7)
	}
	recordID := first(hash(record), 16)
	count := (len(record) + maxFragmentPayload - 1) / maxFragmentPayload
	digests := make([]string, 0, count)
	var firstFragment, lastFragment []byte
	for index := 0; index < count; index++ {
		start := index * maxFragmentPayload
		end := min(start+maxFragmentPayload, len(record))
		header := make([]byte, 52)
		header[0], header[1] = 1, 8
		if index == count-1 {
			header[2] = 1
		}
		copy(header[4:20], f.operationID)
		copy(header[20:36], recordID)
		binary.BigEndian.PutUint32(header[36:40], uint32(index))
		binary.BigEndian.PutUint32(header[40:44], uint32(count))
		binary.BigEndian.PutUint32(header[44:48], uint32(len(record)))
		binary.BigEndian.PutUint32(header[48:52], uint32(end-start))
		fragment := slices.Concat(header, record[start:end])
		digests = append(digests, b64(hash(fragment)))
		if index == 0 {
			firstFragment = slices.Clone(fragment)
		}
		if index == count-1 {
			lastFragment = slices.Clone(fragment)
		}
	}
	return map[string]any{
		"name": "maximum-block-record-fragmentation", "operationIdB64": b64(f.operationID),
		"recordPattern": "byte((index*31+7)&255)", "recordLength": fmt.Sprint(len(record)),
		"recordDigestB64": b64(hash(record)), "recordIdB64": b64(recordID), "fragmentCount": uint32(count),
		"fragmentDigestsB64": digests, "firstFragmentB64": b64(firstFragment), "lastFragmentB64": b64(lastFragment),
		"firstPayloadLength": fmt.Sprint(maxFragmentPayload), "lastPayloadLength": fmt.Sprint(len(lastFragment) - 52),
		"maxFrameBytes": maxFrameBytes, "maxOperationPlaintextBytes": maxOperationPlaintext,
		"maxFragmentPayloadBytes": maxFragmentPayload, "maxBlockRecordBytes": maxBlockRecordBytes,
	}
}

func legalOperationFinals() map[string][]string {
	return map[string][]string{
		"renew-lease":    {"lease-result", "operation-error"},
		"release-lease":  {"operation-complete", "operation-error"},
		"request-blocks": {"operation-complete", "operation-error"},
	}
}

func operationFinalMatrix() []any {
	finals := legalOperationFinals()
	return []any{
		map[string]any{"request": "renew-lease", "legalFinals": finals["renew-lease"]},
		map[string]any{"request": "release-lease", "legalFinals": finals["release-lease"]},
		map[string]any{"request": "request-blocks", "legalFinals": finals["request-blocks"]},
	}
}

func zipMemberFailure(memberStarted bool) (string, string) {
	if memberStarted {
		return "abort-job", "aborted"
	}
	return "skip-and-report", "completed-with-errors"
}

func zipMemberFailureCases() []any {
	notStartedAction, notStartedOutcome := zipMemberFailure(false)
	startedAction, startedOutcome := zipMemberFailure(true)
	return []any{
		map[string]any{"memberStarted": false, "action": notStartedAction, "jobOutcome": notStartedOutcome},
		map[string]any{"memberStarted": true, "action": startedAction, "jobOutcome": startedOutcome},
	}
}

func semanticCases() []any {
	limits := map[string]string{
		"minChunkBytes": fmt.Sprint(minChunkSize), "maxChunkBytes": fmt.Sprint(maxChunkSize),
		"segmentBytes": fmt.Sprint(segmentBytes), "maxFileBytes": fmt.Sprint(maxFileBytes),
		"maxCatalogPageBytes": fmt.Sprint(maxCatalogPageBytes), "maxCatalogPageEntries": fmt.Sprint(maxCatalogPageEntries),
		"maxDirectoryEntries": fmt.Sprint(maxDirectoryEntries), "maxSelectedRoots": fmt.Sprint(maxSelectedRoots),
		"maxSelectedRootNameBytes": fmt.Sprint(maxSelectedRootNameBytes), "maxDescriptorBytes": fmt.Sprint(maxDescriptorBytes),
		"maxOpenBatch": fmt.Sprint(maxOpenBatch), "maxInitialRangesPerFile": fmt.Sprint(maxInitialRangesPerFile),
		"maxInitialRangesPerOpen": fmt.Sprint(maxInitialRangesPerOpen),
		"maxBlockRequestIndices":  fmt.Sprint(maxBlockRequestIndices), "leaseTTLSeconds": fmt.Sprint(leaseTTLSeconds),
		"leaseRenewWindowSeconds": fmt.Sprint(leaseRenewWindowSeconds), "leaseMaximumSeconds": fmt.Sprint(leaseMaximumSeconds),
		"revisionGraceSeconds": fmt.Sprint(revisionGraceSeconds), "maxFrameBytes": fmt.Sprint(maxFrameBytes),
		"scanConcurrencySession": fmt.Sprint(scanConcurrencySession), "scanConcurrencyShare": fmt.Sprint(scanConcurrencyShare),
		"scanConcurrencyProcess": fmt.Sprint(scanConcurrencyProcess), "scanWorkSession": fmt.Sprint(scanWorkSession),
		"scanWorkShare": fmt.Sprint(scanWorkShare), "scanWorkProcess": fmt.Sprint(scanWorkProcess),
		"committedEntriesShare": fmt.Sprint(committedEntriesShare), "committedEntriesProcess": fmt.Sprint(committedEntriesProcess),
		"catalogMemorySession": fmt.Sprint(catalogMemorySession), "catalogMemoryShare": fmt.Sprint(catalogMemoryShare),
		"catalogMemoryProcess": fmt.Sprint(catalogMemoryProcess), "catalogSpillShare": fmt.Sprint(catalogSpillShare),
		"catalogSpillProcess": fmt.Sprint(catalogSpillProcess), "activeLeasesSession": fmt.Sprint(activeLeasesSession),
		"activeLeasesShare": fmt.Sprint(activeLeasesShare), "activeLeasesProcess": fmt.Sprint(activeLeasesProcess),
		"stableHandlesSession": fmt.Sprint(activeLeasesSession), "stableHandlesShare": fmt.Sprint(activeLeasesShare),
		"stableHandlesProcess": fmt.Sprint(activeLeasesProcess), "activeLanesSession": fmt.Sprint(activeLanesSession),
		"logicalLanesSession": fmt.Sprint(activeLanesSession),
		"activeLanesShare":    fmt.Sprint(activeLanesShare), "activeLanesProcess": fmt.Sprint(activeLanesProcess),
		"sealedCacheShare": fmt.Sprint(sealedCacheShare), "sealedCacheProcess": fmt.Sprint(sealedCacheProcess),
		"receiverCacheSession": fmt.Sprint(receiverCacheSession), "receiverCacheProcess": fmt.Sprint(receiverCacheProcess),
		"reassemblyOperation": fmt.Sprint(maxBlockRecordBytes), "reassemblySession": fmt.Sprint(reassemblySession),
		"reassemblyShare": fmt.Sprint(reassemblyShare), "reassemblyProcess": fmt.Sprint(reassemblyProcess),
		"reassemblyRecordsSession": fmt.Sprint(reassemblyRecordsSession), "reassemblyRecordsShare": fmt.Sprint(reassemblyRecordsShare),
		"reassemblyRecordsProcess": fmt.Sprint(reassemblyRecordsProcess), "controlQueueFrames": fmt.Sprint(controlQueueFrames),
		"controlQueueBytes": fmt.Sprint(controlQueueBytes), "dataQueueFrames": fmt.Sprint(dataQueueFrames),
		"dataQueueBytes": fmt.Sprint(dataQueueBytes), "maxDataFairnessBurst": fmt.Sprint(maxDataFairnessBurst),
		"senderCrashGraceSeconds": fmt.Sprint(senderCrashGraceSeconds), "relayChallengeSeconds": fmt.Sprint(relayChallengeSeconds),
		"joinStartingSeconds": fmt.Sprint(joinStartingSeconds), "clientHelloReplaySeconds": fmt.Sprint(clientHelloReplaySeconds),
		"operationTombstoneSeconds": fmt.Sprint(operationTombstoneSeconds), "applicationRelaySeconds": fmt.Sprint(applicationRelaySeconds),
		"relaySessionTombstoneSeconds": fmt.Sprint(relaySessionTombstoneSeconds),
		"maxOpaqueCiphertextBytes":     fmt.Sprint(maxOpaqueCiphertextBytes),
		"opfsStagingJobBytes":          fmt.Sprint(opfsStagingJobBytes), "opfsStagingProcessBytes": fmt.Sprint(opfsStagingProcessBytes),
		"opfsMinimumReserveBytes": fmt.Sprint(opfsMinimumReserveBytes), "outputOpenTransactions": fmt.Sprint(outputOpenTransactions),
	}
	selection := []any{
		map[string]any{"files": "29", "bytes": fmt.Sprint((8 << 20) - 1), "terminal": true, "failed": false, "class": "small"},
		map[string]any{"files": "30", "bytes": "0", "terminal": true, "failed": false, "class": "large"},
		map[string]any{"files": "1", "bytes": fmt.Sprint(8 << 20), "terminal": true, "failed": false, "class": "large"},
		map[string]any{"files": "30", "bytes": "0", "terminal": false, "failed": false, "class": "large"},
		map[string]any{"files": "1", "bytes": fmt.Sprint(8 << 20), "terminal": true, "failed": true, "class": "large"},
		map[string]any{"files": "0", "bytes": "0", "terminal": false, "failed": false, "class": "unknown"},
		map[string]any{"files": "0", "bytes": "0", "terminal": true, "failed": true, "class": "unknown"},
		map[string]any{"files": "0", "bytes": "0", "terminal": true, "failed": false, "class": "small"},
	}
	checkpointCuts := []any{
		map[string]any{"cut": "after-data-write", "published": false},
		map[string]any{"cut": "after-data-flush", "published": false},
		map[string]any{"cut": "after-journal-write", "published": false},
		map[string]any{"cut": "after-journal-flush", "published": false},
		map[string]any{"cut": "after-install", "published": false},
		map[string]any{"cut": "after-reopen-verify", "published": true},
	}
	return []any{
		map[string]any{"name": "frozen-limits", "values": limits},
		map[string]any{
			"name":      "error-domains",
			"session":   map[string]uint16{"auth": 0x1001, "replay-sequence": 0x1002, "malformed": 0x1003, "version": 0x1004, "budget": 0x1005, "sender-signature": 0x1006, "illegal-terminal": 0x1007, "sender-stopped": sessionCodeSenderStopped},
			"directory": map[string]uint16{"stale": 0x2001, "permission": 0x2002, "collision": 0x2003, "too-wide": 0x2004, "budget": 0x2005, "permanent-io": 0x2006, "transient-io": 0x2007, "cancelled": 0x2008},
			"revision":  map[string]uint16{"stale": 0x3001, "not-found": 0x3002, "unreadable": 0x3003, "unsupported-stability": 0x3004, "quota": 0x3005, "lease-expired": 0x3006, "drift": 0x3007, "invalid-lease": 0x3008},
			"block":     map[string]uint16{"invalid-ref": 0x4001, "out-of-range": 0x4002, "object-auth": 0x4003, "fragment-conflict": 0x4004, "timeout": 0x4005, "cancelled": 0x4006},
			"peer":      map[string]uint16{"negotiation": 0x5001, "timeout": 0x5002, "candidates": 0x5003, "admission": 0x5004},
		},
		map[string]any{"name": "relay-registration-errors", "codes": map[string]uint16{
			"malformed": 1, "unsupported-mode": 2, "share-id-collision": 3, "already-registered": 4,
			"challenge-expired": 5, "invalid-proof": 6, "descriptor-invalid": 7, "not-found": 8,
			"starting": 9, "admission": 10, "stopped": 11,
		}},
		map[string]any{
			"name": "relay-route-lifecycle", "crashGraceSeconds": fmt.Sprint(senderCrashGraceSeconds),
			"sessionTombstoneSeconds": fmt.Sprint(relaySessionTombstoneSeconds),
			"routeBudgetCounts":       []string{"starting", "live", "crash-grace", "stopped-tombstone"},
			"sessionBudgetCounts":     []string{"active", "ended-id-tombstone"},
			"sessionBudgetScopes":     []string{"global", "per-share"},
			"stopStoreOutcomes":       []string{"committed", "definitely-not-committed", "unknown"},
			"explicitStop": []string{
				"per-route-storage-transaction", "durable-tombstone-before-ack", "exact-participant-cleanup-before-ack",
				"unknown-durability-fail-closed", "same-stop-id-resolution", "no-crash-grace", "permanent-reject-same-instance",
			},
			"unexpectedDisconnect":      []string{"drop-sessions", "enter-bounded-crash-grace", "immediate-retirement-during-stop-commit"},
			"stoppedTombstoneRetention": "until-future-authenticated-refresh",
		},
		map[string]any{"name": "selection-classification", "cases": selection, "fileLimitExclusive": "30", "byteLimitExclusive": fmt.Sprint(8 << 20)},
		map[string]any{"name": "operation-final-matrix", "operations": operationFinalMatrix()},
		map[string]any{"name": "connection-timing", "triggers": []any{
			map[string]any{"trigger": "browse", "startsP2P": false, "p2pStartSeconds": nil, "applicationRelayDeadlineSeconds": nil, "outputPicker": "none"},
			map[string]any{"trigger": "preview-click", "startsP2P": true, "p2pStartSeconds": "0", "applicationRelayDeadlineSeconds": fmt.Sprint(applicationRelaySeconds), "outputPicker": "none"},
			map[string]any{"trigger": "download-click", "startsP2P": true, "p2pStartSeconds": "0", "applicationRelayDeadlineSeconds": fmt.Sprint(applicationRelaySeconds), "outputPicker": "synchronous"},
		}, "independentTimers": true, "discoveryCannotDelay": true, "unknownUsesNonSmallTiming": true, "turnInsertionOnly": true},
		map[string]any{"name": "strict-sequence", "cases": []any{
			map[string]any{"epoch": uint32(0), "expected": "0", "candidate": "0", "accepted": true},
			map[string]any{"epoch": uint32(0), "expected": "1", "candidate": "0", "accepted": false},
			map[string]any{"epoch": uint32(0), "expected": "1", "candidate": "2", "accepted": false},
			map[string]any{"epoch": uint32(1), "expected": "0", "candidate": "0", "accepted": true},
			map[string]any{"epoch": uint32(0), "expected": "closed", "candidate": "1", "accepted": false},
		}},
		map[string]any{"name": "lane-epoch-acceptance", "globallyAllocated": []uint32{1, 2, 3, 4, 5, 6, 7}, "cases": []any{
			map[string]any{"lane": uint32(1), "lastAccepted": uint32(3), "candidate": uint32(5), "accepted": true},
			map[string]any{"lane": uint32(1), "lastAccepted": uint32(5), "candidate": uint32(5), "accepted": false},
			map[string]any{"lane": uint32(1), "lastAccepted": uint32(5), "candidate": uint32(4), "accepted": false},
			map[string]any{"lane": uint32(2), "lastAccepted": nil, "candidate": uint32(4), "otherLaneLast": uint32(7), "accepted": true},
		}},
		map[string]any{"name": "output-checkpoint-crash-cuts", "order": []string{"data-write", "data-flush", "journal-write", "journal-flush", "atomic-install", "reopen-verify"}, "cuts": checkpointCuts},
		map[string]any{"name": "output-backend-capabilities", "backends": []any{
			map[string]any{"backend": "fsa", "durability": "none-until-reauthorization-and-reopen-proof", "randomWrite": true, "fileFailureIsolation": true, "mtime": false, "powerLoss": false},
			map[string]any{"backend": "opfs-staging", "durability": "process-restart", "randomWrite": true, "fileFailureIsolation": true, "mtime": false, "powerLoss": false},
			map[string]any{"backend": "single-file-stream", "durability": "none", "randomWrite": false, "fileFailureIsolation": false, "mtime": false, "failureAfterFirstByte": "abort-job"},
			map[string]any{"backend": "zip-stream", "durability": "none", "randomWrite": false, "fileFailureIsolation": false, "mtime": false, "memberStart": "first-local-file-header-byte"},
			map[string]any{"backend": "cli-osfs", "durability": "power-loss-when-file-and-directory-sync-proved-else-process-restart", "randomWrite": true, "fileFailureIsolation": true, "mtime": true},
		}},
		map[string]any{"name": "zip-member-failure", "cases": zipMemberFailureCases()},
		map[string]any{"name": "catalog-transaction", "publishOnlyAfter": []string{"pages", "node-records", "terminal", "budget-charge", "spill-flush", "atomic-commit"}, "preCommitCrashVisible": false},
		map[string]any{"name": "stable-source-platforms", "platforms": []any{
			map[string]any{"platform": "windows-local-ntfs-refs", "mechanism": "deny-share-write-handle+volume-file-id", "supported": true},
			map[string]any{"platform": "linux-local-regular", "mechanism": "device+inode+size+mtime-ns+ctime-ns", "supported": true},
			map[string]any{"platform": "darwin-local-regular", "mechanism": "device+inode+size+mtime-ns+ctime-ns", "supported": true},
			map[string]any{"platform": "other-network-pseudo", "mechanism": "unsupported-stability", "supported": false},
		}},
		map[string]any{
			"name": "offline-lifecycle", "states": []string{"preparing", "live-only", "offline-uploading", "offline-committed", "stopping", "stopped"},
			"transitions": []any{
				map[string]any{"from": "preparing", "event": "registered", "to": "live-only"},
				map[string]any{"from": "preparing", "event": "stop", "to": "stopping"},
				map[string]any{"from": "live-only", "event": "begin-offline", "to": "offline-uploading"},
				map[string]any{"from": "live-only", "event": "stop", "to": "stopping"},
				map[string]any{"from": "offline-uploading", "event": "commit-ack", "to": "offline-committed"},
				map[string]any{"from": "offline-uploading", "event": "stop", "to": "stopping"},
				map[string]any{"from": "stopping", "event": "cleanup-complete", "to": "stopped"},
				map[string]any{"from": "offline-committed", "event": "sender-exit", "to": "offline-committed"},
			},
			"explicitStopEffects":        []string{"reject-join", "signed-session-terminal", "cancel-scan-revision-lanes", "cancel-uncommitted-upload", "challenged-signed-stop", "cleanup-staging"},
			"explicitStopUsesCrashGrace": false, "crashGraceSeconds": "60",
			"unexpectedDisconnectStates": []string{"live-only", "offline-uploading"},
		},
	}
}
