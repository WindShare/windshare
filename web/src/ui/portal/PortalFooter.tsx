export function PortalFooter() {
  return (
    <footer className="portal-footer">
      <div className="portal-container">
        <div className="portal-footer-inner">
          <div className="portal-footer-left">
            <span className="portal-footer-brand">WindShare · 端到端加密直传</span>
            <span className="portal-footer-note">
              所有解密操作均在您的浏览器内存中独立完成。中转节点无解密密钥，不留存任何明文数据。
            </span>
          </div>

          <div className="portal-footer-links">
            <a href="https://github.com/windshare/windshare" target="_blank" rel="noopener noreferrer">
              GitHub
            </a>
            <a href="https://github.com/windshare/windshare/blob/main/docs/协议规范.md" target="_blank" rel="noopener noreferrer">
              协议规范
            </a>
            <a href="https://github.com/windshare/windshare/issues" target="_blank" rel="noopener noreferrer">
              问题反馈
            </a>
          </div>
        </div>
      </div>
    </footer>
  )
}
