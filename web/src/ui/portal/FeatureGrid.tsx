export function FeatureGrid() {
  const features = [
    {
      icon: (
        <svg width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" strokeLinejoin="round">
          <polygon points="13 2 3 14 12 14 11 22 21 10 12 10 13 2" />
        </svg>
      ),
      title: '零预上传 · 秒级出链',
      description: '彻底告别漫长的先上传云端等待。无论 1MB 还是 100GB，只要在本机选定文件，毫秒级即可生成端到端加密分享链接。',
      badge: '即发即传 · 零等待',
    },
    {
      icon: (
        <svg width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" strokeLinejoin="round">
          <path d="M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z" />
          <path d="m9 12 2 2 4-4" />
        </svg>
      ),
      title: '零知识端到端加密',
      description: '采用 Suite-02 现代密码学套件。解密密钥仅存在于 URL Fragment (#) 中，不传中转；中转节点对文件名、目录与内容完全零知。',
      badge: 'ChaCha20 · 绝对隐私',
    },
    {
      icon: (
        <svg width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" strokeLinejoin="round">
          <circle cx="12" cy="12" r="10" />
          <line x1="2" y1="12" x2="22" y2="12" />
          <path d="M12 2a15.3 15.3 0 0 1 4 10 15.3 15.3 0 0 1-4 10 15.3 15.3 0 0 1-4-10 15.3 15.3 0 0 1 4-10z" />
        </svg>
      ),
      title: 'WebRTC P2P 直连 + 中转保底',
      description: '优先打通端到端 WebRTC 数据通道极速直传，局域网公网自适应；复杂网络自动启用 WebSocket 中转隧道保底，多路并行。',
      badge: '点对点传输 · 极速吞吐',
    },
    {
      icon: (
        <svg width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" strokeLinejoin="round">
          <path d="M22 19a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h5l2 3h9a2 2 0 0 1 2 2z" />
          <line x1="12" y1="11" x2="12" y2="17" />
          <line x1="9" y1="14" x2="15" y2="14" />
        </svg>
      ),
      title: '原生目录树与断点续传',
      description: '无需打包成巨大压缩包。接收方可直接浏览多级子目录，支持单文件预览与按需下载。原生落盘不占双倍空间，支持 24 小时检查点恢复。',
      badge: '渐进发现 · 无双倍写入',
    },
  ]

  return (
    <section id="features" className="portal-section">
      <div className="portal-container">
        <div className="portal-section-header">
          <span className="portal-section-tag">ARCHITECTURAL HIGHLIGHTS</span>
          <h2>专为隐私、速度与大文件打造的下一代传输体系</h2>
          <p>
            打破传统网盘与中心化传输工具的限制，将数据的控制权彻底还给用户本机。
          </p>
        </div>

        <div className="portal-feature-grid">
          {features.map((item, idx) => (
            <div key={idx} className="portal-feature-card">
              <div className="portal-feature-icon">
                {item.icon}
              </div>
              <h3>{item.title}</h3>
              <p>{item.description}</p>
              <div className="portal-feature-badge">
                <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="3" strokeLinecap="round" strokeLinejoin="round">
                  <polyline points="20 6 9 17 4 12" />
                </svg>
                {item.badge}
              </div>
            </div>
          ))}
        </div>
      </div>
    </section>
  )
}
