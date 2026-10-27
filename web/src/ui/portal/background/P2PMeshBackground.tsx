import { useEffect, useRef } from 'react'
import * as THREE from 'three'

interface NodePoint {
  position: THREE.Vector3
  velocity: THREE.Vector3
  origin: THREE.Vector3
}

interface ActiveEdge {
  from: number
  to: number
  alpha: number
}

interface SignalPulse {
  fromNode: number
  toNode: number
  progress: number
  speed: number
  active: boolean
}

const BOUND_X = 260
const BOUND_Y = 160
const BOUND_Z = 120
const CAMERA_FOV = 60
const CAMERA_Z = 220
const FOG_COLOR = 0x070c09
const FOG_DENSITY = 0.0018

const MOBILE_BREAKPOINT = 768
const DESKTOP_NODE_COUNT = 54
const MOBILE_NODE_COUNT = 32
const DESKTOP_MAX_DISTANCE = 75
const MOBILE_MAX_DISTANCE = 65
const DESKTOP_PULSE_COUNT = 14
const MOBILE_PULSE_COUNT = 7

const NODE_POINT_SIZE = 7.5
const PULSE_POINT_SIZE = 7.2
const PULSE_TRAIL_SEGMENTS = 5
const PULSE_TRAIL_LENGTH = 0.24
const TEXTURE_SIZE = 128

function cryptoRandom(): number {
  const arr = new Uint32Array(1)
  window.crypto.getRandomValues(arr)
  return (arr[0] ?? 0) / 0xffffffff
}

/**
 * Prevents additive primitives from writing 1.0 into the WebGL canvas alpha channel,
 * which would otherwise cause the browser compositor to darken the CSS hero gradient.
 */
function applyAdditiveGlowBlending(material: THREE.Material) {
  material.transparent = true
  material.depthWrite = false
  material.blending = THREE.CustomBlending
  material.blendEquation = THREE.AddEquation
  material.blendSrc = THREE.SrcAlphaFactor
  material.blendDst = THREE.OneFactor
  material.blendEquationAlpha = THREE.AddEquation
  material.blendSrcAlpha = THREE.ZeroFactor
  material.blendDstAlpha = THREE.OneFactor
}

function createCircleTexture(): THREE.CanvasTexture {
  const canvas = document.createElement('canvas')
  canvas.width = TEXTURE_SIZE
  canvas.height = TEXTURE_SIZE
  const ctx = canvas.getContext('2d')
  if (ctx) {
    const half = TEXTURE_SIZE / 2
    const gradient = ctx.createRadialGradient(half, half, 0, half, half, half)
    gradient.addColorStop(0, 'rgba(52, 211, 153, 1)')
    gradient.addColorStop(0.35, 'rgba(16, 185, 129, 0.7)')
    gradient.addColorStop(0.7, 'rgba(6, 182, 212, 0.25)')
    gradient.addColorStop(1, 'rgba(0, 0, 0, 0)')
    ctx.fillStyle = gradient
    ctx.beginPath()
    ctx.arc(half, half, half, 0, Math.PI * 2)
    ctx.fill()
  }
  return new THREE.CanvasTexture(canvas)
}

/**
 * Uses a sky-blue radial bokeh gradient (#e0f2fe -> #38bdf8 -> #0284c7) so pulse orbs
 * keep the blue color distinction while rendering with soft optical diffusion.
 */
function createPulseGlowTexture(): THREE.CanvasTexture {
  const canvas = document.createElement('canvas')
  canvas.width = TEXTURE_SIZE
  canvas.height = TEXTURE_SIZE
  const ctx = canvas.getContext('2d')
  if (ctx) {
    const half = TEXTURE_SIZE / 2
    const gradient = ctx.createRadialGradient(half, half, 0, half, half, half)
    gradient.addColorStop(0, 'rgba(224, 242, 254, 1)')
    gradient.addColorStop(0.35, 'rgba(56, 189, 248, 0.82)')
    gradient.addColorStop(0.7, 'rgba(2, 132, 199, 0.3)')
    gradient.addColorStop(1, 'rgba(0, 0, 0, 0)')
    ctx.fillStyle = gradient
    ctx.beginPath()
    ctx.arc(half, half, half, 0, Math.PI * 2)
    ctx.fill()
  }
  return new THREE.CanvasTexture(canvas)
}

function createNodePoints(count: number): { nodes: NodePoint[]; nodePositions: Float32Array } {
  const nodes: NodePoint[] = []
  const nodePositions = new Float32Array(count * 3)

  for (let i = 0; i < count; i++) {
    const x = (cryptoRandom() - 0.5) * BOUND_X * 2
    const y = (cryptoRandom() - 0.5) * BOUND_Y * 2
    const z = (cryptoRandom() - 0.5) * BOUND_Z * 2
    const origin = new THREE.Vector3(x, y, z)
    nodes.push({
      position: origin.clone(),
      velocity: new THREE.Vector3(
        (cryptoRandom() - 0.5) * 0.25,
        (cryptoRandom() - 0.5) * 0.25,
        (cryptoRandom() - 0.5) * 0.2,
      ),
      origin,
    })
    nodePositions[i * 3] = x
    nodePositions[i * 3 + 1] = y
    nodePositions[i * 3 + 2] = z
  }

  return { nodes, nodePositions }
}

function updateNodePhysics(nodes: NodePoint[], positions: Float32Array, delta: number) {
  for (let i = 0; i < nodes.length; i++) {
    const node = nodes[i]
    if (!node) continue

    node.position.addScaledVector(node.velocity, delta * 30)

    if (Math.abs(node.position.x) > BOUND_X) node.velocity.x *= -1
    if (Math.abs(node.position.y) > BOUND_Y) node.velocity.y *= -1
    if (Math.abs(node.position.z) > BOUND_Z) node.velocity.z *= -1

    positions[i * 3] = node.position.x
    positions[i * 3 + 1] = node.position.y
    positions[i * 3 + 2] = node.position.z
  }
}

function updateMeshLines(
  nodes: NodePoint[],
  maxDist: number,
  linePositions: Float32Array,
  lineColors: Float32Array,
): { lineCount: number; activeEdges: ActiveEdge[] } {
  let lineIndex = 0
  const activeEdges: ActiveEdge[] = []

  for (let i = 0; i < nodes.length; i++) {
    const nodeI = nodes[i]
    if (!nodeI) continue

    for (let j = i + 1; j < nodes.length; j++) {
      const nodeJ = nodes[j]
      if (!nodeJ) continue

      const dist = nodeI.position.distanceTo(nodeJ.position)
      if (dist < maxDist) {
        const alpha = 1 - dist / maxDist
        activeEdges.push({ from: i, to: j, alpha })

        const i6 = lineIndex * 6

        linePositions[i6] = nodeI.position.x
        linePositions[i6 + 1] = nodeI.position.y
        linePositions[i6 + 2] = nodeI.position.z
        linePositions[i6 + 3] = nodeJ.position.x
        linePositions[i6 + 4] = nodeJ.position.y
        linePositions[i6 + 5] = nodeJ.position.z

        lineColors[i6] = 0.06 * alpha
        lineColors[i6 + 1] = 0.72 * alpha
        lineColors[i6 + 2] = 0.56 * alpha
        lineColors[i6 + 3] = 0.02 * alpha
        lineColors[i6 + 4] = 0.71 * alpha
        lineColors[i6 + 5] = 0.83 * alpha

        lineIndex++
      }
    }
  }

  return { lineCount: lineIndex, activeEdges }
}

function edgeKey(a: number, b: number): number {
  return a < b ? a * 1024 + b : b * 1024 + a
}

function collectSpawnCandidates(pulses: SignalPulse[], activeEdges: ActiveEdge[]): ActiveEdge[] {
  const occupied = new Set<number>()
  for (const pulse of pulses) {
    if (pulse.active) occupied.add(edgeKey(pulse.fromNode, pulse.toNode))
  }
  return activeEdges.filter((e) => e.alpha >= 0.2 && !occupied.has(edgeKey(e.from, e.to)))
}

function advanceOrSpawnPulse(
  pulse: SignalPulse,
  spawnCandidates: ActiveEdge[],
  delta: number,
) {
  if (pulse.active) {
    pulse.progress += pulse.speed * (delta * 60)
    if (pulse.progress >= 1) {
      pulse.active = false
    }
  }

  if (pulse.active || spawnCandidates.length === 0) return

  const pickIndex = Math.floor(cryptoRandom() * spawnCandidates.length)
  const chosen = spawnCandidates.splice(pickIndex, 1)[0]
  if (!chosen) return

  const reverse = cryptoRandom() > 0.5
  pulse.fromNode = reverse ? chosen.to : chosen.from
  pulse.toNode = reverse ? chosen.from : chosen.to
  pulse.progress = pulse.progress < 0 ? 0.15 + cryptoRandom() * 0.7 : 0
  pulse.speed = 0.0045 + cryptoRandom() * 0.006
  pulse.active = true
}

function writePulseTrailAndHead(
  fromPos: THREE.Vector3,
  toPos: THREE.Vector3,
  headT: number,
  envelope: number,
  trailPositions: Float32Array,
  trailColors: Float32Array,
  trailOffset: number,
  headPositions: Float32Array,
  headColors: Float32Array,
  headIndex: number,
): number {
  const ax = fromPos.x
  const ay = fromPos.y
  const az = fromPos.z
  const dx = toPos.x - ax
  const dy = toPos.y - ay
  const dz = toPos.z - az
  const tailT = Math.max(0, headT - PULSE_TRAIL_LENGTH)

  for (let s = 0; s < PULSE_TRAIL_SEGMENTS; s++) {
    const frac0 = s / PULSE_TRAIL_SEGMENTS
    const frac1 = (s + 1) / PULSE_TRAIL_SEGMENTS
    const t0 = tailT + (headT - tailT) * frac0
    const t1 = tailT + (headT - tailT) * frac1
    const c0 = frac0 * frac0 * envelope * 0.65
    const c1 = frac1 * frac1 * envelope * 0.65

    const base = (trailOffset + s) * 6
    trailPositions[base] = ax + dx * t0
    trailPositions[base + 1] = ay + dy * t0
    trailPositions[base + 2] = az + dz * t0
    trailPositions[base + 3] = ax + dx * t1
    trailPositions[base + 4] = ay + dy * t1
    trailPositions[base + 5] = az + dz * t1

    trailColors[base] = 0.08 * c0
    trailColors[base + 1] = 0.62 * c0
    trailColors[base + 2] = 0.94 * c0
    trailColors[base + 3] = 0.22 * c1
    trailColors[base + 4] = 0.78 * c1
    trailColors[base + 5] = 0.99 * c1
  }

  const h3 = headIndex * 3
  headPositions[h3] = ax + dx * headT
  headPositions[h3 + 1] = ay + dy * headT
  headPositions[h3 + 2] = az + dz * headT

  const headIntensity = envelope * 0.95
  headColors[h3] = 0.35 * headIntensity
  headColors[h3 + 1] = 0.82 * headIntensity
  headColors[h3 + 2] = 1.0 * headIntensity

  return trailOffset + PULSE_TRAIL_SEGMENTS
}

function updateSignalPulses(
  pulses: SignalPulse[],
  nodes: NodePoint[],
  activeEdges: ActiveEdge[],
  maxDist: number,
  trailPositions: Float32Array,
  trailColors: Float32Array,
  headPositions: Float32Array,
  headColors: Float32Array,
  delta: number,
): { trailSegmentCount: number; headCount: number } {
  const spawnCandidates = collectSpawnCandidates(pulses, activeEdges)
  let trailSegmentCount = 0
  let headCount = 0

  for (const pulse of pulses) {
    advanceOrSpawnPulse(pulse, spawnCandidates, delta)
    if (!pulse.active) continue

    const fromNode = nodes[pulse.fromNode]
    const toNode = nodes[pulse.toNode]
    const dist = fromNode && toNode ? fromNode.position.distanceTo(toNode.position) : maxDist
    if (!fromNode || !toNode || dist >= maxDist) {
      pulse.active = false
      continue
    }

    const edgeAlpha = Math.sqrt(1 - dist / maxDist)
    const envelope = Math.sin(pulse.progress * Math.PI) * edgeAlpha
    if (envelope <= 0.01) continue

    trailSegmentCount = writePulseTrailAndHead(
      fromNode.position,
      toNode.position,
      pulse.progress,
      envelope,
      trailPositions,
      trailColors,
      trailSegmentCount,
      headPositions,
      headColors,
      headCount,
    )
    headCount++
  }

  return { trailSegmentCount, headCount }
}

export function P2PMeshBackground() {
  const containerRef = useRef<HTMLDivElement>(null)

  useEffect(() => {
    const container = containerRef.current
    if (!container) return

    if (window.matchMedia('(prefers-reduced-motion: reduce)').matches) return

    const scene = new THREE.Scene()
    scene.fog = new THREE.FogExp2(FOG_COLOR, FOG_DENSITY)

    const camera = new THREE.PerspectiveCamera(
      CAMERA_FOV,
      container.clientWidth / Math.max(container.clientHeight, 1),
      0.1,
      1000,
    )
    camera.position.z = CAMERA_Z

    let renderer: THREE.WebGLRenderer | null = null
    try {
      renderer = new THREE.WebGLRenderer({
        antialias: true,
        alpha: true,
        powerPreference: 'high-performance',
      })
    } catch {
      return
    }

    renderer.setSize(container.clientWidth, container.clientHeight)
    renderer.setPixelRatio(Math.min(window.devicePixelRatio, 2))
    renderer.setClearColor(0x000000, 0)
    container.appendChild(renderer.domElement)

    const isMobile = window.innerWidth < MOBILE_BREAKPOINT
    const nodeCount = isMobile ? MOBILE_NODE_COUNT : DESKTOP_NODE_COUNT
    const maxDistance = isMobile ? MOBILE_MAX_DISTANCE : DESKTOP_MAX_DISTANCE
    const maxPulses = isMobile ? MOBILE_PULSE_COUNT : DESKTOP_PULSE_COUNT

    const { nodes, nodePositions } = createNodePoints(nodeCount)
    const pointsPosAttr = new THREE.BufferAttribute(nodePositions, 3).setUsage(THREE.DynamicDrawUsage)
    const pointsGeometry = new THREE.BufferGeometry()
    pointsGeometry.setAttribute('position', pointsPosAttr)

    const particleTexture = createCircleTexture()
    const pointsMaterial = new THREE.PointsMaterial({
      size: NODE_POINT_SIZE,
      map: particleTexture,
    })
    applyAdditiveGlowBlending(pointsMaterial)
    scene.add(new THREE.Points(pointsGeometry, pointsMaterial))

    const maxLines = (nodeCount * (nodeCount - 1)) / 2
    const linePositions = new Float32Array(maxLines * 6)
    const lineColors = new Float32Array(maxLines * 6)
    const linePosAttr = new THREE.BufferAttribute(linePositions, 3).setUsage(THREE.DynamicDrawUsage)
    const lineColorAttr = new THREE.BufferAttribute(lineColors, 3).setUsage(THREE.DynamicDrawUsage)
    const lineGeometry = new THREE.BufferGeometry()
    lineGeometry.setAttribute('position', linePosAttr)
    lineGeometry.setAttribute('color', lineColorAttr)

    const lineMaterial = new THREE.LineBasicMaterial({
      vertexColors: true,
      fog: false,
    })
    applyAdditiveGlowBlending(lineMaterial)
    scene.add(new THREE.LineSegments(lineGeometry, lineMaterial))

    const maxTrailSegments = maxPulses * PULSE_TRAIL_SEGMENTS
    const trailPositions = new Float32Array(maxTrailSegments * 6)
    const trailColors = new Float32Array(maxTrailSegments * 6)
    const trailPosAttr = new THREE.BufferAttribute(trailPositions, 3).setUsage(THREE.DynamicDrawUsage)
    const trailColorAttr = new THREE.BufferAttribute(trailColors, 3).setUsage(THREE.DynamicDrawUsage)
    const trailGeometry = new THREE.BufferGeometry()
    trailGeometry.setAttribute('position', trailPosAttr)
    trailGeometry.setAttribute('color', trailColorAttr)
    scene.add(new THREE.LineSegments(trailGeometry, lineMaterial))

    const pulseTexture = createPulseGlowTexture()
    const headPositions = new Float32Array(maxPulses * 3)
    const headColors = new Float32Array(maxPulses * 3)
    const headPosAttr = new THREE.BufferAttribute(headPositions, 3).setUsage(THREE.DynamicDrawUsage)
    const headColorAttr = new THREE.BufferAttribute(headColors, 3).setUsage(THREE.DynamicDrawUsage)
    const headGeometry = new THREE.BufferGeometry()
    headGeometry.setAttribute('position', headPosAttr)
    headGeometry.setAttribute('color', headColorAttr)
    const headMaterial = new THREE.PointsMaterial({
      size: PULSE_POINT_SIZE,
      map: pulseTexture,
      vertexColors: true,
    })
    applyAdditiveGlowBlending(headMaterial)
    scene.add(new THREE.Points(headGeometry, headMaterial))

    const pulses: SignalPulse[] = Array.from({ length: maxPulses }, () => ({
      fromNode: 0,
      toNode: 1,
      progress: -1,
      speed: 0.005,
      active: false,
    }))

    let targetMouseX = 0
    let targetMouseY = 0
    let currentMouseX = 0
    let currentMouseY = 0

    const onPointerMove = (e: PointerEvent) => {
      const rect = container.getBoundingClientRect()
      const x = ((e.clientX - rect.left) / rect.width) * 2 - 1
      const y = -(((e.clientY - rect.top) / rect.height) * 2 - 1)
      targetMouseX = x * 35
      targetMouseY = y * 25
    }

    window.addEventListener('pointermove', onPointerMove, { passive: true })

    const onResize = () => {
      if (!container || !renderer) return
      camera.aspect = container.clientWidth / Math.max(container.clientHeight, 1)
      camera.updateProjectionMatrix()
      renderer.setSize(container.clientWidth, container.clientHeight)
    }

    window.addEventListener('resize', onResize)

    const timer = new THREE.Timer()
    // Hidden time must not advance the animation when the tab becomes visible again.
    timer.connect(document)

    let animationFrameId: number
    const animate = () => {
      animationFrameId = requestAnimationFrame(animate)
      if (document.hidden || !renderer) return

      timer.update()
      const delta = Math.min(timer.getDelta(), 0.1)

      currentMouseX += (targetMouseX - currentMouseX) * 0.04
      currentMouseY += (targetMouseY - currentMouseY) * 0.04
      camera.position.x = currentMouseX
      camera.position.y = currentMouseY
      camera.lookAt(0, 0, 0)

      scene.rotation.y += delta * 0.03
      scene.rotation.x = Math.sin(timer.getElapsed() * 0.2) * 0.05

      updateNodePhysics(nodes, pointsPosAttr.array as Float32Array, delta)
      pointsPosAttr.needsUpdate = true

      const { lineCount, activeEdges } = updateMeshLines(nodes, maxDistance, linePositions, lineColors)
      lineGeometry.setDrawRange(0, lineCount * 2)
      linePosAttr.needsUpdate = true
      lineColorAttr.needsUpdate = true

      const { trailSegmentCount, headCount } = updateSignalPulses(
        pulses,
        nodes,
        activeEdges,
        maxDistance,
        trailPositions,
        trailColors,
        headPositions,
        headColors,
        delta,
      )
      trailGeometry.setDrawRange(0, trailSegmentCount * 2)
      trailPosAttr.needsUpdate = true
      trailColorAttr.needsUpdate = true

      headGeometry.setDrawRange(0, headCount)
      headPosAttr.needsUpdate = true
      headColorAttr.needsUpdate = true

      renderer.render(scene, camera)
    }

    animate()

    return () => {
      cancelAnimationFrame(animationFrameId)
      window.removeEventListener('pointermove', onPointerMove)
      window.removeEventListener('resize', onResize)
      timer.dispose()

      pointsGeometry.dispose()
      pointsMaterial.dispose()
      particleTexture.dispose()
      pulseTexture.dispose()
      lineGeometry.dispose()
      lineMaterial.dispose()
      trailGeometry.dispose()
      headGeometry.dispose()
      headMaterial.dispose()

      if (renderer) {
        renderer.dispose()
        if (renderer.domElement.parentNode) {
          renderer.domElement.parentNode.removeChild(renderer.domElement)
        }
      }
    }
  }, [])

  return <div ref={containerRef} className="portal-3d-bg" aria-hidden="true" />
}
