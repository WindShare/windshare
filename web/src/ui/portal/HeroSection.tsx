import { useState, type FormEvent } from 'react'
import type { V2ReceiverController } from '../v2-controller'
import type { V2ReceiverSnapshot } from '../v2-model'
import type { V2RetainedReceiveOperation } from '../v2-receive-runtime'

function ReceiveTab({
  controller,
  retainedOp,
}: {
  readonly controller: V2ReceiverController
  readonly retainedOp: V2RetainedReceiveOperation | null
}) {
  const [keyInput, setKeyInput] = useState('')

  const handlePaste = async () => {
    try {
      if (navigator.clipboard?.readText) {
        const text = await navigator.clipboard.readText()
        if (text) setKeyInput(text.trim())
      }
    } catch {
      // Ignore clipboard read error
    }
  }

  const handleReceiveSubmit = (e: FormEvent) => {
    e.preventDefault()
    if (keyInput.trim()) {
      controller.submitKey(keyInput.trim())
    }
  }

  return (
    <form className="portal-key-form" onSubmit={handleReceiveSubmit}>
      <div className="portal-key-input-wrapper">
        <input
          type="text"
          className="portal-key-input"
          placeholder="粘贴 WindShare 完整链接、口令或密钥 (例如 https://...#...)"
          value={keyInput}
          onChange={(e) => setKeyInput(e.target.value)}
          autoComplete="off"
          spellCheck={false}
          required
        />
        <button
          type="button"
          className="portal-btn-paste"
          onClick={handlePaste}
          title="从系统剪贴板粘贴"
        >
          <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
            <rect width="8" height="4" x="8" y="2" rx="1" ry="1" />
            <path d="M16 4h2a2 2 0 0 1 2 2v14a2 2 0 0 1-2 2H6a2 2 0 0 1-2-2V6a2 2 0 0 1 2-2h2" />
          </svg>
          粘贴
        </button>
      </div>

      <button type="submit" className="portal-btn-submit">
        <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.4" strokeLinecap="round" strokeLinejoin="round">
          <circle cx="12" cy="12" r="10" />
          <polygon points="10 8 16 12 10 16 10 8" />
        </svg>
        开启安全直连接收
      </button>

      <p className="portal-input-hint">
        💡 密钥与解密过程完全在您的本地浏览器内存中运行，地址栏与中转服务器绝不接触任何明文密钥。
      </p>

      {retainedOp && retainedOp.actions.length > 0 && retainedOp.actions[0] && (
        <div className="portal-retained-alert">
          <div className="portal-retained-info">
            <strong>发现本地未完成的接收任务</strong>
            <p>浏览器保留了断点检查点或等待保存的文件产物。</p>
          </div>
          <button
            type="button"
            className="portal-btn-resume"
            onClick={() => {
              const action = retainedOp.actions[0]
              if (action) {
                controller.performRetainedAction(retainedOp, action)
              }
            }}
          >
            一键恢复任务 →
          </button>
        </div>
      )}
    </form>
  )
}

function CliTab() {
  const [selectedOs, setSelectedOs] = useState<'macos' | 'windows' | 'linux'>('macos')
  const [copiedKey, setCopiedKey] = useState<string | null>(null)

  const handleCopy = (text: string, id: string) => {
    navigator.clipboard?.writeText(text)
    setCopiedKey(id)
    setTimeout(() => setCopiedKey(null), 2000)
  }

  const getOsCommand = () => {
    switch (selectedOs) {
      case 'macos':
        return 'brew install windshare'
      case 'windows':
        return 'scoop install windshare'
      case 'linux':
        return 'curl -fsSL https://windshare.top/install.sh | bash'
    }
  }

  return (
    <div className="portal-cli-view">
      <div>
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '8px' }}>
          <span style={{ fontSize: '0.85rem', color: 'var(--portal-text-muted)', fontWeight: 600 }}>
            1. 一键安装命令行客户端
          </span>
          <div className="portal-cli-os-tabs">
            <button
              type="button"
              className={`portal-cli-os-btn ${selectedOs === 'macos' ? 'active' : ''}`}
              onClick={() => setSelectedOs('macos')}
            >
              macOS
            </button>
            <button
              type="button"
              className={`portal-cli-os-btn ${selectedOs === 'windows' ? 'active' : ''}`}
              onClick={() => setSelectedOs('windows')}
            >
              Windows
            </button>
            <button
              type="button"
              className={`portal-cli-os-btn ${selectedOs === 'linux' ? 'active' : ''}`}
              onClick={() => setSelectedOs('linux')}
            >
              Linux
            </button>
          </div>
        </div>
        <div className="portal-code-block">
          <span className="portal-code-text">
            <span className="portal-code-prompt">$</span>
            {getOsCommand()}
          </span>
          <button
            type="button"
            className="portal-btn-copy"
            onClick={() => handleCopy(getOsCommand(), 'install')}
          >
            {copiedKey === 'install' ? '✓ 已复制' : '复制'}
          </button>
        </div>
      </div>

      <div>
        <span style={{ display: 'block', fontSize: '0.85rem', color: 'var(--portal-text-muted)', fontWeight: 600, marginBottom: '8px' }}>
          2. 一行命令即刻分享文件或文件夹
        </span>
        <div className="portal-code-block">
          <span className="portal-code-text">
            <span className="portal-code-prompt">$</span>
            windshare share ./My-Project-Folder
          </span>
          <button
            type="button"
            className="portal-btn-copy"
            onClick={() => handleCopy('windshare share ./My-Project-Folder', 'share')}
          >
            {copiedKey === 'share' ? '✓ 已复制' : '复制'}
          </button>
        </div>
      </div>
    </div>
  )
}

function DesktopTab() {
  return (
    <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(200px, 1fr))', gap: '16px' }}>
      <a
        href="https://github.com/windshare/windshare/releases"
        target="_blank"
        rel="noopener noreferrer"
        className="portal-feature-card"
        style={{ padding: '20px', textDecoration: 'none', textAlign: 'center', alignItems: 'center' }}
      >
        <div className="portal-feature-icon" style={{ margin: '0 0 12px' }}>🪟</div>
        <h3 style={{ fontSize: '1.05rem', margin: '0 0 4px' }}>Windows 版</h3>
        <p style={{ margin: 0, fontSize: '0.82rem' }}>支持鼠标右键一键快速分享</p>
      </a>
      <a
        href="https://github.com/windshare/windshare/releases"
        target="_blank"
        rel="noopener noreferrer"
        className="portal-feature-card"
        style={{ padding: '20px', textDecoration: 'none', textAlign: 'center', alignItems: 'center' }}
      >
        <div className="portal-feature-icon" style={{ margin: '0 0 12px' }}>🍏</div>
        <h3 style={{ fontSize: '1.05rem', margin: '0 0 4px' }}>macOS 版</h3>
        <p style={{ margin: 0, fontSize: '0.82rem' }}>支持 Apple Silicon & Intel 架构</p>
      </a>
      <a
        href="https://github.com/windshare/windshare/releases"
        target="_blank"
        rel="noopener noreferrer"
        className="portal-feature-card"
        style={{ padding: '20px', textDecoration: 'none', textAlign: 'center', alignItems: 'center' }}
      >
        <div className="portal-feature-icon" style={{ margin: '0 0 12px' }}>🐧</div>
        <h3 style={{ fontSize: '1.05rem', margin: '0 0 4px' }}>Linux 版</h3>
        <p style={{ margin: 0, fontSize: '0.82rem' }}>提供 AppImage 与通用二进制包</p>
      </a>
    </div>
  )
}

export function HeroSection({
  controller,
  snapshot,
}: {
  readonly controller: V2ReceiverController
  readonly snapshot: V2ReceiverSnapshot
}) {
  const [activeTab, setActiveTab] = useState<'receive' | 'cli' | 'desktop'>('receive')

  const retainedOp: V2RetainedReceiveOperation | null =
    snapshot.retained.kind === 'ready' && snapshot.retained.operations.length > 0
      ? (snapshot.retained.operations[0] ?? null)
      : null

  return (
    <section className="portal-hero">
      <div className="portal-container">
        <div className="portal-hero-eyebrow">
          <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round">
            <rect width="18" height="11" x="3" y="11" rx="2" ry="2" />
            <path d="M7 11V7a5 5 0 0 1 10 0v4" />
          </svg>
          端到端加密 · WebRTC P2P 直连 · 零云端中转
        </div>

        <h1>无需上传云端 · 秒出任意文件分享链接</h1>
        <p className="portal-hero-subtitle">
          基于 WebRTC 与零知识 Suite-02 加密规范。选定多文件或多级文件夹，毫秒级出链；
          接收方浏览器免装软件即开即下，大文件不占双倍空间。
        </p>

        {/* Dual Channel Console Card */}
        <div className="portal-console-card">
          <div className="portal-console-tabs" role="tablist">
            <button
              type="button"
              role="tab"
              aria-selected={activeTab === 'receive'}
              className={`portal-tab-btn ${activeTab === 'receive' ? 'active' : ''}`}
              onClick={() => setActiveTab('receive')}
            >
              <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" strokeLinejoin="round">
                <path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4" />
                <polyline points="7 10 12 15 17 10" />
                <line x1="12" x2="12" y1="15" y2="3" />
              </svg>
              极速接收 (Receive)
            </button>
            <button
              type="button"
              role="tab"
              aria-selected={activeTab === 'cli'}
              className={`portal-tab-btn ${activeTab === 'cli' ? 'active' : ''}`}
              onClick={() => setActiveTab('cli')}
            >
              <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" strokeLinejoin="round">
                <polyline points="4 17 10 11 4 5" />
                <line x1="12" x2="20" y1="19" y2="19" />
              </svg>
              CLI 命令行分享
            </button>
            <button
              type="button"
              role="tab"
              aria-selected={activeTab === 'desktop'}
              className={`portal-tab-btn ${activeTab === 'desktop' ? 'active' : ''}`}
              onClick={() => setActiveTab('desktop')}
            >
              <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" strokeLinejoin="round">
                <rect width="20" height="14" x="2" y="3" rx="2" />
                <line x1="8" x2="16" y1="21" y2="21" />
                <line x1="12" x2="12" y1="17" y2="21" />
              </svg>
              桌面客户端
            </button>
          </div>

          <div className="portal-console-body">
            {activeTab === 'receive' && <ReceiveTab controller={controller} retainedOp={retainedOp} />}
            {activeTab === 'cli' && <CliTab />}
            {activeTab === 'desktop' && <DesktopTab />}
          </div>
        </div>
      </div>
    </section>
  )
}
