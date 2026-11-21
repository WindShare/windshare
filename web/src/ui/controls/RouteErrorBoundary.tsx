import { Component, type ErrorInfo, type ReactNode } from 'react'

interface RouteErrorBoundaryProps {
  readonly fallback: ReactNode
  readonly onError?: (error: Error, info: ErrorInfo) => void
  readonly children: ReactNode
}

interface RouteErrorBoundaryState {
  readonly failed: boolean
}

/** Contains lazy route chunk and render failures so the document never collapses into a blank root. */
export class RouteErrorBoundary extends Component<RouteErrorBoundaryProps, RouteErrorBoundaryState> {
  state: RouteErrorBoundaryState = { failed: false }

  static getDerivedStateFromError(): RouteErrorBoundaryState {
    return { failed: true }
  }

  componentDidCatch(error: Error, info: ErrorInfo): void {
    this.props.onError?.(error, info)
  }

  render(): ReactNode {
    return this.state.failed ? this.props.fallback : this.props.children
  }
}
