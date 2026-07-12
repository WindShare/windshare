# M1a 加密与规范符合性 对抗评审

> 评审员:独立对抗评审(未参与实现) · 只读审查 · 未改代码 / 未 commit
> 范围:`core/internal/keyderiv`、`core/link`、`core/chunk`、`core/manifest`、`core/layout`、`core/share`、`testvectors/`
> 权威规范:`docs/执行计划.md` §6.3–§6.5、§8、附录 B(B1–B15);`docs/M1-实施说明.md`
> 验证手段:通读源码 + 全量 `go test`(通过)+ `go vet`(干净)+ **独立复算全部加密向量**(手写 HMAC/HKDF 与 stdlib GCM,不经被测代码)

## 一句话结论

**未发现严重或中等问题。** keyderiv / link / chunk / manifest / layout / share 与 §6.3–§6.5 及附录 B1–B15 **逐条符合**;四支柱完整性的三类可挡攻击(换块 / 重排 / 跨分享拼接)均被现有校验挡住并有测试覆盖;唯一"可通过校验"的伪造是**持链者在真实几何内替换内容**,这正是规范文档化接受、留待 M2 suite `0x02` 修复的残余风险,非实现缺陷。仅列 3 项轻微/信息性建议。

## 严重(必修):0

无。

## 中等(应修):0

无。

## 轻微(建议):3

### L1 — B15「请升级」提示对触发 `MaxArrayElements` 的敌意 CBOR 不生效(边界说明)
`manifest/manifest.go:93` 的 `probeDecMode` 设 `MaxArrayElements: 1<<20`。版本探测若遇到**声明超大数组**的畸形 CBOR(如 `9b …` 声明数十亿条目、字节截断),fxamacker 在读元素前即按声明数拒绝,`decodeManifest`(`manifest.go:238`)返回的是泛化的「清单 CBOR 解析失败」,而非 §6.4/B15 承诺的「请升级」。
- **影响**:极低。`openSuite`(`manifest.go:207`)已先按 `MaxManifestSize`(16 MiB)拒超限 blob,而合法的未来版本清单(≤16 MiB)装不下 2²⁰ 条目(2²⁰×最小 ~26 B ≈ 27 MiB),故对**良构**未来清单该保证仍成立;此路径只被敌意/损坏输入命中,拒绝语义正确、只是缺升级提示。
- **建议**:文档层面澄清「请升级」保证仅覆盖良构未来清单;或在 probe 失败后对「疑似版本更高」再给一次友好提示。不阻塞 M1a。

### L2 — 折叠碰撞检测未钉死 Unicode 版本,存在 Go↔TS 判定分歧风险(前向,影响 M1b)
`manifest/path.go:119` 的 `foldPath = norm.NFC(cases.Fold(p))` 以 Go stdlib 的全 Unicode 折叠近似 Windows(大小写不敏感)/macOS(归一不敏感)文件系统语义。两点:
1. `cases.Fold()` 比 Windows 的定长大写表更激进,方向是**过拒**(拒绝本可共存的名字),对落盘安全无害;但
2. 该判定**不入金标向量**、双端各自独立执行,`x/text` 的 Unicode 版本随 Go 升级漂移 → M1b 的 TS 端(`cbor-x`/手写)极可能给出**不同**的碰撞判定,造成「一端收、一端拒」同一清单的跨实现不一致。
- **影响**:M1a 单 Go 端无实际问题;M1b Go↔TS 对拍时可能暴露。
- **建议**:M1b 前补一组**折叠碰撞跨实现向量**(含 `ẞ`/`ss`、Turkish `i`/`İ`、NFC↔NFD 等边角),把双端折叠语义钉到同一集合;并在规范注明所依赖的 Unicode 版本口径。

### L3 — 清单 AAD 仅域分隔,未绑定 `shareId`(设计确认,非缺陷)
`manifest/manifest.go:262` 的 `suiteAAD` 只含 `suiteByte`;清单真实性完全依赖 `manifestKey`(由随机唯一的 `readSecret` 派生)。若未来出现两份分享**复用同一 `readSecret`**(当前不变量禁止),其 sealedManifest 可互换。
- **影响**:无(§6.3「keys 每分享唯一性由 readSecret 随机唯一保证」是既定不变量;`shareId` 明确不入密钥/AAD)。
- **建议**:仅信息性记录——本项符合规范,列出以示已考虑「同源密钥绑定」的边界前提。

## 确认合规项(逐条核对,均已独立验证)

### 1. HKDF 派生(§6.3 / M1 说明 §2)— 符合
- `keyderiv.go:44` salt 恒空(`hkdf.Key(…, nil, …)`);label 为精确 ASCII 字面、无结尾 NUL(`keyderiv.go:15-19`);`SegKey` info = `"windshare/v1 seg" ‖ u32_be(seg)`(`keyderiv.go:35-40`,大端)。
- 三键公式与 §6.3 一致。**独立复算**:手写 RFC 5869(空 salt = 32 零字节)对 `secret=00..0f` 与 `ff×16` 复现 `keyderiv.json` 全部 manifestKey/streamKey/segKey(seg=0/1/256/2³²−1)**逐字节一致**。u32_be 字节序由 seg=1 与 seg=256 两例钉死。

### 2. chunk AEAD(§6.3,B1–B3/B12/B13)— 符合
- **随机 nonce**:每次 `Seal` 从注入 rng 取 12B(`chunk.go:123-126`);**rng 故障即返错、绝不产弱 nonce**——`io.ReadFull` 失败在 `st.seals++` 之前返回(`chunk.go:124-127`),`TestSealRNGFailure` 覆盖「读即错」与「源耗尽」两路。
- **输出布局** `nonce‖ct‖tag`:以 nonce 切片为 dst 零拷贝追加 GCM 输出(`chunk.go:123-130`)。
- **AAD = `suiteByte ‖ u64_be(全局 i)`**(`chunk.go:159-164`),**绑定全局块号而非段内序号**——`segOf` 仅用 `i/chunksPerSeg` 选 key、不入 AAD。**独立复算确证**:以 `aad = 0x01‖u64_be(2²⁴)`(全局 index)成功解开 `cross-segment-seg1`;换段内序号则必败。
- **segKey 选取** `seg = i/(SegmentBytes/chunkSize)`(`chunk.go:169`),懒派生并缓存(`chunk.go:178-193`)。
- **Seal 计数熔断**:`st.seals >= MaxSealsPerSegKey(2³²)` 先检、后 `++`(`chunk.go:120-127`)→ 恰允许 2³² 次 Seal/segKey,碰撞界 ≤2⁻³²;**按 Seal 调用次数计非位置数**(`TestSealCountFuse`、`TestConcurrentSealOpen` 断言计数合计 = Seal 次数);他段不受累。
- **Open**:先切 12B nonce、按 `suiteByte` 分派(`parseBlockCT`/`suiteTrailerLen`,`chunk.go:197-218`),**尾部长度不硬编码**——0x01 trailer=0,未知 suite(含 0x02)返 `ErrUnknownSuite`,为 0x02 的 `‖sig(64)` 留位(`TestUnknownSuiteDispatch`)。
- **chunkSize 2 的幂校验** + 上界 `SegmentBytes`(`chunk.go:85`)。
- 常量 `DefaultChunkSize/SegmentBytes/MaxSealsPerSegKey/NonceBytes/TagBytes` 归属 `core/chunk`,值经 `TestProtocolConstants` 钉死。

### 3. manifest(§6.4,B4/B5/B6/B14/B15)— 符合
- **确定性 CBOR**:`encMode = CoreDetEncOptions + NilContainerAsEmpty`(`manifest.go:80-88`);`TestEncodeGoldenBytes` 以独立 hex 字面钉住键序(顶层 v<entries<chunkSize、条目 path<size<isDir<mtime,即 RFC 8949 长度优先字节序)。**独立复算**:以 manifestKey 手工 GCM-Open `manifest-seal.json` 的 sealed,得到的 canonicalCbor 与向量字段**逐字节一致**。
- **严格解码拒非 canonical**:`strictDecMode`(拒重复键/不定长/tag/未知字段)+ **解码后确定性重编码比对**兜住「非最短整数编码/键未排序」(`manifest.go:107-120, 251-257`);`TestDecodeManifest` 覆盖 8 类非 canonical 拒绝。
- **B15 版本宽容探测**:`probeDecMode` 先读 `v`,未知版本报 `ErrUnsupportedVersion`(「请升级」),已知才严格解码(`manifest.go:233-246`);`TestOpenUnsupportedVersionEndToEnd`、`TestDecodeManifest` 的「未来 schema 宽容探测(含不定长)」覆盖。
- **Seal 随机 nonce + aad=suiteByte**(`manifest.go:188-193`);rng=nil 退 crypto/rand,注入固定 rng 可对拍(`TestSealDeterministicBytes` 且与独立 stdlib GCM 对照)。
- **B14 无 offset/streamLen 字段**:`Entry{Path,Size,MTime,IsDir}`、`Manifest{Version,ChunkSize,Entries}`(`manifest.go:56-69`),几何由双端前缀和派生。
- **B7 全量路径校验**:`Validate` 逐条 `ValidatePath`(NFC/保留名/非法字符/控制字符/`.wsresume` 前缀)+ `size≥0` + 前缀和 ≤`MaxStreamBytes`(先比后加、免 int64 回绕)+ 大小写&NFC 折叠碰撞(`manifest.go:124-161`、`path.go`);构建期(Seal 内)与解封后(NewReceiver)**共用同一校验**。
- **MaxManifestSize 预检**:Seal 前按序列化后全长(含 nonce+tag)预检(`manifest.go:181-184`),出链接前即报错;Open 同限先拒超限 blob(`manifest.go:207-209`)。
- `:` 作为 NTFS ADS 字符被拒(`path.go:16`);Windows 保留名按 Win32 名字解析(剥 `.` 主干与结尾空格,`path.go:107-114`)。

### 4. layout(§6.4,B14)— 符合
- 几何纯由 size 前缀和派生;**目录与 size=0 文件不占流**(`layout.go:102-106`);`size≥0` 且前缀和 ≤`MaxStreamBytes`(`layout.go:98-109`,回绕安全写法 `Size > Max-sum`)。
- `ChunkToRanges`(块→文件段,二分定位铺满 `[i·cs, min((i+1)·cs, streamLen))`)与 `ChunksFor`(文件集→升序去重块集,前缀 `p+"/"` 取子树)互逆;整数不溢出有注释保证(i<2⁵³)。`TestEndToEnd`/`TestSelectiveSubset` 等验证双向一致。
- 排序仅为可复现/局部性、不承担安全职责;接收端按数组顺序推导、不验序(§6.4)。

### 5. link(§6.3 / §6.10)— 符合
- fragment = **base64url 无填充**(`RawURLEncoding`)`suiteByte‖readSecret`,恰 23 字符(`TestFragmentEncoding`);`?r=` 自始按**多值列表**解析(`link.go:210`)。
- split-key 宽容解析:纯密钥串 / 带 `#` / 误粘完整链接均可(`decodeKey`,`link.go:236-256`),首尾空白容忍;`Merge` 对裸链接自带 fragment 做一致性核对、冲突报 `ErrKeyConflict`。
- **未知 suite 报「请升级」**:`secretLen`/`Parse` 对非 0x01 返 `ErrUnknownSuite`,不按 0x01 硬解出错误密钥(`link.go:261-267`、`TestParseErrors` 覆盖 0x00/0x02)。向量 `link.json` 双向(结构↔url/bareUrl/keyString)对拍。

### 6. share 门面(§6.6,B9/B11)— 符合
- **SealedManifest Seal-once 字节复用**:清单仅在 `NewSharer` 内 Seal 一次,`SealedManifest()` 返克隆(`sharer.go:71, 128-130`);`TestSealedManifestReuse` 断言两次调用字节一致且不可被调用方改动(GCM tag 即清单指纹,续传锚定)。
- **Rand 同驱块与清单 nonce(B11)**:消耗序 readSecret→shareId→清单 nonce(12B)→逐块 nonce(`share.go:44-51` 文档 + `sharer.go` 接线)。**独立复算确证**:金标分享清单 nonce=`00..0b`、块 0 nonce=`0c..17`、块 1=`18..23`,严格顺序;与 `manifest-seal.json`/`chunk-seal.json` 一致。
- **接收侧解封后跑结构校验**:`NewReceiver` = `manifest.Open`(严格 CBOR)→ `Manifest.Validate`(路径/折叠/前缀和)→ `layout.New`(几何界)→ `chunk.NewCodec`(chunkSize≤SegmentBytes 兜底),四道纵深(`receiver.go:46-68`);`TestReceiverRejectsForgedStructure` 以合法密钥直封畸形清单验证全链拦截。
- **块明文长度校验**:`writeRanges` 在置 have 位前强校 `len(plaintext)==几何`(`receiver.go:243-249`),挡住持链者 Seal 的合法-tag 错长块(`TestWrongLengthBlockRejected`,`ErrBlockLength`);越界块号先于 AEAD 被拒。
- 校验顺序符合 §6.5:重组→取 nonce→AEAD 验证→(几何长度)→落盘;`session/receive.go:485-514` 亦是先 `opener.Open` 后 `sink.WriteBlock`,无明文哈希步。

### 7. 向量(§7,T0.3)— 符合
- **u64 十进制字符串**(`index:"16777216"` 等),防 JS float64 精度损失;协议 base64url 形态原样入 JSON(README 约定 + `frame-codec.json`/`chunk-seal.json` 落实)。
- **`-update` 幂等**:重跑 `go test ./share -update` 后向量文件字节无变化(git status 前后一致),`TestVectorFilesUpToDate` 在常规 CI 守「文件⩵当前实现生成」。
- **RNG 消耗序钉死**:计数 RNG(0x00,0x01,…)使清单 nonce 与逐块 nonce 完全确定且互异。
- 覆盖**末短块**(share-block-2-short-tail)、**跨文件块**(share-block-1)、**跨段块**(cross-segment-seg1,index=2²⁴→seg 1)、**NFC 非 ASCII 路径**(`tree/naïve.txt`,单码点 ï=U+00EF,钉住 CBOR UTF-8 字节)。

### 8. 完整性四支柱(§6.5)与绕过尝试 — 均落地
| 支柱 | 落地点 | 对抗验证 |
|---|---|---|
| ① 每块 GCM tag | `chunk.Open` | 篡改任一字节 → 拒(`TestTamperedBlockRejected`/`TestOpenRejectsTamperAndMisuse`) |
| ② AAD=u64_be(全局i) 位置绑定 | `chunk.aad` | **换块/重排**:块 1 冒充块 2 → 拒(`TestMisplacedBlockRejected`);独立复算确证绑定全局 i |
| ③ 清单根 MAC | `manifest` GCM tag | 篡改 sealed → 拒(`TestOpenRejectsTamperAndWrongKey`);数组顺序即流顺序、被 tag 认证 |
| ④ 同源密钥绑定 | keyderiv 密钥树 | **跨分享拼接**:他分享同号块 → keyB≠keyA → 拒(`TestCrossShareRejected`/`TestOpenRejectsForeignKey`) |

- **截断/丢块**:接收端 `Missing()` 反映、`Finalize` 缺块即 `ErrMissingBlocks`(`TestSelectiveSubset`)——恶意中转丢块只致不完整、非静默损坏。
- **一致性(非安全)**:随机 nonce 根除 nonce 复用灾难;漂移由 `osfs.Source` **读后以打开句柄 fstat 复核**(非按路径二次 stat,消除 TOCTOU 与 symlink 调包,`osfs/source.go:31-51`),变更即 `ErrDrift`→分享级 ERROR 中止(`TestSessionDriftAbort`)。
- **唯一「可通过」的伪造**:持 `readSecret` 者在真实几何内替换块/清单内容——即 §2.2「能验即能造」残余风险,规范明确 M1 文档化接受、M2 由 suite `0x02` 逐块签名封堵。非本层缺陷。

## 附:验证命令(只读)
- `cd core && go test ./internal/keyderiv ./link ./chunk ./manifest ./layout ./share` → 全通过
- `cd core && go vet ./...` → 干净
- 独立复算脚本(手写 HMAC/HKDF + stdlib GCM,不 import 被测包):复现 `keyderiv.json` 全键、`chunk-seal` 跨段/块0 明文、`manifest-seal` canonicalCbor,**全部逐字节一致**
