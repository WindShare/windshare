import { useEffect, useRef } from 'react'
import * as THREE from 'three'

interface NodePoint {
  position: THREE.Vector3
  velocity: THREE.Vector3
  origin: THREE.Vector3
}

interface PulsePacket {
  fromNode: number
  toNode: number
  progress: number
  speed: number
  mesh: THREE.Mesh
}

const BOUND_X = 260
const BOUND_Y = 160
const BOUND_Z = 120

function cryptoRandom(): number {
  const arr = new Uint32Array(1)
  window.crypto.getRandomValues(arr)
  return (arr[0] ?? 0) / 0xffffffff
}

function createCircleTexture(): THREE.CanvasTexture {
  const canvas = document.createElement('canvas')
  canvas.width = 64
  canvas.height = 64
  const ctx = canvas.getContext('2d')
  if (ctx) {
    const gradient = ctx.createRadialGradient(32, 32, 0, 32, 32, 32)
    gradient.addColorStop(0, 'rgba(52, 211, 153, 1)')
    gradient.addColorStop(0.35, 'rgba(16, 185, 129, 0.7)')
    gradient.addColorStop(0.7, 'rgba(6, 182, 212, 0.25)')
    gradient.addColorStop(1, 'rgba(0, 0, 0, 0)')
    ctx.fillStyle = gradient
    ctx.beginPath()
    ctx.arc(32, 32, 32, 0, Math.PI * 2)
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
): { lineCount: number; activePairs: [number, number][] } {
  let lineIndex = 0
  const activePairs: [number, number][] = []

  for (let i = 0; i < nodes.length; i++) {
    const nodeI = nodes[i]
    if (!nodeI) continue

    for (let j = i + 1; j < nodes.length; j++) {
      const nodeJ = nodes[j]
      if (!nodeJ) continue

      const dist = nodeI.position.distanceTo(nodeJ.position)
      if (dist < maxDist) {
        activePairs.push([i, j])

        const alpha = 1 - dist / maxDist
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

  return { lineCount: lineIndex, activePairs }
}

function updatePulsePackets(
  pulses: PulsePacket[],
  nodes: NodePoint[],
  activePairs: [number, number][],
  delta: number,
) {
  if (activePairs.length === 0) return

  for (let p = 0; p < pulses.length; p++) {
    const pulse = pulses[p]
    if (!pulse) continue

    pulse.progress += pulse.speed * (delta * 60)

    if (pulse.progress >= 1 || pulse.mesh.visible === false) {
      const pair = activePairs[Math.floor(cryptoRandom() * activePairs.length)]
      if (pair && pair[0] !== undefined && pair[1] !== undefined) {
        pulse.fromNode = pair[0]
        pulse.toNode = pair[1]
        pulse.progress = 0
        pulse.mesh.visible = true
      }
    }

    const fromNode = nodes[pulse.fromNode]
    const toNode = nodes[pulse.toNode]
    if (fromNode && toNode) {
      pulse.mesh.position.lerpVectors(fromNode.position, toNode.position, pulse.progress)
    }
  }
}

export function P2PMeshBackground() {
  const containerRef = useRef<HTMLDivElement>(null)

  useEffect(() => {
    const container = containerRef.current
    if (!container) return

    if (window.matchMedia('(prefers-reduced-motion: reduce)').matches) return

    const scene = new THREE.Scene()
    scene.fog = new THREE.FogExp2(0x070c09, 0.0018)

    const camera = new THREE.PerspectiveCamera(
      60,
      container.clientWidth / Math.max(container.clientHeight, 1),
      0.1,
      1000,
    )
    camera.position.z = 220

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

    const nodeCount = window.innerWidth < 768 ? 32 : 54
    const maxDistance = window.innerWidth < 768 ? 65 : 75
    const { nodes, nodePositions } = createNodePoints(nodeCount)

    const pointsPosAttr = new THREE.BufferAttribute(nodePositions, 3).setUsage(THREE.DynamicDrawUsage)
    const pointsGeometry = new THREE.BufferGeometry()
    pointsGeometry.setAttribute('position', pointsPosAttr)

    const particleTexture = createCircleTexture()
    const pointsMaterial = new THREE.PointsMaterial({
      size: 7.5,
      map: particleTexture,
      transparent: true,
      blending: THREE.AdditiveBlending,
      depthWrite: false,
    })

    scene.add(new THREE.Points(pointsGeometry, pointsMaterial))

    const maxLines = (nodeCount * (nodeCount - 1)) / 2
    const linePositions = new Float32Array(maxLines * 6)
    const lineColors = new Float32Array(maxLines * 6)
    const linePosAttr = new THREE.BufferAttribute(linePositions, 3).setUsage(THREE.DynamicDrawUsage)
    const lineColorAttr = new THREE.BufferAttribute(lineColors, 3).setUsage(THREE.DynamicDrawUsage)

    const lineGeometry = new THREE.BufferGeometry()
    lineGeometry.setAttribute('position', linePosAttr)
    lineGeometry.setAttribute('color', lineColorAttr)

    const lineSegments = new THREE.LineSegments(
      lineGeometry,
      new THREE.LineBasicMaterial({
        vertexColors: true,
        transparent: true,
        blending: THREE.AdditiveBlending,
        depthWrite: false,
      }),
    )
    scene.add(lineSegments)

    const pulseGeometry = new THREE.SphereGeometry(1.2, 8, 8)
    const pulseMaterial = new THREE.MeshBasicMaterial({
      color: 0x38bdf8,
      transparent: true,
      opacity: 0.85,
      blending: THREE.AdditiveBlending,
    })

    const pulses: PulsePacket[] = []
    const maxPulses = window.innerWidth < 768 ? 8 : 16

    for (let i = 0; i < maxPulses; i++) {
      const mesh = new THREE.Mesh(pulseGeometry, pulseMaterial)
      mesh.visible = false
      scene.add(mesh)
      pulses.push({
        fromNode: 0,
        toNode: 1,
        progress: cryptoRandom(),
        speed: 0.004 + cryptoRandom() * 0.007,
        mesh,
      })
    }

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

    let isVisible = !document.hidden
    const onVisibilityChange = () => {
      isVisible = !document.hidden
    }
    document.addEventListener('visibilitychange', onVisibilityChange)

    let animationFrameId: number
    const clock = new THREE.Clock()

    const animate = () => {
      animationFrameId = requestAnimationFrame(animate)
      if (!isVisible || !renderer) return

      const delta = Math.min(clock.getDelta(), 0.1)

      currentMouseX += (targetMouseX - currentMouseX) * 0.04
      currentMouseY += (targetMouseY - currentMouseY) * 0.04
      camera.position.x = currentMouseX
      camera.position.y = currentMouseY
      camera.lookAt(0, 0, 0)

      scene.rotation.y += delta * 0.03
      scene.rotation.x = Math.sin(clock.getElapsedTime() * 0.2) * 0.05

      updateNodePhysics(nodes, pointsPosAttr.array as Float32Array, delta)
      pointsPosAttr.needsUpdate = true

      const { lineCount, activePairs } = updateMeshLines(nodes, maxDistance, linePositions, lineColors)
      lineGeometry.setDrawRange(0, lineCount * 2)
      linePosAttr.needsUpdate = true
      lineColorAttr.needsUpdate = true

      updatePulsePackets(pulses, nodes, activePairs, delta)

      renderer.render(scene, camera)
    }

    animate()

    return () => {
      cancelAnimationFrame(animationFrameId)
      window.removeEventListener('pointermove', onPointerMove)
      window.removeEventListener('resize', onResize)
      document.removeEventListener('visibilitychange', onVisibilityChange)

      pointsGeometry.dispose()
      pointsMaterial.dispose()
      particleTexture.dispose()
      lineGeometry.dispose()
      lineSegments.geometry.dispose()
      pulseGeometry.dispose()
      pulseMaterial.dispose()

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
