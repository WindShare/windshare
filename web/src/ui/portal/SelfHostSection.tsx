export function SelfHostSection() {
  return (
    <section id="self-host" className="portal-section">
      <div className="portal-container">
        <div className="portal-selfhost-box">
          <div className="portal-selfhost-text">
            <span className="portal-section-tag">OPEN & DECENTRALIZED</span>
            <h2>100% 开源 · 人人皆可自建中转</h2>
            <p>
              WindShare 完全开源，拒绝任何云端锁定。所有中转服务器均可独立部署在您的私有 VPS、
              局域网或 Cloudflare 上。配合官方或私有前端，构建完全受您掌控的数据直传网络。
            </p>
            <div style={{ display: 'flex', gap: '14px', flexWrap: 'wrap' }}>
              <a
                href="https://github.com/windshare/windshare"
                target="_blank"
                rel="noopener noreferrer"
                className="portal-btn-github"
                style={{ padding: '10px 18px' }}
              >
                📖 查看自建中转指南 →
              </a>
            </div>
          </div>

          <div className="portal-terminal-preview" aria-hidden="true">
            <div className="portal-terminal-header">
              <span className="portal-terminal-dot dot-red" />
              <span className="portal-terminal-dot dot-yellow" />
              <span className="portal-terminal-dot dot-green" />
              <span style={{ fontSize: '0.75rem', color: '#9ca3af', marginLeft: '6px' }}>wsrelay - private node</span>
            </div>
            <div className="portal-terminal-content">
              <div className="terminal-line terminal-comment"># 启动私有 WindShare 中转信令节点</div>
              <div className="terminal-line">
                <span className="portal-code-prompt">$</span> ./wsrelay --addr 0.0.0.0:8080
              </div>
              <div className="terminal-line" style={{ color: '#6ee7b7' }}>
                [INFO] wsrelay v2.0 listening on :8080 (TLS/WSS)
              </div>
              <div className="terminal-line" style={{ color: '#6ee7b7' }}>
                [INFO] End-to-end encrypted envelope routing ready
              </div>
              <div className="terminal-line" style={{ color: '#38bdf8' }}>
                [INFO] P2P ICE discovery and relay fallback active
              </div>
            </div>
          </div>
        </div>
      </div>
    </section>
  )
}
