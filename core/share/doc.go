// Package share 是纯加密门面(执行计划 §6.6):Sharer/Receiver 组合
// link/layout/chunk/manifest,把全部 IO 以 FileSource/FileSink 接口注入,
// 自身无网络/磁盘副作用。Options 可注入固定 readSecret 与随机源——随机
// nonce 时代金标向量确定性的命脉(附录 B11)。密钥派生在此接线
// (core/internal/keyderiv)。
package share
