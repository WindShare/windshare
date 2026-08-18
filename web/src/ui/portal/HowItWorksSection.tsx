export function HowItWorksSection() {
  const steps = [
    {
      num: '01',
      title: '本地选定与密码学生成',
      desc: '发送端选择文件或整个文件夹。WindShare 本地生成不可变的目录 generation 与 Suite-02 加密密钥，毫秒级就绪，无需等待上传。',
    },
    {
      num: '02',
      title: '零知识信令与 WebRTC 握手',
      desc: '接收方打开链接，本地解析 URL 凭证。两端通过 Relay 中转交换加密信令，迅速建立点对点 WebRTC DataChannel 直连。',
    },
    {
      num: '03',
      title: '按需拉取与就地落盘',
      desc: '接收方可在线流式预览图片与音视频，或选择原文件夹层级保存。数据流经本地解密后直接落盘，无冗余副本与双倍写入。',
    },
  ]

  return (
    <section id="how-it-works" className="portal-section" style={{ background: 'rgba(0, 0, 0, 0.25)' }}>
      <div className="portal-container">
        <div className="portal-section-header">
          <span className="portal-section-tag">ZERO-CLOUD WORKFLOW</span>
          <h2>数据如何不经云端直达对方设备？</h2>
          <p>
            端到端点对点流式传输，去除中间商，保证极高传输速率与极致隐私安全。
          </p>
        </div>

        <div className="portal-steps-grid">
          {steps.map((step) => (
            <div key={step.num} className="portal-step-card">
              <div className="portal-step-num">{step.num}</div>
              <h3>{step.title}</h3>
              <p>{step.desc}</p>
            </div>
          ))}
        </div>
      </div>
    </section>
  )
}
