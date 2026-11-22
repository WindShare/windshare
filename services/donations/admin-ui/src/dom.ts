export type Child = Node | string | number | null | undefined | false

export interface ElementProps {
  readonly className?: string
  readonly id?: string
  readonly title?: string
  readonly role?: string
  readonly attrs?: Readonly<Record<string, string | boolean | undefined>>
  readonly on?: Readonly<Record<string, EventListener>>
}

const SVG_NS = 'http://www.w3.org/2000/svg'

export function appendChildren(parent: Element | DocumentFragment, children: readonly Child[]): void {
  for (const child of children) {
    if (child === null || child === undefined || child === false) continue
    if (typeof child === 'string' || typeof child === 'number') {
      parent.appendChild(document.createTextNode(String(child)))
    } else {
      parent.appendChild(child)
    }
  }
}

function applyAttrs(
  node: HTMLElement,
  attrs: Readonly<Record<string, string | boolean | undefined>>,
): void {
  for (const [key, value] of Object.entries(attrs)) {
    if (value === undefined || value === false) continue
    node.setAttribute(key, value === true ? '' : value)
  }
}

function applyListeners(
  node: HTMLElement,
  listeners: Readonly<Record<string, EventListener>>,
): void {
  for (const [event, listener] of Object.entries(listeners)) {
    node.addEventListener(event, listener)
  }
}

function applyProps(node: HTMLElement, props: ElementProps): void {
  if (props.className !== undefined) node.className = props.className
  if (props.id !== undefined) node.id = props.id
  if (props.title !== undefined) node.title = props.title
  if (props.role !== undefined) node.setAttribute('role', props.role)
  if (props.attrs !== undefined) applyAttrs(node, props.attrs)
  if (props.on !== undefined) applyListeners(node, props.on)
}

export function h<K extends keyof HTMLElementTagNameMap>(
  tag: K,
  props?: ElementProps | null,
  ...children: Child[]
): HTMLElementTagNameMap[K] {
  const node = document.createElement(tag)
  if (props !== undefined && props !== null) {
    applyProps(node, props)
  }
  appendChildren(node, children)
  return node
}

export function svgEl(
  tag: string,
  attrs?: Readonly<Record<string, string>>,
  ...children: Element[]
): SVGElement {
  const node = document.createElementNS(SVG_NS, tag)
  if (attrs !== undefined) {
    for (const [key, value] of Object.entries(attrs)) {
      node.setAttribute(key, value)
    }
  }
  for (const child of children) {
    node.appendChild(child)
  }
  return node
}
