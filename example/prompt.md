# BUILD: `galaxy.html` — Procedural Space Game (30B LLM edition)

You are a senior Three.js engineer. Produce **one self-contained HTML file** named `galaxy.html`. Output **ONLY the file content**, starting with `<!DOCTYPE html>` and ending with `</html>`. No prose, no markdown fences, no commentary.

This v4 is tuned for a local 30B model: **every difficult shader and every tricky formula is provided verbatim below. Copy them exactly.** Your job is to assemble these blocks plus the connecting JS into one working file. Do not improvise math.

---

## 0. NON-NEGOTIABLE RULES

**R1. NO UNDEFINED IDENTIFIERS.** Every name you call must be defined above in the same file. Helpers like `lerp`, `clamp`, `smoothstep`, `posNoise`, `mulberry32`, `makeRandomBasis`, `randRange`, `randPick` are pre-defined in §2 — use those, do not redefine.

**R2. IMPORTS — EXACTLY THESE FOUR, NO OTHERS.**
```js
import * as THREE from 'three';
import { EffectComposer } from 'three/addons/postprocessing/EffectComposer.js';
import { RenderPass } from 'three/addons/postprocessing/RenderPass.js';
import { UnrealBloomPass } from 'three/addons/postprocessing/UnrealBloomPass.js';
import { ShaderPass } from 'three/addons/postprocessing/ShaderPass.js';
```

**R3. NO `new` IN HOT PATHS.** Inside `animate()` or anything it calls every frame, do not allocate `new Vector3 / Color / Quaternion / Matrix4`. Use the scratch vars `_v1 _v2 _v3 _q1 _m1 _c1 _c2` from §2.

**R4. SHADER BLOCKS ARE LAW.** Where a GLSL shader is given verbatim in this file, paste it character-for-character (template strings). Do NOT rewrite, "simplify", or "fix" them. The `#include <common>` / `#include <logdepthbuf_*>` lines are mandatory because `logarithmicDepthBuffer: true` is enabled.

**R5. NUMBERS ARE LAW.** Where a numeric constant is given (orbit radius, star count, opacity, etc.), use exactly that number. The look only works at these scales.

**R6. NEVER CREATE OTHER FILES.** Only `galaxy.html`. No README, no separate JS, no CSS.

---

## 1. HTML SKELETON — COPY VERBATIM

```html
<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Galaxy</title>
<style>
  html, body { margin:0; padding:0; overflow:hidden; background:#000; height:100%; }
  canvas { display:block; }
  #hud {
    position:fixed; top:10px; left:12px;
    font-family:'Courier New', monospace;
    color:rgba(34,255,68,0.78); font-size:11px; line-height:1.35;
    letter-spacing:0.5px;
    text-shadow:0 0 4px rgba(34,255,68,0.45);
    pointer-events:none; user-select:none;
  }
  #hud .dim { opacity:0.45; }
  #hud .v { color:#aaffbb; }
  #crosshair {
    position:fixed; top:50%; left:50%; transform:translate(-50%,-50%);
    color:#22ff44; font-family:monospace; font-size:20px;
    text-shadow:0 0 6px #22ff44; pointer-events:none;
  }
  #click-to-start {
    position:fixed; bottom:80px; left:50%; transform:translateX(-50%);
    color:#22ff44; font-family:monospace; font-size:16px; letter-spacing:2px;
    text-shadow:0 0 6px #22ff44;
    background:rgba(0,0,0,0.55); padding:12px 22px;
    border:1px solid rgba(34,255,68,0.55);
    cursor:pointer; z-index:10;
  }
  #title-overlay {
    position:fixed; top:48%; left:50%; transform:translate(-50%, -50%);
    text-align:center; color:#22ff44; font-family:monospace;
    pointer-events:none; user-select:none; z-index:9;
    text-shadow:0 0 10px rgba(34,255,68,0.6);
  }
  #title-overlay .big  { font-size:48px; letter-spacing:14px; margin:0; }
  #title-overlay .sub  { font-size:14px; letter-spacing:6px; margin:8px 0 0; opacity:0.75; }
</style>
<script type="importmap">
{ "imports": {
  "three": "https://unpkg.com/three@0.160.0/build/three.module.js",
  "three/addons/": "https://unpkg.com/three@0.160.0/examples/jsm/"
}}
</script>
</head>
<body>
<div id="hud"></div>
<div id="crosshair">+</div>
<div id="title-overlay">
  <h1 class="big">GALAXY</h1>
  <p class="sub">A PROCEDURAL UNIVERSE</p>
</div>
<div id="click-to-start">► CLICK TO START</div>
<script type="module">
/* all code goes here, in the order of the sections below */
</script>
</body>
</html>
```

---

## 2. HELPERS — COPY VERBATIM TO TOP OF SCRIPT

```js
const TAU = Math.PI * 2;
const lerp = (a, b, t) => a + (b - a) * t;
const clamp = (v, a, b) => v < a ? a : (v > b ? b : v);
const randRange = (r, a, b) => a + r() * (b - a);
const randPick = (r, arr) => arr[Math.floor(r() * arr.length)];

function smoothstep(edge0, edge1, x) {
  const t = clamp((x - edge0) / (edge1 - edge0), 0, 1);
  return t * t * (3 - 2 * t);
}

function posNoise(x, y, z) {
  const s = Math.sin(x*12.9898 + y*78.233 + z*37.719) * 43758.5453;
  return s - Math.floor(s);
}

function mulberry32(seed) {
  return function() {
    seed |= 0; seed = seed + 0x6D2B79F5 | 0;
    let t = Math.imul(seed ^ seed >>> 15, 1 | seed);
    t = t + Math.imul(t ^ t >>> 7, 61 | t) ^ t;
    return ((t ^ t >>> 14) >>> 0) / 4294967296;
  };
}

// Random orthonormal basis: n is a uniform unit vector, u/v span the plane perpendicular to it.
function makeRandomBasis(rng) {
  const cosT = rng() * 2 - 1;
  const sinT = Math.sqrt(1 - cosT * cosT);
  const phi = rng() * TAU;
  const n = new THREE.Vector3(sinT * Math.cos(phi), cosT, sinT * Math.sin(phi));
  const ref = Math.abs(n.y) < 0.9 ? new THREE.Vector3(0, 1, 0) : new THREE.Vector3(1, 0, 0);
  const u = new THREE.Vector3().crossVectors(n, ref).normalize();
  const v = new THREE.Vector3().crossVectors(n, u).normalize();
  return { u, v, n };
}

// Scratch — reuse, never allocate in render loop.
const _v1 = new THREE.Vector3();
const _v2 = new THREE.Vector3();
const _v3 = new THREE.Vector3();
const _q1 = new THREE.Quaternion();
const _qTmp = new THREE.Quaternion();
const _m1 = new THREE.Matrix4();
const _c1 = new THREE.Color();
const _c2 = new THREE.Color();
const AXIS_X = new THREE.Vector3(1, 0, 0);
const AXIS_Y = new THREE.Vector3(0, 1, 0);
const AXIS_Z = new THREE.Vector3(0, 0, 1);
```

---

## 3. RENDERER + SCENE + CAMERA

```js
const renderer = new THREE.WebGLRenderer({
  antialias: true,
  logarithmicDepthBuffer: true,
  powerPreference: 'high-performance'
});
renderer.setPixelRatio(Math.min(devicePixelRatio, 2));
renderer.setSize(innerWidth, innerHeight);
renderer.setClearColor(0x000000);
renderer.toneMapping = THREE.ACESFilmicToneMapping;
renderer.toneMappingExposure = 0.28;
renderer.outputColorSpace = THREE.SRGBColorSpace;
document.body.appendChild(renderer.domElement);

const scene = new THREE.Scene();
scene.fog = null;
const camera = new THREE.PerspectiveCamera(70, innerWidth/innerHeight, 0.1, 400000);
scene.add(camera);

const camQuat = new THREE.Quaternion().copy(camera.quaternion);
let yawDelta = 0, pitchDelta = 0;
```

---

## 4. LIGHTING

```js
scene.add(new THREE.AmbientLight(0x141a2b, 0.10));
const starLight = new THREE.DirectionalLight(0xffffff, 3.5);
starLight.position.set(0, 0, 0);
scene.add(starLight);
scene.add(starLight.target);
```

Later, in `updateNearStars()`, you'll update `starLight.position` to the nearest star and `starLight.color` to its color, and aim `starLight.target` at the camera position.

---

## 5. STAR FIELD — INFINITE CHUNK STREAMING

This is the core of the universe. The space is divided into 20000-unit cubic chunks. A 21×21×21 box around the camera (9261 chunks) is always loaded. Each chunk's RNG is seeded by its (cx,cy,cz). Every 4×4×4 box of chunks forms a "region" with a theme that biases star count and size.

```js
const CHUNK_SIZE = 20000;
const CHUNK_RADIUS = 10;
const STARS_PER_CHUNK = 2;
const MAX_ACTIVE_STARS = (CHUNK_RADIUS*2 + 1) ** 3 * STARS_PER_CHUNK;
const STAR_VISIBLE_FAR  = 380000;
const STAR_VISIBLE_NEAR = 800;

const chunkCache = new Map();
const allActiveStars = [];
let currentChunkKey = null;

function chunkKey(cx, cy, cz) { return cx + ',' + cy + ',' + cz; }
function chunkHash(cx, cy, cz) {
  let h = (cx * 73856093) ^ (cy * 19349663) ^ (cz * 83492791);
  return mulberry32(h | 0);
}

const REGION_SIZE = 4;
function regionTheme(cx, cy, cz) {
  const rx = Math.floor(cx / REGION_SIZE);
  const ry = Math.floor(cy / REGION_SIZE);
  const rz = Math.floor(cz / REGION_SIZE);
  let h = (rx * 134775813) ^ (ry * 49979693) ^ (rz * 191) ^ 0xCAFEBABE;
  const t = mulberry32(h | 0)();
  if (t < 0.30)      return 'void';
  else if (t < 0.50) return 'cluster';
  else if (t < 0.80) return 'normal';
  else               return 'giants';
}

function getOrCreateChunk(cx, cy, cz) {
  const key = chunkKey(cx, cy, cz);
  let chunk = chunkCache.get(key);
  if (chunk) return chunk;
  chunk = [];
  const r = chunkHash(cx, cy, cz);
  const theme = regionTheme(cx, cy, cz);
  let count, sizeMin, sizeMax, biasGiants;
  switch (theme) {
    case 'void':    count = 0;               sizeMin = 80;  sizeMax = 450; biasGiants = 0.0; break;
    case 'cluster': count = STARS_PER_CHUNK; sizeMin = 30;  sizeMax = 200; biasGiants = 0.0; break;
    case 'giants':  count = 1;               sizeMin = 200; sizeMax = 700; biasGiants = 1.0; break;
    default:        count = 1;               sizeMin = 50;  sizeMax = 400; biasGiants = 0.0; break;
  }
  if (cx === 0 && cy === 0 && cz === 0) count = Math.max(count, 1);
  const isCluster = theme === 'cluster';
  const ax = isCluster ? r() : 0;
  const ay = isCluster ? r() : 0;
  const az = isCluster ? r() : 0;
  for (let i = 0; i < count; i++) {
    let fx, fy, fz;
    if (isCluster) {
      fx = ax + (r()-0.5) * 0.25;
      fy = ay + (r()-0.5) * 0.25;
      fz = az + (r()-0.5) * 0.25;
    } else { fx = r(); fy = r(); fz = r(); }
    const x = (cx + fx) * CHUNK_SIZE;
    const y = (cy + fy) * CHUNK_SIZE;
    const z = (cz + fz) * CHUNK_SIZE;
    const radius = randRange(r, sizeMin, sizeMax);
    let t = r();
    if (biasGiants > 0) t = t * (1 - biasGiants) + 0.95 * biasGiants;
    let baseCol, klass;
    if (cx === 0 && cy === 0 && cz === 0 && i === 0) {
      baseCol = new THREE.Color(0xfff2c0); klass = 'G';
    } else if (t < 0.55) { baseCol = new THREE.Color(0xff9966); klass = 'M'; }
    else if (t < 0.78) { baseCol = new THREE.Color(0xffcc66); klass = 'K'; }
    else if (t < 0.90) { baseCol = new THREE.Color(0xfff2c0); klass = 'G'; }
    else if (t < 0.97) { baseCol = new THREE.Color(0xffffff); klass = 'F'; }
    else { baseCol = new THREE.Color(0xaaccff); klass = 'A'; }
    baseCol.multiplyScalar(0.6 + r() * 0.6);
    chunk.push({ pos: new THREE.Vector3(x, y, z), color: baseCol, radius, klass,
                 key: key + ':' + i, system: null });
  }
  chunkCache.set(key, chunk);
  return chunk;
}

const homeChunk = getOrCreateChunk(0, 0, 0);
const home = homeChunk[0];

camera.position.set(home.pos.x + home.radius + 1400, home.pos.y + 250, home.pos.z + 900);
camera.lookAt(home.pos.x - 100, home.pos.y + 100, home.pos.z - 300);
camQuat.copy(camera.quaternion);
```

### 5.1 Far-star Points (pixel renderer)

```js
const starGeometry = new THREE.BufferGeometry();
const starPositions = new Float32Array(MAX_ACTIVE_STARS * 3);
const starColors    = new Float32Array(MAX_ACTIVE_STARS * 3);
const starSizes     = new Float32Array(MAX_ACTIVE_STARS);
starGeometry.setAttribute('position',  new THREE.BufferAttribute(starPositions, 3));
starGeometry.setAttribute('color',     new THREE.BufferAttribute(starColors, 3));
starGeometry.setAttribute('starSize',  new THREE.BufferAttribute(starSizes, 1));
starGeometry.setDrawRange(0, 0);

const starMat = new THREE.ShaderMaterial({
  uniforms: {
    uFadeFar:  { value: STAR_VISIBLE_FAR },
    uFadeNear: { value: STAR_VISIBLE_NEAR },
    uTime:     { value: 0 }
  },
  vertexShader: `
    #include <common>
    #include <logdepthbuf_pars_vertex>
    uniform float uFadeFar;
    uniform float uFadeNear;
    uniform float uTime;
    attribute float starSize;
    attribute vec3 color;
    varying vec3 vColor;
    varying float vFade;
    varying float vSize;
    varying float vTwinkle;
    void main() {
      vColor = color;
      vec4 mv = modelViewMatrix * vec4(position, 1.0);
      gl_Position = projectionMatrix * mv;
      #include <logdepthbuf_vertex>
      float dist = length(mv.xyz);
      float projPx = starSize * 770.0 / max(1.0, dist);
      float ps = clamp(projPx, 1.4, 160.0);
      gl_PointSize = ps;
      vSize = ps;
      vFade = smoothstep(uFadeFar, uFadeNear, dist);
      float seed = fract(sin(dot(position.xyz, vec3(12.9898, 78.233, 37.719))) * 43758.5453);
      float fast = sin(uTime * 2.7 + seed * 6.2831) * 0.5 + 0.5;
      float slow = sin(uTime * 0.6 + seed * 12.566) * 0.5 + 0.5;
      vTwinkle = 0.78 + 0.22 * mix(fast, slow, 0.4);
    }
  `,
  fragmentShader: `
    #include <common>
    #include <logdepthbuf_pars_fragment>
    varying vec3 vColor;
    varying float vFade;
    varying float vSize;
    varying float vTwinkle;
    void main() {
      vec2 uv = (gl_PointCoord - 0.5) * 2.0;
      float d = length(uv);
      if (d > 1.0) discard;
      float haloAmt = smoothstep(3.0, 9.0, vSize);
      float core = exp(-d * d * 32.0);
      float midGlow = exp(-d * d * 6.0) * 0.35;
      float halo = exp(-d * d * 1.2) * 0.18 * haloAmt;
      float spikeAmt = smoothstep(5.0, 14.0, vSize);
      float sx = exp(-uv.x*uv.x * 220.0) * exp(-uv.y*uv.y * 1.8);
      float sy = exp(-uv.y*uv.y * 220.0) * exp(-uv.x*uv.x * 1.8);
      float spikes = (sx + sy) * spikeAmt * 0.55;
      float smallBoost = (1.0 - haloAmt) * 0.9;
      float bodyI = core * (1.7 + smallBoost) + midGlow * (0.7 + smallBoost * 0.4)
                  + halo * 0.55 + spikes * 1.3;
      float alphaI = core + midGlow * 0.4 + halo + spikes;
      gl_FragColor = vec4(vColor * bodyI * vTwinkle, alphaI * vFade * vTwinkle);
      #include <logdepthbuf_fragment>
    }
  `,
  transparent: true,
  depthWrite: false,
  blending: THREE.AdditiveBlending
});
const starPoints = new THREE.Points(starGeometry, starMat);
starPoints.frustumCulled = false;
scene.add(starPoints);
```

### 5.2 Instanced 3D Star Spheres (real meshes per star)

Every active star also exists as a real `SphereGeometry(1, 16, 12)` instance. The shader fades it in based on projected pixel size — distant stars are below 3px and discarded, near stars become real spheres. Use BOTH the pixel renderer and the instanced mesh simultaneously — they overlap and naturally hand off as the camera approaches.

```js
const STAR_INST_GEOM = new THREE.SphereGeometry(1, 16, 12);
const starInstColors = new Float32Array(MAX_ACTIVE_STARS * 3);
STAR_INST_GEOM.setAttribute('instColor', new THREE.InstancedBufferAttribute(starInstColors, 3));

const starInstMat = new THREE.ShaderMaterial({
  uniforms: { uTime: { value: 0 } },
  vertexShader: `
    #include <common>
    #include <logdepthbuf_pars_vertex>
    attribute vec3 instColor;
    varying vec3 vColor;
    varying vec3 vNormalE;
    varying vec3 vViewE;
    varying vec3 vLocal;
    varying float vFade;
    varying float vSeed;
    void main() {
      vColor = instColor;
      vLocal = position;
      mat4 mvi = modelViewMatrix * instanceMatrix;
      vec4 mv = mvi * vec4(position, 1.0);
      vNormalE = normalize(mat3(mvi) * normal);
      vViewE = normalize(-mv.xyz);
      float radius = length(vec3(instanceMatrix[0]));
      vec4 centerMV = mvi * vec4(0.0, 0.0, 0.0, 1.0);
      float distC = length(centerMV.xyz);
      float projPx = radius * 770.0 / max(1.0, distC);
      vFade = smoothstep(1.6, 4.0, projPx);
      vSeed = fract(sin(instanceMatrix[3].x * 0.0123 + instanceMatrix[3].z * 0.0456) * 43758.5453);
      gl_Position = projectionMatrix * mv;
      #include <logdepthbuf_vertex>
    }
  `,
  fragmentShader: `
    #include <common>
    #include <logdepthbuf_pars_fragment>
    uniform float uTime;
    varying vec3 vColor;
    varying vec3 vNormalE;
    varying vec3 vViewE;
    varying vec3 vLocal;
    varying float vFade;
    varying float vSeed;
    float hash(vec3 p) { return fract(sin(dot(p, vec3(12.9898,78.233,37.719))) * 43758.5453); }
    float noise(vec3 p) {
      vec3 i = floor(p); vec3 f = fract(p); f = f*f*(3.0-2.0*f);
      float n000=hash(i+vec3(0,0,0)); float n100=hash(i+vec3(1,0,0));
      float n010=hash(i+vec3(0,1,0)); float n110=hash(i+vec3(1,1,0));
      float n001=hash(i+vec3(0,0,1)); float n101=hash(i+vec3(1,0,1));
      float n011=hash(i+vec3(0,1,1)); float n111=hash(i+vec3(1,1,1));
      return mix(mix(mix(n000,n100,f.x),mix(n010,n110,f.x),f.y),
                 mix(mix(n001,n101,f.x),mix(n011,n111,f.x),f.y), f.z);
    }
    void main() {
      if (vFade < 0.5) discard;
      float dotNV = abs(dot(vNormalE, vViewE));
      vec3 n = normalize(vLocal);
      float t = uTime * 0.05 + vSeed * 30.0;
      float gran = noise(n * 8.0 + vec3(t,0.0,0.0)) * 0.55
                 + noise(n * 22.0 + vec3(0.0, t*1.7, 0.0)) * 0.35
                 + noise(n * 60.0) * 0.20;
      gran = clamp(gran, 0.0, 1.0);
      float limb = pow(dotNV, 0.55);
      float bright = mix(0.55, 1.0, limb) * (0.78 + gran * 0.42);
      float hot = smoothstep(0.65, 0.95, gran) * limb * 0.65;
      vec3 surf = vColor * bright + vColor * hot;
      surf = surf / (1.0 + max(max(surf.r, surf.g), surf.b) * 0.35);
      gl_FragColor = vec4(surf, 1.0);
      #include <logdepthbuf_fragment>
    }
  `,
  side: THREE.FrontSide,
  blending: THREE.NormalBlending
});
const starInstMesh = new THREE.InstancedMesh(STAR_INST_GEOM, starInstMat, MAX_ACTIVE_STARS);
starInstMesh.frustumCulled = false;
starInstMesh.count = 0;
starInstMesh.userData.kind = 'star';
scene.add(starInstMesh);

const instanceIndexToStar = new Array(MAX_ACTIVE_STARS);

function rebuildStarPoints() {
  let n = 0;
  for (const s of allActiveStars) {
    if (s.dead) continue;
    starPositions[n*3]=s.pos.x; starPositions[n*3+1]=s.pos.y; starPositions[n*3+2]=s.pos.z;
    starColors[n*3]=s.color.r; starColors[n*3+1]=s.color.g; starColors[n*3+2]=s.color.b;
    starSizes[n] = s.radius;
    n++;
  }
  starGeometry.setDrawRange(0, n);
  starGeometry.attributes.position.needsUpdate = true;
  starGeometry.attributes.color.needsUpdate = true;
  starGeometry.attributes.starSize.needsUpdate = true;
}

function rebuildInstancedStars() {
  let n = 0;
  for (const s of allActiveStars) {
    if (s.dead) { s.instanceIndex = -1; continue; }
    _v1.copy(s.pos); _q1.identity(); _v2.set(s.radius, s.radius, s.radius);
    _m1.compose(_v1, _q1, _v2);
    starInstMesh.setMatrixAt(n, _m1);
    starInstColors[n*3]=s.color.r; starInstColors[n*3+1]=s.color.g; starInstColors[n*3+2]=s.color.b;
    s.instanceIndex = n;
    instanceIndexToStar[n] = s;
    n++;
  }
  starInstMesh.count = n;
  starInstMesh.instanceMatrix.needsUpdate = true;
  STAR_INST_GEOM.attributes.instColor.needsUpdate = true;
}

function rebuildActiveStars(force) {
  const cx = Math.floor(camera.position.x / CHUNK_SIZE);
  const cy = Math.floor(camera.position.y / CHUNK_SIZE);
  const cz = Math.floor(camera.position.z / CHUNK_SIZE);
  const newKey = cx + ',' + cy + ',' + cz;
  if (!force && newKey === currentChunkKey) return;
  currentChunkKey = newKey;
  allActiveStars.length = 0;
  for (let dx = -CHUNK_RADIUS; dx <= CHUNK_RADIUS; dx++)
  for (let dy = -CHUNK_RADIUS; dy <= CHUNK_RADIUS; dy++)
  for (let dz = -CHUNK_RADIUS; dz <= CHUNK_RADIUS; dz++) {
    const ch = getOrCreateChunk(cx + dx, cy + dy, cz + dz);
    for (const s of ch) if (!s.dead) allActiveStars.push(s);
  }
  rebuildStarPoints();
  rebuildInstancedStars();
}
rebuildActiveStars(true);
```

---

## 6. INFINITE BACKGROUND (camera-locked group)

A second group of stars + a skydome that follows the camera position every frame. From the camera's perspective these points never translate — they read as infinite-distance background.

```js
const bgStarsGroup = new THREE.Group();
scene.add(bgStarsGroup);
{
  const BG_COUNT = 7500;
  const bgRng = mulberry32(7777);
  const bgGeom = new THREE.BufferGeometry();
  const bgPos = new Float32Array(BG_COUNT * 3);
  const bgCol = new Float32Array(BG_COUNT * 3);
  const bgSize = new Float32Array(BG_COUNT);
  for (let i = 0; i < BG_COUNT; i++) {
    const u = bgRng() * 2 - 1;
    const theta = bgRng() * TAU;
    const sr = Math.sqrt(1 - u*u);
    const dist = 140000 + bgRng() * 80000;
    bgPos[i*3]   = sr * Math.cos(theta) * dist;
    bgPos[i*3+1] = u * dist;
    bgPos[i*3+2] = sr * Math.sin(theta) * dist;
    const t = bgRng();
    const c = new THREE.Color();
    if      (t < 0.55) c.setHex(0xff9966);
    else if (t < 0.78) c.setHex(0xffcc66);
    else if (t < 0.90) c.setHex(0xfff2c0);
    else if (t < 0.97) c.setHex(0xffffff);
    else               c.setHex(0xaaccff);
    c.multiplyScalar(0.35 + bgRng() * 0.65);
    bgCol[i*3]=c.r; bgCol[i*3+1]=c.g; bgCol[i*3+2]=c.b;
    bgSize[i] = 80 + bgRng() * 150;
  }
  bgGeom.setAttribute('position', new THREE.BufferAttribute(bgPos, 3));
  bgGeom.setAttribute('color',    new THREE.BufferAttribute(bgCol, 3));
  bgGeom.setAttribute('starSize', new THREE.BufferAttribute(bgSize, 1));
  const bgMat = new THREE.ShaderMaterial({
    vertexShader: `
      #include <common>
      #include <logdepthbuf_pars_vertex>
      attribute float starSize;
      attribute vec3 color;
      varying vec3 vColor;
      void main() {
        vColor = color;
        vec4 mv = modelViewMatrix * vec4(position, 1.0);
        gl_Position = projectionMatrix * mv;
        #include <logdepthbuf_vertex>
        float dist = length(mv.xyz);
        gl_PointSize = clamp(starSize * 770.0 / max(1.0, dist), 1.4, 2.8);
      }
    `,
    fragmentShader: `
      #include <common>
      #include <logdepthbuf_pars_fragment>
      varying vec3 vColor;
      void main() {
        vec2 uv = gl_PointCoord - 0.5;
        float d = length(uv);
        float a = smoothstep(0.5, 0.18, d);
        gl_FragColor = vec4(vColor * 1.15, a);
        #include <logdepthbuf_fragment>
      }
    `,
    transparent: true, depthWrite: false, blending: THREE.AdditiveBlending
  });
  bgStarsGroup.add(new THREE.Points(bgGeom, bgMat));
}
```

### 6.1 Anchor "headline" stars (28 large, with diffraction spikes)

```js
{
  const ANCHOR_COUNT = 28;
  const aRng = mulberry32(31337);
  const aGeom = new THREE.BufferGeometry();
  const aPos = new Float32Array(ANCHOR_COUNT * 3);
  const aCol = new Float32Array(ANCHOR_COUNT * 3);
  const aSize = new Float32Array(ANCHOR_COUNT);
  for (let i = 0; i < ANCHOR_COUNT; i++) {
    const u = aRng() * 2 - 1;
    const theta = aRng() * TAU;
    const sr = Math.sqrt(1 - u*u);
    const dist = 150000;
    aPos[i*3]   = sr * Math.cos(theta) * dist;
    aPos[i*3+1] = u * dist * 0.65;
    aPos[i*3+2] = sr * Math.sin(theta) * dist;
    const t = aRng();
    let c;
    if      (t < 0.30) c = new THREE.Color(0xff7733);
    else if (t < 0.55) c = new THREE.Color(0xffaa55);
    else if (t < 0.78) c = new THREE.Color(0xfff0c0);
    else if (t < 0.93) c = new THREE.Color(0xeeeeff);
    else               c = new THREE.Color(0x99bbff);
    c.multiplyScalar(0.85 + aRng() * 0.5);
    aCol[i*3]=c.r; aCol[i*3+1]=c.g; aCol[i*3+2]=c.b;
    const sr2 = aRng();
    if (sr2 < 0.6)      aSize[i] = 8 + aRng() * 8;
    else if (sr2 < 0.9) aSize[i] = 18 + aRng() * 10;
    else                aSize[i] = 30 + aRng() * 18;
  }
  aGeom.setAttribute('position', new THREE.BufferAttribute(aPos, 3));
  aGeom.setAttribute('color',    new THREE.BufferAttribute(aCol, 3));
  aGeom.setAttribute('starSize', new THREE.BufferAttribute(aSize, 1));
  const aMat = new THREE.ShaderMaterial({
    uniforms: { uTime: { value: 0 } },
    vertexShader: `
      #include <common>
      #include <logdepthbuf_pars_vertex>
      attribute float starSize;
      attribute vec3 color;
      uniform float uTime;
      varying vec3 vColor;
      varying float vSize;
      varying float vTwinkle;
      void main() {
        vColor = color;
        vec4 mv = modelViewMatrix * vec4(position, 1.0);
        gl_Position = projectionMatrix * mv;
        #include <logdepthbuf_vertex>
        gl_PointSize = starSize;
        vSize = starSize;
        float seed = fract(sin(dot(position.xyz, vec3(11.1,71.7,21.3))) * 21345.6789);
        vTwinkle = 0.78 + 0.22 * (sin(uTime * 1.4 + seed * 6.2831) * 0.5 + 0.5);
      }
    `,
    fragmentShader: `
      #include <common>
      #include <logdepthbuf_pars_fragment>
      varying vec3 vColor;
      varying float vSize;
      varying float vTwinkle;
      void main() {
        vec2 uv = (gl_PointCoord - 0.5) * 2.0;
        float d = length(uv);
        if (d > 1.0) discard;
        float spikeAmt = smoothstep(14.0, 30.0, vSize);
        float core = exp(-d * d * 32.0);
        float halo = exp(-d * d * 1.7) * 0.40;
        float sx = exp(-uv.x*uv.x * 140.0) * exp(-uv.y*uv.y * 1.8);
        float sy = exp(-uv.y*uv.y * 140.0) * exp(-uv.x*uv.x * 1.8);
        vec2 rot = vec2(uv.x + uv.y, uv.y - uv.x) * 0.7071;
        float sd1 = exp(-rot.x*rot.x * 280.0) * exp(-rot.y*rot.y * 3.0);
        float sd2 = exp(-rot.y*rot.y * 280.0) * exp(-rot.x*rot.x * 3.0);
        float spikes = (sx + sy) * (0.7 + spikeAmt * 0.4) + (sd1 + sd2) * spikeAmt * 0.45;
        float bright = core * 1.7 + halo * 0.6 + spikes * 0.95;
        float alpha  = core + halo + spikes * 0.7;
        gl_FragColor = vec4(vColor * bright * vTwinkle, alpha * vTwinkle);
        #include <logdepthbuf_fragment>
      }
    `,
    transparent: true, depthWrite: false, blending: THREE.AdditiveBlending
  });
  const anchors = new THREE.Points(aGeom, aMat);
  anchors.frustumCulled = false;
  anchors.userData.anchorMat = aMat;
  bgStarsGroup.add(anchors);
  window.__anchorMat = aMat;
}
```

### 6.2 Procedural Skydome with Milky Way (BackSide sphere)

Copy this shader verbatim. Procedural FBM paints a galactic band, dust lanes, and a warm bulge.

```js
{
  const skyMat = new THREE.ShaderMaterial({
    side: THREE.BackSide, depthWrite: false, depthTest: false,
    vertexShader: `
      varying vec3 vDir;
      void main() {
        vDir = normalize(position);
        gl_Position = projectionMatrix * modelViewMatrix * vec4(position, 1.0);
      }
    `,
    fragmentShader: `
      varying vec3 vDir;
      float hash(vec3 p) { return fract(sin(dot(p, vec3(12.9898,78.233,37.719))) * 43758.5453); }
      float noise(vec3 p) {
        vec3 i = floor(p); vec3 f = fract(p); f = f*f*(3.0-2.0*f);
        float n000=hash(i+vec3(0,0,0)); float n100=hash(i+vec3(1,0,0));
        float n010=hash(i+vec3(0,1,0)); float n110=hash(i+vec3(1,1,0));
        float n001=hash(i+vec3(0,0,1)); float n101=hash(i+vec3(1,0,1));
        float n011=hash(i+vec3(0,1,1)); float n111=hash(i+vec3(1,1,1));
        return mix(mix(mix(n000,n100,f.x),mix(n010,n110,f.x),f.y),
                   mix(mix(n001,n101,f.x),mix(n011,n111,f.x),f.y), f.z);
      }
      float fbm(vec3 p) {
        float v = 0.0; float a = 0.5;
        for (int i = 0; i < 5; i++) { v += noise(p) * a; p *= 2.13; a *= 0.55; }
        return v;
      }
      void main() {
        vec3 d = normalize(vDir);
        float up = clamp(d.y * 0.5 + 0.5, 0.0, 1.0);
        vec3 base = mix(vec3(0.006,0.009,0.020), vec3(0.014,0.020,0.038), smoothstep(0.20, 0.95, up));
        vec3 galN = normalize(vec3(0.40, 0.92, -0.05));
        float lat = abs(dot(d, galN));
        float band = exp(-lat * lat * 14.0);
        vec3 dp = d * 4.5;
        float clouds = fbm(dp);
        float dust = smoothstep(0.48, 0.78, fbm(dp * 2.7 + vec3(73.1, 11.4, 49.5)));
        vec3 coreDir = normalize(vec3(-0.25, -0.05, 0.96));
        float core = max(0.0, dot(d, coreDir));
        float bulge = pow(core, 10.0) * band;
        vec3 bandColor = mix(vec3(0.038,0.034,0.055), vec3(0.150,0.105,0.070), smoothstep(0.0, 0.55, core));
        float bandIntensity = band * (0.78 + clouds * 1.30) * (1.0 - dust * 0.78);
        base += bandColor * bandIntensity;
        base += vec3(0.190,0.130,0.080) * bulge * 1.8;
        float core2 = clamp(dot(d, normalize(vec3(-0.6, 0.2, 0.3))) * 0.5 + 0.5, 0.0, 1.0);
        base += vec3(0.028,0.020,0.014) * pow(core2, 6.0) * 0.7;
        float wide = fbm(d * 1.6 + vec3(5.0, 11.0, 7.0));
        base += vec3(0.012,0.014,0.020) * pow(wide, 1.4) * 0.55;
        gl_FragColor = vec4(base, 1.0);
      }
    `
  });
  const sky = new THREE.Mesh(new THREE.SphereGeometry(200000, 48, 32), skyMat);
  sky.frustumCulled = false;
  sky.renderOrder = -10;
  bgStarsGroup.add(sky);
}
```

Important: in the render loop, **every frame** do `bgStarsGroup.position.copy(camera.position)`. The group must follow the camera so the background never appears to translate.

---

## 7. ASTEROID SHAPES (10 pre-baked) + BELT MATERIALS

Use `posNoise` so vertices at identical positions get identical displacement → no cracks even on non-indexed `IcosahedronGeometry`. `flatShading:true` hides any sub-pixel gaps.

```js
function makeAsteroidGeometry(seed) {
  const r = mulberry32(seed);
  const detail = 1 + Math.floor(r() * 2);
  const geom = new THREE.IcosahedronGeometry(1, detail);
  const pos = geom.attributes.position;
  for (let i = 0; i < pos.count; i++) {
    const x = pos.getX(i), y = pos.getY(i), z = pos.getZ(i);
    const n1 = posNoise(x*3.0, y*3.0, z*3.0);
    const n2 = posNoise(x*7.0 + 5, y*7.0 + 5, z*7.0 + 5);
    const bump = 0.7 + n1*0.4 + n2*0.15;
    pos.setXYZ(i, x*bump, y*bump, z*bump);
  }
  pos.needsUpdate = true;
  geom.computeVertexNormals();
  geom.computeBoundingSphere();
  return geom;
}
const asteroidGeoms = [];
for (let i = 0; i < 10; i++) asteroidGeoms.push(makeAsteroidGeometry(1000 + i));

const beltMaterials = [
  new THREE.MeshStandardMaterial({ color: 0x6a5848, roughness:0.95, metalness:0.05, flatShading:true }),
  new THREE.MeshStandardMaterial({ color: 0x3d2e20, roughness:0.95, metalness:0.05, flatShading:true }),
  new THREE.MeshStandardMaterial({ color: 0x8a7860, roughness:0.92, metalness:0.05, flatShading:true }),
  new THREE.MeshStandardMaterial({ color: 0x4a4348, roughness:0.90, metalness:0.10, flatShading:true }),
  new THREE.MeshStandardMaterial({ color: 0x6b3a25, roughness:0.95, metalness:0.03, flatShading:true })
];

function makeBelt(starPos, innerR, outerR, count, matIdx) {
  const geom = asteroidGeoms[Math.floor(Math.random() * asteroidGeoms.length)];
  const mat = beltMaterials[(matIdx ?? 0) % beltMaterials.length];
  const inst = new THREE.InstancedMesh(geom, mat, count);
  const beltBasis = makeRandomBasis(Math.random);
  const thickness = 0.04 + Math.random() * 0.22;
  for (let i = 0; i < count; i++) {
    const a = Math.random() * TAU;
    const rr = innerR + Math.random() * (outerR - innerR);
    const offN = (Math.random() - 0.5) * (outerR - innerR) * thickness;
    const px = Math.cos(a)*rr * beltBasis.u.x + Math.sin(a)*rr * beltBasis.v.x + offN * beltBasis.n.x;
    const py = Math.cos(a)*rr * beltBasis.u.y + Math.sin(a)*rr * beltBasis.v.y + offN * beltBasis.n.y;
    const pz = Math.cos(a)*rr * beltBasis.u.z + Math.sin(a)*rr * beltBasis.v.z + offN * beltBasis.n.z;
    _v1.set(starPos.x + px, starPos.y + py, starPos.z + pz);
    _q1.setFromAxisAngle(_v2.set(Math.random()-0.5, Math.random()-0.5, Math.random()-0.5).normalize(),
                         Math.random() * TAU);
    const sr = Math.random();
    let s = sr < 0.7  ? 0.3 + Math.random() * 2.0
          : sr < 0.95 ? 2.0 + Math.random() * 4.0
                      : 5.0 + Math.random() * 9.0;
    _v3.set(s, s, s);
    _m1.compose(_v1, _q1, _v3);
    inst.setMatrixAt(i, _m1);
  }
  inst.instanceMatrix.needsUpdate = true;
  inst.frustumCulled = false;
  return inst;
}
```

---

## 8. RING-ASTEROID GROUP (per-instance rotation via shader)

Planet rings get real 3D rock instances WITH per-instance spin baked into the vertex shader (axis-angle rotation). This is the trickiest part — copy verbatim.

```js
const ringSpinMaterials = [];

function makeSpinningRockMaterial(baseMat) {
  const mat = baseMat.clone();
  const uTime = { value: 0 };
  mat.userData.uTime = uTime;
  mat.onBeforeCompile = (shader) => {
    shader.uniforms.uTime = uTime;
    shader.vertexShader = `
      attribute vec4 aSpinAxis;
      uniform float uTime;
      mat3 _axisAngleM(vec3 a, float ang) {
        float c = cos(ang), s = sin(ang), ic = 1.0 - c;
        return mat3(
          c + a.x*a.x*ic,     a.y*a.x*ic + a.z*s, a.z*a.x*ic - a.y*s,
          a.x*a.y*ic - a.z*s, c + a.y*a.y*ic,     a.z*a.y*ic + a.x*s,
          a.x*a.z*ic + a.y*s, a.y*a.z*ic - a.x*s, c + a.z*a.z*ic
        );
      }
    ` + shader.vertexShader
      .replace('#include <beginnormal_vertex>', `
        mat3 _spinM = _axisAngleM(aSpinAxis.xyz, uTime * aSpinAxis.w);
        vec3 objectNormal = _spinM * vec3( normal );
        #ifdef USE_TANGENT
          vec3 objectTangent = _spinM * vec3( tangent.xyz );
        #endif
      `)
      .replace('#include <begin_vertex>', `vec3 transformed = _spinM * vec3( position );`);
  };
  ringSpinMaterials.push(mat);
  return mat;
}

function makeRingAsteroidsGroup(planet) {
  if (!planet.rings || planet.ringInner == null) return null;
  const group = new THREE.Group();
  group.rotation.copy(planet.rings.rotation);
  const seed = (Math.floor(planet.orbit * 137) ^ Math.floor(planet.radius * 7919) ^ 0xC0FFEE) | 0;
  const r = mulberry32(seed);
  const innerR = planet.ringInner;
  const outerR = planet.ringOuter;
  const ringWidth = outerR - innerR;
  const count = Math.floor(9000 + r() * 6000 + ringWidth * 10);
  const baseGeom = asteroidGeoms[Math.floor(r() * asteroidGeoms.length)];
  const geom = baseGeom.clone();
  const mat = makeSpinningRockMaterial(beltMaterials[Math.floor(r() * 3)]);
  const inst = new THREE.InstancedMesh(geom, mat, count);
  inst.frustumCulled = false;
  const thickness = ringWidth * 0.022;
  const spinData = new Float32Array(count * 4);
  for (let i = 0; i < count; i++) {
    let t = r();
    t = 0.5 + (t - 0.5) * (0.9 + 0.2 * Math.sin(t * 12.0));
    t = clamp(t, 0.0, 1.0);
    const rad = innerR + t * ringWidth;
    const a = r() * TAU;
    const offN = (r() - 0.5) * thickness;
    const px = Math.cos(a) * rad, py = Math.sin(a) * rad, pz = offN;
    const sr = r();
    let s = sr < 0.82 ? 0.04 + r() * 0.18
          : sr < 0.98 ? 0.22 + r() * 0.55
                      : 0.80 + r() * 1.40;
    const q = new THREE.Quaternion().setFromAxisAngle(
      _v2.set(r()-0.5, r()-0.5, r()-0.5).normalize(), r() * TAU
    );
    _v1.set(px, py, pz); _v3.set(s, s, s);
    _m1.compose(_v1, q, _v3);
    inst.setMatrixAt(i, _m1);
    let ax = r()-0.5, ay = r()-0.5, az = r()-0.5;
    const len = Math.hypot(ax, ay, az) || 1;
    spinData[i*4]   = ax/len;
    spinData[i*4+1] = ay/len;
    spinData[i*4+2] = az/len;
    spinData[i*4+3] = (r() - 0.5) * 1.0 * (0.4 + r() * 0.6);
  }
  inst.instanceMatrix.needsUpdate = true;
  geom.setAttribute('aSpinAxis', new THREE.InstancedBufferAttribute(spinData, 4));
  group.add(inst);
  group.userData.spinMesh = inst;
  group.userData.angularSpeed = (0.012 + r() * 0.020) * (r() < 0.5 ? 1 : -1);
  return group;
}
```

---

## 9. RING TEXTURE + RING GEOMETRY

Long banded 1D-style canvas texture; custom UV mapping so the radial direction maps to the texture's X axis.

```js
const ringTextureCache = [];
function makeRingTexture(seed) {
  const r = mulberry32(seed);
  const W = 1024, H = 8;
  const cnv = document.createElement('canvas');
  cnv.width = W; cnv.height = H;
  const ctx = cnv.getContext('2d');
  ctx.clearRect(0, 0, W, H);
  const gapCount = 3 + Math.floor(r() * 4);
  const gaps = [];
  for (let i = 0; i < gapCount; i++) {
    gaps.push({ center: 0.20 + r() * 0.70, width: 0.005 + r() * 0.025 });
  }
  const bandFreqs = [
    { f: 70 + r() * 50, a: 0.30 + r() * 0.20, phase: r() * TAU },
    { f: 23 + r() * 12, a: 0.45 + r() * 0.20, phase: r() * TAU },
    { f: 7  + r() * 5,  a: 0.30 + r() * 0.20, phase: r() * TAU }
  ];
  const img = ctx.createImageData(W, H);
  const data = img.data;
  for (let x = 0; x < W; x++) {
    const t = x / W;
    let density = smoothstep(0.0, 0.08, t) * (1.0 - smoothstep(0.85, 1.0, t));
    let band = 0.7;
    for (const b of bandFreqs) band += Math.sin(t * b.f + b.phase) * b.a * 0.5;
    band = clamp(band, 0.0, 1.0);
    density *= band;
    for (const g of gaps) {
      const d = Math.abs(t - g.center) / g.width;
      density *= clamp(d * d, 0.0, 1.0);
    }
    density *= 0.65 + 0.35 * posNoise(t * 47, seed * 0.31, 0.5);
    density = clamp(density, 0.0, 1.0);
    const warm = 1.0 - t;
    const rC = 0.85 + warm * 0.15;
    const gC = 0.78 + warm * 0.12;
    const bC = 0.72 + warm * 0.05;
    for (let y = 0; y < H; y++) {
      const idx = (y * W + x) * 4;
      data[idx]   = (rC * 255) | 0;
      data[idx+1] = (gC * 255) | 0;
      data[idx+2] = (bC * 255) | 0;
      data[idx+3] = (density * 255) | 0;
    }
  }
  ctx.putImageData(img, 0, 0);
  const tex = new THREE.CanvasTexture(cnv);
  tex.colorSpace = THREE.SRGBColorSpace;
  tex.anisotropy = 8;
  return tex;
}
for (let i = 0; i < 6; i++) ringTextureCache.push(makeRingTexture(2000 + i));

function makeRingGeometry(innerR, outerR, segs) {
  const geom = new THREE.RingGeometry(innerR, outerR, segs, 1);
  const uv = geom.attributes.uv;
  const pos = geom.attributes.position;
  for (let i = 0; i < uv.count; i++) {
    const x = pos.getX(i), y = pos.getY(i);
    const rad = Math.sqrt(x*x + y*y);
    const t = (rad - innerR) / (outerR - innerR);
    uv.setXY(i, t, 0.5);
  }
  uv.needsUpdate = true;
  return geom;
}
```

---

## 10. PLANET — Surface Paint + Atmosphere + Clouds + Rings + Moons

Three planet types: **rocky** (radius 10–45, vertex-colored continents+oceans+ice caps), **gas** (60–160, banded with 6 palettes, optional storms), **ice** (25–75, pale blue-white). Each planet has a random 3D orbital plane via `makeRandomBasis`.

```js
function paintPlanetSurface(geom, r, type) {
  const pos = geom.attributes.position;
  const colors = new Float32Array(pos.count * 3);
  if (type === 'gas') {
    const palettes = [
      { a: new THREE.Color(0xd6c6a3), b: new THREE.Color(0x8a6438), c: new THREE.Color(0xefdcb6) },
      { a: new THREE.Color(0xe9c896), b: new THREE.Color(0xa6743a), c: new THREE.Color(0xf6e8c4) },
      { a: new THREE.Color(0xa6c2d8), b: new THREE.Color(0x4a6e92), c: new THREE.Color(0xd4e2ee) },
      { a: new THREE.Color(0x3760a4), b: new THREE.Color(0x1b2a4e), c: new THREE.Color(0x6488c4) },
      { a: new THREE.Color(0xc05544), b: new THREE.Color(0x6b1f15), c: new THREE.Color(0xe28872) },
      { a: new THREE.Color(0xa46db3), b: new THREE.Color(0x3a1d4a), c: new THREE.Color(0xd0a3df) }
    ];
    const palette = palettes[Math.floor(r() * palettes.length)];
    const baseA = palette.a, baseB = palette.b, baseC = palette.c;
    const bandCount = 8 + Math.floor(r() * 6);
    const phase = r() * TAU;
    for (let i = 0; i < pos.count; i++) {
      const x = pos.getX(i), y = pos.getY(i), z = pos.getZ(i);
      const R = Math.sqrt(x*x + y*y + z*z);
      const lat = y / R;
      const turb = posNoise(x*0.08, y*0.08, z*0.08) * 0.22 + posNoise(x*0.35, y*0.35, z*0.35) * 0.08;
      const raw = Math.cos((lat + turb) * bandCount + phase);
      const sig = clamp(raw * 0.5 + 0.5, 0, 1);
      const sigSharp = sig * sig * (3 - 2 * sig);
      const tri = (lat * 0.5 + 0.5) * 3.0;
      const slice = Math.floor(tri);
      const local = tri - slice;
      const tA = (slice === 0) ? baseA : (slice === 1 ? baseB : baseC);
      const tB = (slice === 0) ? baseB : (slice === 1 ? baseC : baseA);
      const c = _c1.copy(tA).lerp(tB, local);
      const highlight = _c2.copy(baseC).lerp(baseA, 0.4);
      c.lerp(highlight, sigSharp * 0.4);
      const storm = posNoise(x*0.40 + 1.7, y*0.40, z*0.40 + 3.3);
      if (Math.abs(lat) < 0.45 && storm > 0.75) c.lerp(_c2.copy(baseB).multiplyScalar(0.6), (storm - 0.75) * 4);
      const tint = clamp(0.72 + posNoise(x*0.55, y*0.55, z*0.55) * 0.5, 0.55, 1.40);
      c.multiplyScalar(tint);
      colors[i*3] = c.r; colors[i*3+1] = c.g; colors[i*3+2] = c.b;
    }
  } else if (type === 'ice') {
    const baseA = new THREE.Color().setHSL(0.55 + r()*0.10, 0.25, 0.78);
    const baseB = new THREE.Color().setHSL(0.55 + r()*0.10, 0.32, 0.55);
    for (let i = 0; i < pos.count; i++) {
      const x = pos.getX(i), y = pos.getY(i), z = pos.getZ(i);
      const R = Math.sqrt(x*x + y*y + z*z);
      const lat = Math.abs(y / R);
      const n = posNoise(x*0.18, y*0.18, z*0.18);
      const c = _c1.copy(baseB).lerp(baseA, lat * 0.8 + n * 0.25);
      colors[i*3] = c.r; colors[i*3+1] = c.g; colors[i*3+2] = c.b;
    }
  } else {
    const oceanCol = new THREE.Color().setHSL(0.58 + r()*0.05, 0.55, 0.28);
    const landCol  = new THREE.Color().setHSL(0.08 + r()*0.18, 0.45, 0.38);
    const desertCol= new THREE.Color().setHSL(0.10 + r()*0.04, 0.30, 0.55);
    const iceCol   = new THREE.Color(0xeaf2ff);
    const hasOcean = r() < 0.7;
    for (let i = 0; i < pos.count; i++) {
      const x = pos.getX(i), y = pos.getY(i), z = pos.getZ(i);
      const R = Math.sqrt(x*x + y*y + z*z);
      const lat = Math.abs(y / R);
      const n1 = posNoise(x*0.12, y*0.12, z*0.12);
      const n2 = posNoise(x*0.55 + 7, y*0.55, z*0.55 + 3);
      const elev = n1 * 0.7 + n2 * 0.3;
      let c;
      if (hasOcean && elev < 0.45) c = _c1.copy(oceanCol);
      else                         c = _c1.copy(landCol).lerp(desertCol, Math.max(0, n2 - 0.4));
      if (lat > 0.80 - n2*0.10) c.lerp(iceCol, smoothstep(0.78, 0.95, lat));
      colors[i*3] = c.r; colors[i*3+1] = c.g; colors[i*3+2] = c.b;
    }
  }
  geom.setAttribute('color', new THREE.BufferAttribute(colors, 3));
}
```

### 10.1 Atmosphere shader (Fresnel rim + sun-side bias)

```js
function makeAtmoMaterial(atmoColor) {
  return new THREE.ShaderMaterial({
    uniforms: {
      uColor: { value: atmoColor },
      uOpacity: { value: 1.0 },
      uPlanetCenterW: { value: new THREE.Vector3() },
      uSunDirW: { value: new THREE.Vector3(1,0,0) }
    },
    vertexShader: `
      #include <common>
      #include <logdepthbuf_pars_vertex>
      varying vec3 vN; varying vec3 vV; varying vec3 vWorld;
      void main() {
        vec4 wp = modelMatrix * vec4(position, 1.0);
        vWorld = wp.xyz;
        vec4 mv = viewMatrix * wp;
        vN = normalize(normalMatrix * normal);
        vV = normalize(-mv.xyz);
        gl_Position = projectionMatrix * mv;
        #include <logdepthbuf_vertex>
      }
    `,
    fragmentShader: `
      #include <common>
      #include <logdepthbuf_pars_fragment>
      uniform vec3 uColor; uniform float uOpacity;
      uniform vec3 uPlanetCenterW; uniform vec3 uSunDirW;
      varying vec3 vN; varying vec3 vV; varying vec3 vWorld;
      void main() {
        float f = 1.0 - abs(dot(normalize(vN), normalize(vV)));
        float rim = pow(f, 2.6);
        vec3 surfaceDirW = normalize(vWorld - uPlanetCenterW);
        float sunFacing = clamp(dot(surfaceDirW, uSunDirW) * 0.5 + 0.5, 0.0, 1.0);
        float side = mix(0.25, 1.0, smoothstep(0.30, 0.78, sunFacing));
        gl_FragColor = vec4(uColor * (0.7 + rim * 1.4) * side, rim * uOpacity * side);
        #include <logdepthbuf_fragment>
      }
    `,
    transparent: true, depthWrite: false, side: THREE.BackSide, blending: THREE.AdditiveBlending
  });
}
```

### 10.2 Cloud shader (gas giants, with FBM bands)

```js
function makeCloudMaterial(cloudColor, seed) {
  return new THREE.ShaderMaterial({
    uniforms: { uColor: { value: cloudColor }, uOpacity: { value: 1.0 }, uTime: { value: 0 }, uSeed: { value: seed } },
    vertexShader: `
      #include <common>
      #include <logdepthbuf_pars_vertex>
      varying vec3 vLocal; varying vec3 vN; varying vec3 vV;
      void main() {
        vLocal = position;
        vec4 mv = modelViewMatrix * vec4(position, 1.0);
        vN = normalize(normalMatrix * normal);
        vV = normalize(-mv.xyz);
        gl_Position = projectionMatrix * mv;
        #include <logdepthbuf_vertex>
      }
    `,
    fragmentShader: `
      #include <common>
      #include <logdepthbuf_pars_fragment>
      uniform vec3 uColor; uniform float uOpacity; uniform float uTime; uniform float uSeed;
      varying vec3 vLocal; varying vec3 vN; varying vec3 vV;
      float hash(vec3 p) { return fract(sin(dot(p, vec3(12.9898,78.233,37.719))) * 43758.5453); }
      float noise(vec3 p) {
        vec3 i = floor(p); vec3 f = fract(p); f = f*f*(3.0-2.0*f);
        float n000=hash(i); float n100=hash(i+vec3(1,0,0));
        float n010=hash(i+vec3(0,1,0)); float n110=hash(i+vec3(1,1,0));
        float n001=hash(i+vec3(0,0,1)); float n101=hash(i+vec3(1,0,1));
        float n011=hash(i+vec3(0,1,1)); float n111=hash(i+vec3(1,1,1));
        return mix(mix(mix(n000,n100,f.x),mix(n010,n110,f.x),f.y),
                   mix(mix(n001,n101,f.x),mix(n011,n111,f.x),f.y), f.z);
      }
      void main() {
        vec3 n = normalize(vLocal);
        float lat = n.y;
        float t = uTime * 0.02 + uSeed * 30.0;
        float a = noise(vec3(n.x*4.0 + t, lat*6.0, n.z*4.0)) * 0.6;
        float b = noise(vec3(n.x*16.0 - t*1.7, lat*22.0, n.z*16.0 + t*0.3)) * 0.4;
        float pattern = a + b;
        float bandSig = sin(lat * 9.0 + pattern * 1.5 + uSeed * 12.0) * 0.5 + 0.5;
        float alpha = smoothstep(0.30, 0.70, pattern * 0.6 + bandSig * 0.5);
        float f = clamp(dot(normalize(vN), normalize(vV)), 0.0, 1.0);
        alpha *= mix(0.25, 0.85, f);
        gl_FragColor = vec4(uColor, alpha * uOpacity);
        #include <logdepthbuf_fragment>
      }
    `,
    transparent: true, depthWrite: false, blending: THREE.NormalBlending
  });
}
```

### 10.3 Ring shader (textured banding + planet shadow)

```js
function makeRingMaterial(ringTex, ringTint, planetRadius) {
  return new THREE.ShaderMaterial({
    uniforms: {
      uMap: { value: ringTex }, uTint: { value: ringTint }, uOpacity: { value: 0.95 },
      uPlanetCenterW: { value: new THREE.Vector3() },
      uPlanetRadius: { value: planetRadius },
      uSunDirW: { value: new THREE.Vector3(1,0,0) }
    },
    vertexShader: `
      #include <common>
      #include <logdepthbuf_pars_vertex>
      varying vec2 vUv; varying vec3 vWorld;
      void main() {
        vUv = uv;
        vec4 wp = modelMatrix * vec4(position, 1.0);
        vWorld = wp.xyz;
        gl_Position = projectionMatrix * viewMatrix * wp;
        #include <logdepthbuf_vertex>
      }
    `,
    fragmentShader: `
      #include <common>
      #include <logdepthbuf_pars_fragment>
      uniform sampler2D uMap; uniform vec3 uTint; uniform float uOpacity;
      uniform vec3 uPlanetCenterW; uniform float uPlanetRadius; uniform vec3 uSunDirW;
      varying vec2 vUv; varying vec3 vWorld;
      void main() {
        vec4 t = texture2D(uMap, vUv);
        vec3 c = t.rgb * uTint;
        vec3 rel = vWorld - uPlanetCenterW;
        float along = dot(rel, uSunDirW);
        vec3 perp = rel - along * uSunDirW;
        float pd = length(perp);
        float shadow = 1.0;
        if (along < 0.0) shadow = smoothstep(uPlanetRadius * 0.85, uPlanetRadius * 1.10, pd);
        float litHemi = 0.5 + 0.5 * sign(along);
        float light = mix(0.55, 1.10, smoothstep(0.0, 1.0, litHemi));
        gl_FragColor = vec4(c * light * shadow, t.a * uOpacity);
        #include <logdepthbuf_fragment>
      }
    `,
    transparent: true, depthWrite: false, side: THREE.DoubleSide, blending: THREE.NormalBlending
  });
}
```

### 10.4 `makePlanet(r, starRadius, orbit)`

```js
function makePlanet(r, starRadius, orbit) {
  const typeRoll = r();
  let type, radius;
  if (typeRoll < 0.42)      { type = 'rocky'; radius = randRange(r, 10, 45); }
  else if (typeRoll < 0.78) { type = 'gas';   radius = randRange(r, 60, 160); }
  else                      { type = 'ice';   radius = randRange(r, 25, 75); }
  const geom = new THREE.SphereGeometry(radius, 48, 48);
  paintPlanetSurface(geom, r, type);
  geom.computeBoundingSphere();
  const mat = new THREE.MeshStandardMaterial({
    vertexColors: true,
    roughness: type === 'gas' ? 0.85 : (type === 'ice' ? 0.55 : 0.95),
    metalness: 0.0, transparent: true, opacity: 1
  });
  const mesh = new THREE.Mesh(geom, mat);
  mesh.userData.kind = 'planet';
  const basis = makeRandomBasis(r);
  const result = {
    mesh, orbit, phase: r() * TAU, speed: 0.05 + r() * 0.1, radius, type,
    atmoBaseOpacity: 0.45, ringBaseOpacity: 0.85,
    orbitU: basis.u, orbitV: basis.v, orbitN: basis.n, moons: []
  };
  const wantAtmo = (type === 'gas') || (type === 'rocky' && r() < 0.55) || (type === 'ice' && r() < 0.30);
  if (wantAtmo) {
    let atmoColor;
    if (type === 'gas')      atmoColor = new THREE.Color().setHSL(0.06 + r()*0.10, 0.65, 0.60);
    else if (type === 'ice') atmoColor = new THREE.Color().setHSL(0.55, 0.55, 0.78);
    else                     atmoColor = new THREE.Color().setHSL(0.55 + r()*0.10, 0.55, 0.65);
    result.atmosphere = new THREE.Mesh(new THREE.SphereGeometry(radius * 1.08, 32, 32), makeAtmoMaterial(atmoColor));
  }
  if (type === 'gas' && r() < 0.7) {
    const cloudColor = new THREE.Color().setHSL(0.06 + r()*0.10, 0.18, 0.85);
    result.clouds = new THREE.Mesh(new THREE.SphereGeometry(radius * 1.012, 48, 32), makeCloudMaterial(cloudColor, r()));
  }
  let ringChance = type === 'gas' ? 0.75 : type === 'ice' ? 0.30 : (radius > 35 ? 0.12 : 0);
  if (r() < ringChance) {
    const ringTex = ringTextureCache[Math.floor(r() * ringTextureCache.length)];
    const ringInner = radius * (1.30 + r() * 0.20);
    const ringOuter = radius * (2.05 + r() * 0.55);
    const ringTint = new THREE.Color().setHSL(0.08 + r()*0.06, 0.30 + r()*0.20, 0.60 + r()*0.20);
    const ringMat = makeRingMaterial(ringTex, ringTint, radius);
    const rings = new THREE.Mesh(makeRingGeometry(ringInner, ringOuter, 192), ringMat);
    rings.rotation.set(Math.PI * 0.5 - (0.18 + r()*0.40), r() * TAU, r() * 0.4 - 0.2);
    result.rings = rings;
    result.ringInner = ringInner;
    result.ringOuter = ringOuter;
    result.ringAsteroids = makeRingAsteroidsGroup(result);
  }
  let moonCount = 0;
  if (type === 'gas')      moonCount = 1 + Math.floor(r() * 3);
  else if (type === 'ice') moonCount = Math.floor(r() * 3);
  else if (radius > 25)    moonCount = Math.floor(r() * 2);
  for (let m = 0; m < moonCount; m++) {
    const moonR = radius * (0.14 + r() * 0.22);
    const moonGeom = new THREE.SphereGeometry(moonR, 28, 20);
    const flav = r();
    let base, secondary, mottle, glow = 0;
    if      (flav < 0.30) { base = new THREE.Color(0x8a7c66); secondary = new THREE.Color(0x4a4138); mottle = 0.55; }
    else if (flav < 0.55) { base = new THREE.Color(0xe6eaf0); secondary = new THREE.Color(0xb9c4d6); mottle = 0.35; }
    else if (flav < 0.75) { base = new THREE.Color(0xc4823a); secondary = new THREE.Color(0x6b3a18); mottle = 0.75; glow = 0.18; }
    else if (flav < 0.88) { base = new THREE.Color(0x9a4f3a); secondary = new THREE.Color(0x4f2418); mottle = 0.55; }
    else                  { base = new THREE.Color(0x6b8aaa); secondary = new THREE.Color(0x2a3e58); mottle = 0.45; }
    const mcolors = new Float32Array(moonGeom.attributes.position.count * 3);
    for (let i = 0; i < moonGeom.attributes.position.count; i++) {
      const x = moonGeom.attributes.position.getX(i);
      const y = moonGeom.attributes.position.getY(i);
      const z = moonGeom.attributes.position.getZ(i);
      const n1 = posNoise(x*0.5, y*0.5, z*0.5);
      const n2 = posNoise(x*1.4 + 11, y*1.4, z*1.4 + 5);
      const blend = clamp(n1 * 0.6 + n2 * 0.4, 0, 1);
      const c = _c1.copy(secondary).lerp(base, blend);
      c.multiplyScalar(0.75 + mottle * blend * 0.6);
      mcolors[i*3] = c.r; mcolors[i*3+1] = c.g; mcolors[i*3+2] = c.b;
    }
    moonGeom.setAttribute('color', new THREE.BufferAttribute(mcolors, 3));
    const moonMat = new THREE.MeshStandardMaterial({
      vertexColors: true,
      roughness: flav < 0.55 ? 0.55 : 0.95, metalness: 0.0,
      emissive: glow > 0 ? base : new THREE.Color(0x000000),
      emissiveIntensity: glow, transparent: true, opacity: 1
    });
    const moon = new THREE.Mesh(moonGeom, moonMat);
    moon.userData.kind = 'moon';
    const mb = makeRandomBasis(r);
    result.moons.push({ mesh: moon, orbit: radius * (2.6 + r()*2.5), phase: r()*TAU,
                        speed: 0.4 + r()*0.7, radius: moonR, basisU: mb.u, basisV: mb.v });
  }
  mesh.userData.atmosphere = result.atmosphere || null;
  mesh.userData.clouds = result.clouds || null;
  mesh.userData.rings = result.rings || null;
  mesh.userData.ringAsteroids = result.ringAsteroids || null;
  mesh.userData.planetRef = result;
  return result;
}
```

### 10.5 Showcase planet (the home-system hero)

The first planet of the home system must be a hand-tuned **gas giant radius=145** with detailed banded rings (inner 1.42×, outer 2.62×, tint 0xf3d8a0), a vibrant atmosphere, a cloud layer, and exactly **3 contrasting moons** (volcanic ochre 0xc4823a glow=0.18, icy white 0xe6eaf0, bluish ocean 0x6b8aaa). Phase locked at 1.1, speed 0.08, ring rotation (PI*0.5-0.55, 0.7, 0.12). Implement as a `makeShowcasePlanet(r, orbit)` that mirrors `makePlanet` but forces these values and uses `ringTextureCache[0]` with a 256-segment ring geometry.

---

## 11. SYSTEMS — Build / Activate / Deactivate

```js
const destructibles = [];
let activeSystemStar = null;
const PLANET_FULL = 9000;
const PLANET_GONE = 22000;

function buildSystem(star) {
  const sys = { planets: [], belts: [], active: false };
  const r = mulberry32((Math.abs(Math.floor(star.pos.x)) * 7919 +
                        Math.abs(Math.floor(star.pos.y)) * 31 +
                        Math.abs(Math.floor(star.pos.z)) * 113) | 0);
  const planetCount = 4 + Math.floor(r() * 5);
  let lastOrbit = star.radius * 3.0 + 200;
  const isHome = (star === home);
  for (let i = 0; i < planetCount; i++) {
    lastOrbit += 300 + r() * 600;
    const p = (isHome && i === 1) ? makeShowcasePlanet(r, lastOrbit) : makePlanet(r, star.radius, lastOrbit);
    p.starRef = star;
    p.mesh.userData.starRef = star;
    sys.planets.push(p);
  }
  const beltCount = 2 + Math.floor(r() * 4);
  for (let i = 0; i < beltCount; i++) {
    const innerR = star.radius * (2.5 + r() * 4) + r() * 3500;
    const outerR = innerR + 350 + r() * 1900;
    const count = 1500 + Math.floor(r() * 3000);
    sys.belts.push(makeBelt(star.pos, innerR, outerR, count, Math.floor(r() * beltMaterials.length)));
  }
  star.system = sys;
}

function activateSystem(star) {
  if (!star.system) buildSystem(star);
  const sys = star.system;
  if (sys.active) return;
  for (const p of sys.planets) {
    scene.add(p.mesh); destructibles.push(p.mesh);
    if (p.atmosphere) scene.add(p.atmosphere);
    if (p.clouds) scene.add(p.clouds);
    if (p.rings) scene.add(p.rings);
    if (p.ringAsteroids) scene.add(p.ringAsteroids);
    for (const m of p.moons) {
      scene.add(m.mesh);
      m.mesh.userData.kind = 'moon';
      m.mesh.userData.planetRef = p;
      m.mesh.userData.starRef = star;
      destructibles.push(m.mesh);
    }
  }
  for (const b of sys.belts) scene.add(b);
  sys.active = true;
}

function deactivateSystem(star) {
  const sys = star.system;
  if (!sys || !sys.active) return;
  for (const p of sys.planets) {
    if (p.mesh.parent) p.mesh.parent.remove(p.mesh);
    if (p.atmosphere && p.atmosphere.parent) p.atmosphere.parent.remove(p.atmosphere);
    if (p.clouds && p.clouds.parent) p.clouds.parent.remove(p.clouds);
    if (p.rings && p.rings.parent) p.rings.parent.remove(p.rings);
    if (p.ringAsteroids && p.ringAsteroids.parent) p.ringAsteroids.parent.remove(p.ringAsteroids);
    for (const m of p.moons) {
      if (m.mesh.parent) m.mesh.parent.remove(m.mesh);
      const mi = destructibles.indexOf(m.mesh);
      if (mi !== -1) destructibles.splice(mi, 1);
    }
    const di = destructibles.indexOf(p.mesh);
    if (di !== -1) destructibles.splice(di, 1);
  }
  for (const b of sys.belts) if (b.parent) b.parent.remove(b);
  sys.active = false;
}

function updateActiveSystems(time) {
  if (!activeSystemStar) return;
  const star = activeSystemStar; const sys = star.system;
  if (!sys || !sys.active) return;
  for (const p of sys.planets) {
    const a = p.phase + time * p.speed * 0.05;
    const ox = Math.cos(a) * p.orbit;
    const oy = Math.sin(a) * p.orbit;
    p.mesh.position.set(
      star.pos.x + ox * p.orbitU.x + oy * p.orbitV.x,
      star.pos.y + ox * p.orbitU.y + oy * p.orbitV.y,
      star.pos.z + ox * p.orbitU.z + oy * p.orbitV.z
    );
    p.mesh.rotation.y += 0.005;
    const d = camera.position.distanceTo(p.mesh.position);
    const f = smoothstep(PLANET_GONE, PLANET_FULL, d);
    p.mesh.material.opacity = f;
    p.mesh.visible = f > 0.005;
    if (p.atmosphere) {
      p.atmosphere.position.copy(p.mesh.position);
      const u = p.atmosphere.material.uniforms;
      u.uOpacity.value = f * p.atmoBaseOpacity;
      u.uPlanetCenterW.value.copy(p.mesh.position);
      _v1.copy(star.pos).sub(p.mesh.position).normalize();
      u.uSunDirW.value.copy(_v1);
      p.atmosphere.visible = p.mesh.visible;
    }
    if (p.clouds) {
      p.clouds.position.copy(p.mesh.position);
      p.clouds.rotation.copy(p.mesh.rotation);
      p.clouds.material.uniforms.uOpacity.value = f * 0.85;
      p.clouds.material.uniforms.uTime.value = time;
      p.clouds.visible = p.mesh.visible;
    }
    if (p.rings) {
      p.rings.position.copy(p.mesh.position);
      const u = p.rings.material.uniforms;
      u.uOpacity.value = f * p.ringBaseOpacity;
      u.uPlanetCenterW.value.copy(p.mesh.position);
      u.uPlanetRadius.value = p.radius;
      _v1.copy(star.pos).sub(p.mesh.position).normalize();
      u.uSunDirW.value.copy(_v1);
      p.rings.visible = p.mesh.visible;
    }
    if (p.ringAsteroids) {
      p.ringAsteroids.position.copy(p.mesh.position);
      const spin = p.ringAsteroids.userData.spinMesh;
      if (spin) spin.rotation.z = time * p.ringAsteroids.userData.angularSpeed;
      p.ringAsteroids.visible = p.mesh.visible;
    }
    for (const m of p.moons) {
      const ma = m.phase + time * m.speed;
      const mox = Math.cos(ma) * m.orbit;
      const moy = Math.sin(ma) * m.orbit;
      m.mesh.position.set(
        p.mesh.position.x + mox * m.basisU.x + moy * m.basisV.x,
        p.mesh.position.y + mox * m.basisU.y + moy * m.basisV.y,
        p.mesh.position.z + mox * m.basisU.z + moy * m.basisV.z
      );
      m.mesh.rotation.y += 0.01;
      m.mesh.material.opacity = f;
      m.mesh.visible = p.mesh.visible;
    }
  }
}
```

### 11.1 Nearest-star tracker

```js
let currentNearStar = null;
function updateNearStars() {
  let nearest = null, bestD = Infinity;
  for (const s of allActiveStars) {
    if (s.dead) continue;
    const d = camera.position.distanceToSquared(s.pos);
    if (d < bestD) { bestD = d; nearest = s; }
  }
  if (nearest !== currentNearStar) {
    if (activeSystemStar && activeSystemStar !== nearest) {
      deactivateSystem(activeSystemStar); activeSystemStar = null;
    }
    currentNearStar = nearest;
    if (nearest) {
      activateSystem(nearest); activeSystemStar = nearest;
      starLight.position.copy(nearest.pos);
      starLight.color.copy(nearest.color);
      starLight.intensity = 3.5;
    }
  }
  if (currentNearStar) {
    starLight.target.position.copy(camera.position);
    starLight.target.updateMatrixWorld();
  }
}
```

---

## 12. NEBULAE — 5 distant stellar nurseries

Each nursery is a 2-layer sprite billboard + 4–7 embedded newborn-star sprites. Texture uses 6 painted layers on a 512² canvas (halo, puffy clouds, accent pockets, filaments, hot core, dust silhouette). 5 distant placements at 70 000–130 000 from home, distributed quasi-spherically with Fibonacci-spaced theta.

```js
function makeNurseryTexture(coreCol, shellCol, seed) {
  const r = mulberry32(seed);
  const accent = new THREE.Color().setHSL(
    (new THREE.Color().copy(coreCol).getHSL({}).h + 0.5) % 1, 0.55, 0.55
  );
  const size = 512;
  const cnv = document.createElement('canvas'); cnv.width = size; cnv.height = size;
  const ctx = cnv.getContext('2d');
  ctx.fillStyle = '#000'; ctx.fillRect(0, 0, size, size);
  ctx.globalCompositeOperation = 'lighter';
  const cx = size/2, cy = size/2;
  // 1: shell halo
  const halo = ctx.createRadialGradient(cx, cy, size*0.08, cx, cy, size*0.50);
  const sR = (shellCol.r*255)|0, sG = (shellCol.g*255)|0, sB = (shellCol.b*255)|0;
  halo.addColorStop(0.00, `rgba(${sR},${sG},${sB},0.80)`);
  halo.addColorStop(0.30, `rgba(${sR},${sG},${sB},0.35)`);
  halo.addColorStop(0.70, `rgba(${sR},${sG},${sB},0.08)`);
  halo.addColorStop(1.00, `rgba(${sR},${sG},${sB},0)`);
  ctx.fillStyle = halo; ctx.fillRect(0, 0, size, size);
  // 2: puffy clouds (core)
  for (let i = 0; i < 28; i++) {
    const a = r() * TAU; const rad = Math.pow(r(), 0.7) * size * 0.34;
    const x = cx + Math.cos(a) * rad; const y = cy + Math.sin(a) * rad;
    const br = 50 + r() * 90;
    const cR = (coreCol.r*255)|0, cG = (coreCol.g*255)|0, cB = (coreCol.b*255)|0;
    const g = ctx.createRadialGradient(x, y, 0, x, y, br);
    g.addColorStop(0,   `rgba(${cR},${cG},${cB},${0.45 + r()*0.35})`);
    g.addColorStop(0.5, `rgba(${cR},${cG},${cB},${0.18 + r()*0.18})`);
    g.addColorStop(1,   `rgba(${cR},${cG},${cB},0)`);
    ctx.fillStyle = g; ctx.fillRect(x - br, y - br, br*2, br*2);
  }
  // 3: accent pockets
  for (let i = 0; i < 16; i++) {
    const a = r() * TAU; const rad = Math.pow(r(), 0.5) * size * 0.36;
    const x = cx + Math.cos(a) * rad; const y = cy + Math.sin(a) * rad;
    const br = 30 + r() * 60;
    const aR = (accent.r*255)|0, aG = (accent.g*255)|0, aB = (accent.b*255)|0;
    const g = ctx.createRadialGradient(x, y, 0, x, y, br);
    g.addColorStop(0,   `rgba(${aR},${aG},${aB},${0.55 + r()*0.35})`);
    g.addColorStop(0.6, `rgba(${aR},${aG},${aB},${0.18 + r()*0.15})`);
    g.addColorStop(1,   `rgba(${aR},${aG},${aB},0)`);
    ctx.fillStyle = g; ctx.fillRect(x - br, y - br, br*2, br*2);
  }
  // 4: filaments
  for (let i = 0; i < 90; i++) {
    const a = r() * TAU; const rad = Math.pow(r(), 0.4) * size * 0.40;
    const x = cx + Math.cos(a) * rad; const y = cy + Math.sin(a) * rad;
    const br = 4 + r() * 10;
    const g = ctx.createRadialGradient(x, y, 0, x, y, br);
    g.addColorStop(0, `rgba(255,250,230,${0.45 + r()*0.45})`);
    g.addColorStop(1, `rgba(255,250,230,0)`);
    ctx.fillStyle = g; ctx.fillRect(x - br, y - br, br*2, br*2);
  }
  // 5: core
  const core = ctx.createRadialGradient(cx, cy, 0, cx, cy, size*0.14);
  const cR = (coreCol.r*255)|0, cG = (coreCol.g*255)|0, cB = (coreCol.b*255)|0;
  core.addColorStop(0.00, 'rgba(255,255,255,1.0)');
  core.addColorStop(0.18, `rgba(${cR},${cG},${cB},0.65)`);
  core.addColorStop(0.55, `rgba(${cR},${cG},${cB},0.20)`);
  core.addColorStop(1.00, `rgba(${cR},${cG},${cB},0)`);
  ctx.fillStyle = core; ctx.fillRect(0, 0, size, size);
  // 6: dust subtract
  ctx.globalCompositeOperation = 'destination-out';
  for (let i = 0; i < 8; i++) {
    const a = r() * TAU; const rad = Math.pow(r(), 0.6) * size * 0.32;
    const x = cx + Math.cos(a) * rad; const y = cy + Math.sin(a) * rad;
    const br = 28 + r() * 60;
    const g = ctx.createRadialGradient(x, y, 0, x, y, br);
    g.addColorStop(0, `rgba(0,0,0,${0.35 + r()*0.40})`);
    g.addColorStop(1, 'rgba(0,0,0,0)');
    ctx.fillStyle = g; ctx.fillRect(x - br, y - br, br*2, br*2);
  }
  // edge mask
  ctx.globalCompositeOperation = 'destination-in';
  const mask = ctx.createRadialGradient(cx, cy, size*0.08, cx, cy, size*0.50);
  mask.addColorStop(0, 'rgba(0,0,0,1)'); mask.addColorStop(1, 'rgba(0,0,0,0)');
  ctx.fillStyle = mask; ctx.fillRect(0, 0, size, size);
  const tex = new THREE.CanvasTexture(cnv);
  tex.colorSpace = THREE.SRGBColorSpace;
  return tex;
}

function makeNebulaStarTex() {
  const size = 64;
  const cnv = document.createElement('canvas'); cnv.width = size; cnv.height = size;
  const ctx = cnv.getContext('2d');
  const g = ctx.createRadialGradient(size/2, size/2, 0, size/2, size/2, size/2);
  g.addColorStop(0.00, 'rgba(255,255,255,1)');
  g.addColorStop(0.30, 'rgba(255,255,255,0.45)');
  g.addColorStop(1.00, 'rgba(255,255,255,0)');
  ctx.fillStyle = g; ctx.fillRect(0, 0, size, size);
  const tex = new THREE.CanvasTexture(cnv);
  tex.colorSpace = THREE.SRGBColorSpace;
  return tex;
}

function spawnNebulae() {
  const nebRng = mulberry32(9999);
  const schemes = [
    { core: new THREE.Color(0xff5577), shell: new THREE.Color(0x1166aa) },
    { core: new THREE.Color(0xff9944), shell: new THREE.Color(0x115588) },
    { core: new THREE.Color(0xffaa66), shell: new THREE.Color(0x442266) },
    { core: new THREE.Color(0xffeeaa), shell: new THREE.Color(0x1166cc) },
    { core: new THREE.Color(0xff77cc), shell: new THREE.Color(0x114466) },
    { core: new THREE.Color(0x55ffcc), shell: new THREE.Color(0x442288) },
    { core: new THREE.Color(0xffcc66), shell: new THREE.Color(0x223388) }
  ];
  const N = 5;
  for (let i = 0; i < N; i++) {
    const u = (i + 0.5) / N * 2.0 - 1.0;
    const theta = i * 2.39996 + nebRng() * 0.4;
    const sr = Math.sqrt(1 - u*u);
    const dist = 70000 + nebRng() * 60000;
    const cx = home.pos.x + sr * Math.cos(theta) * dist;
    const cy = home.pos.y + u * dist * 0.55;
    const cz = home.pos.z + sr * Math.sin(theta) * dist;
    const scheme = schemes[i % schemes.length];
    const tex = makeNurseryTexture(scheme.core, scheme.shell, 5000 + i);
    const group = new THREE.Group();
    const baseSize = 9000 + nebRng() * 5000;
    for (let layer = 0; layer < 2; layer++) {
      const layerSize = baseSize * (1.0 + layer * 0.35);
      const mat = new THREE.SpriteMaterial({
        map: tex, blending: THREE.AdditiveBlending, depthWrite: false,
        transparent: true, opacity: 0.16 - layer * 0.05
      });
      const sp = new THREE.Sprite(mat);
      sp.position.set(cx + (nebRng()-0.5)*800, cy + (nebRng()-0.5)*800, cz + (nebRng()-0.5)*800);
      sp.scale.set(layerSize, layerSize, 1);
      group.add(sp);
    }
    const starTex = makeNebulaStarTex();
    const nbCount = 4 + Math.floor(nebRng() * 4);
    for (let j = 0; j < nbCount; j++) {
      const m = new THREE.SpriteMaterial({
        map: starTex, color: scheme.core,
        blending: THREE.AdditiveBlending, depthWrite: false,
        transparent: true, opacity: 0.85
      });
      const s = new THREE.Sprite(m);
      s.position.set(cx + (nebRng()-0.5)*1800, cy + (nebRng()-0.5)*1800, cz + (nebRng()-0.5)*1800);
      const sz = 400 + nebRng() * 500;
      s.scale.set(sz, sz, 1);
      group.add(s);
    }
    scene.add(group);
  }
}
spawnNebulae();
```

---

## 13. BLACK HOLE LANDMARK

One deterministic black hole at `home.pos + (24000, 4000, -14000)`, radius 900. Build it from three components: an accretion-disc PlaneGeometry (tilted ~55–75°), a second disc rotated 90° in roll (the Interstellar "ring around the back"), a photon-sphere glow Sprite, and a pure-black core Sprite (`renderOrder = 5` so it occludes the glow).

Use the canvas textures from the snippet below. Place after `spawnNebulae()`.

```js
function makeAccretionDiscTexture(seed) {
  const r = mulberry32(seed);
  const size = 512;
  const cnv = document.createElement('canvas'); cnv.width = size; cnv.height = size;
  const ctx = cnv.getContext('2d');
  ctx.fillStyle = '#000'; ctx.fillRect(0, 0, size, size);
  ctx.globalCompositeOperation = 'lighter';
  const cx = size/2, cy = size/2;
  for (let i = 0; i < 600; i++) {
    const a = r() * TAU; const tR = Math.pow(r(), 0.4);
    const rad = size * (0.18 + tR * 0.30);
    const x = cx + Math.cos(a) * rad, y = cy + Math.sin(a) * rad;
    const br = 2 + r() * 6;
    let R, G, B, A;
    if (tR < 0.10)      { R=255; G=255; B=240; A=0.95; }
    else if (tR < 0.30) { R=255; G=230; B=160; A=0.85; }
    else if (tR < 0.55) { R=255; G=170; B= 90; A=0.70; }
    else if (tR < 0.78) { R=240; G=110; B= 60; A=0.50; }
    else                { R=180; G= 70; B=110; A=0.30; }
    A *= 0.6 + r() * 0.4;
    const g = ctx.createRadialGradient(x, y, 0, x, y, br);
    g.addColorStop(0, `rgba(${R},${G},${B},${A})`);
    g.addColorStop(1, `rgba(${R},${G},${B},0)`);
    ctx.fillStyle = g; ctx.fillRect(x-br, y-br, br*2, br*2);
  }
  ctx.globalCompositeOperation = 'destination-out';
  const punch = ctx.createRadialGradient(cx, cy, 0, cx, cy, size*0.21);
  punch.addColorStop(0,    'rgba(0,0,0,1)');
  punch.addColorStop(0.82, 'rgba(0,0,0,1)');
  punch.addColorStop(1,    'rgba(0,0,0,0)');
  ctx.fillStyle = punch; ctx.fillRect(0,0,size,size);
  ctx.globalCompositeOperation = 'destination-in';
  const outer = ctx.createRadialGradient(cx, cy, size*0.30, cx, cy, size*0.50);
  outer.addColorStop(0, 'rgba(0,0,0,1)'); outer.addColorStop(1, 'rgba(0,0,0,0)');
  ctx.fillStyle = outer; ctx.fillRect(0,0,size,size);
  const tex = new THREE.CanvasTexture(cnv);
  tex.colorSpace = THREE.SRGBColorSpace;
  return tex;
}
function makeBlackHoleGlowTexture() {
  const size = 256;
  const cnv = document.createElement('canvas'); cnv.width = size; cnv.height = size;
  const ctx = cnv.getContext('2d'); const cx = size/2, cy = size/2;
  const ring = ctx.createRadialGradient(cx, cy, size*0.20, cx, cy, size*0.28);
  ring.addColorStop(0.00, 'rgba(255,240,200,0)');
  ring.addColorStop(0.30, 'rgba(255,240,200,1.0)');
  ring.addColorStop(0.55, 'rgba(255,180,100,0.55)');
  ring.addColorStop(1.00, 'rgba(255,150, 80,0)');
  ctx.fillStyle = ring; ctx.fillRect(0,0,size,size);
  const halo = ctx.createRadialGradient(cx, cy, size*0.27, cx, cy, size*0.40);
  halo.addColorStop(0, 'rgba(140,160,220,0.18)');
  halo.addColorStop(1, 'rgba(140,160,220,0)');
  ctx.fillStyle = halo; ctx.fillRect(0,0,size,size);
  const tex = new THREE.CanvasTexture(cnv);
  tex.colorSpace = THREE.SRGBColorSpace;
  return tex;
}
function makeBlackHoleCoreTexture() {
  const size = 128;
  const cnv = document.createElement('canvas'); cnv.width = size; cnv.height = size;
  const ctx = cnv.getContext('2d'); const cx = size/2, cy = size/2;
  ctx.fillStyle = '#000'; ctx.fillRect(0,0,size,size);
  const mask = ctx.createRadialGradient(cx, cy, size*0.42, cx, cy, size*0.48);
  mask.addColorStop(0, 'rgba(0,0,0,1)'); mask.addColorStop(1, 'rgba(0,0,0,0)');
  ctx.globalCompositeOperation = 'destination-in';
  ctx.fillStyle = mask; ctx.fillRect(0,0,size,size);
  const tex = new THREE.CanvasTexture(cnv);
  tex.colorSpace = THREE.SRGBColorSpace;
  return tex;
}
function spawnBlackHole(pos, radius, seed) {
  const r = mulberry32(seed);
  const group = new THREE.Group(); group.position.copy(pos);
  const discTex = makeAccretionDiscTexture(seed);
  const discGeom = new THREE.PlaneGeometry(1, 1);
  const discMat = new THREE.MeshBasicMaterial({
    map: discTex, transparent: true, depthWrite: false,
    blending: THREE.AdditiveBlending, side: THREE.DoubleSide
  });
  const disc = new THREE.Mesh(discGeom, discMat);
  const discSize = radius * 10.0;
  disc.scale.set(discSize, discSize, 1);
  const pitch = Math.PI * 0.5 - (0.25 + r() * 0.25);
  disc.rotation.set(pitch, r() * TAU, r() * TAU);
  const disc2 = new THREE.Mesh(discGeom, discMat.clone());
  disc2.material.opacity = 0.55;
  disc2.scale.set(discSize * 0.95, discSize * 0.95, 1);
  disc2.rotation.set(pitch + Math.PI * 0.5, disc.rotation.y, disc.rotation.z);
  group.add(disc); group.add(disc2);
  const glow = new THREE.Sprite(new THREE.SpriteMaterial({
    map: makeBlackHoleGlowTexture(), transparent: true, depthWrite: false,
    blending: THREE.AdditiveBlending, opacity: 0.95
  }));
  glow.scale.set(radius * 5.5, radius * 5.5, 1);
  group.add(glow);
  const core = new THREE.Sprite(new THREE.SpriteMaterial({
    map: makeBlackHoleCoreTexture(), color: 0x000000,
    transparent: true, depthWrite: false, blending: THREE.NormalBlending, opacity: 1
  }));
  core.scale.set(radius * 2.2, radius * 2.2, 1);
  core.renderOrder = 5;
  group.add(core);
  scene.add(group);
}
spawnBlackHole(new THREE.Vector3(home.pos.x + 24000, home.pos.y + 4000, home.pos.z - 14000), 900, 90210);
```

---

## 14. LASER + EXPLOSIONS + SUPERNOVA

### 14.1 Laser

`BoxGeometry(0.4, 0.4, 1)` child of the camera. Raycaster goes against `[starInstMesh, ...destructibles]`. Hits on the star mesh trigger a **supernova**; hits on planets/moons trigger normal **explode** + cascade destruction.

```js
const laserGeom = new THREE.BoxGeometry(0.4, 0.4, 1);
const laserMat = new THREE.MeshBasicMaterial({
  color: 0x22ff44, transparent: true, opacity: 0.95,
  blending: THREE.AdditiveBlending, depthWrite: false
});
const laser = new THREE.Mesh(laserGeom, laserMat);
laser.visible = false;
camera.add(laser);

const laserGlowTex = (function() {
  const c = document.createElement('canvas'); c.width = 64; c.height = 64;
  const x = c.getContext('2d');
  const g = x.createRadialGradient(32,32,0, 32,32,32);
  g.addColorStop(0, 'rgba(80,255,120,1)');
  g.addColorStop(0.4, 'rgba(40,255,80,0.4)');
  g.addColorStop(1, 'rgba(0,0,0,0)');
  x.fillStyle = g; x.fillRect(0, 0, 64, 64);
  return new THREE.CanvasTexture(c);
})();
const laserGlow = new THREE.Sprite(new THREE.SpriteMaterial({
  map: laserGlowTex, blending: THREE.AdditiveBlending, depthWrite: false, opacity: 0.9
}));
laserGlow.visible = false;
camera.add(laserGlow);

let laserTimer = 0;
const raycaster = new THREE.Raycaster();
raycaster.near = 0; raycaster.far = 100000;

function fireLaser() {
  camera.getWorldPosition(_v1);
  _v2.set(0, 0, -1).applyQuaternion(camera.quaternion);
  raycaster.set(_v1, _v2);
  const hits = raycaster.intersectObjects([starInstMesh, ...destructibles], false);
  let dist = 5000;
  if (hits.length > 0) {
    const hitPoint = hits[0].point;
    dist = hits[0].distance;
    const hit = hits[0].object;
    if (hit === starInstMesh) {
      const star = instanceIndexToStar[hits[0].instanceId];
      if (star && !star.dead) { supernova(hitPoint, star.radius); destroyStar(star); }
    } else {
      explode(hitPoint, hit);
      removeDestructible(hit);
    }
  }
  laser.position.set(0, -1.5, -dist*0.5 - 2);
  laser.scale.set(1, 1, dist);
  laser.visible = true;
  laserGlow.position.set(0, -1.5, -3);
  laserGlow.scale.set(8, 8, 1);
  laserGlow.visible = true;
  laserTimer = 0.18;
  playLaserSound();
}
function updateLaser(dt) {
  if (laserTimer > 0) {
    laserTimer -= dt;
    const a = clamp(laserTimer / 0.18, 0, 1);
    laser.material.opacity = a * 0.95;
    laserGlow.material.opacity = a * 0.9;
    if (laserTimer <= 0) { laser.visible = false; laserGlow.visible = false; }
  }
}
```

### 14.2 Explosion pieces

Cap sizes: `EXP_FLASH_MAX=200`, `EXP_RING_MAX=600`, `EXP_SPHERE_MAX=500`. Max 6 active explosions; oldest is cleaned up if a 7th starts.

Implement 7 part-builders that each push a mesh into `rec.parts` with a `userData = { type, lifetime, totalLife, ... }`:

- `addFlash(rec, point, maxSize, life, color)` — Sphere with AdditiveBlending; scales from 0 to maxSize, opacity fades.
- `addRing(rec, point, maxSize, life, color)` — `RingGeometry(0.9, 1.0, 96)`, DoubleSide, billboarded to camera each frame.
- `addSphere(rec, point, maxSize, life, color, peakOpacity)` — FrontSide (so being inside doesn't whiteout), AdditiveBlending.
- `addBigFrag(rec, point, size, speed, life, emissiveColor)` — picks asteroidGeom, MeshStandardMaterial with emissive; drifts on `vel`, tumbles on `angVel`, alpha+emissive fade in final 40%.
- `addMedFrag(rec, point, size, speed, life)` — MeshBasic 0xffaa44, drifts/tumbles.
- `addSparks(rec, point, count, minSpeed, maxSpeed, life, color, pointSize)` — `THREE.Points` with per-vertex velocity array, AdditiveBlending. 14–20 "spike" directions; points cluster around them with small jitter for radial-burst look.
- `addRays(rec, point, rayCount, headSpeed, tailDelay, color, life)` — `LineSegments`; each ray's head extends at `headSpeed*elapsed`, tail at `headSpeed*(elapsed-tailDelay)`, giving a real streak.

### 14.3 `explode(point, targetObj)` (planet/moon hit)

```text
- flash 0.5s, clamp(R*3, 5, 200), color 0xfff2aa
- ring 1.5s, clamp(R*12, 20, 600), color 0xffaa44
- sphere 1.2s, clamp(R*9, 15, 500), color 0x88bbff, peak 0.6
- 8..20 bigFrag: size R*(0.075..0.25), speed 6..24, life 3.5..6, emissive 0xff5522
- 30..60 medFrag: size R*(0.04..0.14), speed 8..28, life 2..4
- sparks: 600, minSpeed 60, maxSpeed 180, life 1.6, color 0xffee88, size 3
- rays: 56, headSpeed 320, tailDelay 0.18, color 0xffd07a, life 1.6
```

### 14.4 `supernova(point, R)` (star hit)

```text
- flash 0.35s, 6500, color 0xffffff
- flash 1.2s, 4200, color 0xffddaa
- flash 2.0s, 2800, color 0xaaddff
- sphere 6.5s, 9500, color 0xaaccff, peak 0.85
- sphere 4.4s, 6500, color 0xffcc88, peak 0.75
- sphere 2.4s, 3800, color 0xfff2aa, peak 0.85
- ring 7.0s, 11500, color 0xffeeaa
- ring 5.4s,  9200, color 0xff8855
- ring 4.0s,  7000, color 0x88ddff
- ring 2.8s,  5000, color 0xff55cc
- ring 8.5s, 13500, color 0xccaaff
- 32..50 bigFrag: size R*(0.045..0.145), speed 120..440, life 11..20, emissive 0xff6622
- 60..90 medFrag: size R*(0.025..0.075), speed 90..330, life 6..12
- sparks: 2400, speeds 240..1060, life 4.2, color 0xfff1b8, size 4
- rays: 140, headSpeed 1900, tailDelay 0.30, color 0xffeac6, life 5.0
- rays:  64, headSpeed 1100, tailDelay 0.45, color 0xddaaff, life 6.0
- sparks: 900, speeds 30..170, life 12.0, color 0xb088ff, size 2.2
```

### 14.5 `updateExplosions(dt)` switch by `ud.type`

```text
flash:    s = maxSize * (1 - lifetime/totalLife); opacity = lifetime/totalLife
ring:     s = maxSize * (1 - lifetime/totalLife); lookAt(camera.position); opacity = (1-prog)*0.9
sphere:   s = maxSize * (1 - lifetime/totalLife); opacity = (1-prog) * peakOpacity
bigFrag/
medFrag:  pos += vel*dt; rotation += angVel*dt; in last 40% of life fade opacity AND emissiveIntensity
sparks:   for each i in pos: pos[i] += vel[i]*dt; opacity = lifetime/totalLife
rays:     head = elapsed * headSpeed; tail = max(0, elapsed - tailDelay) * headSpeed
          for each k: pos[k*6..k*6+2] = origin + dir*tail; pos[k*6+3..k*6+5] = origin + dir*head
          opacity = lifetime/totalLife
```

Cleanup: when any part's `lifetime <= 0`, set `dead=true`, `visible=false`. When all parts dead (or `rec.t > rec.duration + 8`), call `cleanupExplosion(rec)` which removes from scene and disposes geom/material per-type.

### 14.6 `removeDestructible(obj)` cascade

When a **moon** is hit, remove just that moon (planet stays). When a **planet** is hit, cascade: dispose atmosphere, clouds, rings, ringAsteroids; remove all its moons; remove planetRef from its system's planets array.

### 14.7 `destroyStar(star)`

```text
- mark star.dead = true
- zero out its instance matrix (so it disappears from starInstMesh)
- instanceIndexToStar[i] = null
- if currentNearStar === star, clear it; if activeSystemStar === star, deactivateSystem + null it
- rebuildActiveStars(true) so the chunk's pixel-star is also removed
```

---

## 15. AUDIO (WebAudio, all synthesized inline)

Initialize on click-to-start. Master gain 0.55.

**Sub-bass pad**: 3 sawtooth oscillators at C1 (32.70 Hz), E1 (41.20 Hz), G1 (49.00 Hz) → individual gains 0.5 → lowpass 300 Hz → gain 0.08 → master.

**Engine**: sawtooth 38 Hz + square 76 Hz detuned +7c → lowpass 280 Hz Q 1.2 → engineGain (0 idle) → master. `updateEngineAudio(throttle01)`: target saw freq = 28 + throttle*70, square = 56 + throttle*140, target gain = 0.025 + throttle*0.22, `setTargetAtTime(τ=0.18)` for freq, `τ=0.15` for gain.

**Laser SFX** (`playLaserSound`):
- Noise burst 0.10s, bandpass exp from 950→220 Hz, Q 4.2, distortion curve k=3.0, gain 0.5→0.001.
- Saw lead 0.20s, freq exp 240→70 Hz, lowpass exp 900→220 Hz Q 3, gain 0.35→0.001.

**Explosion SFX** (`playExplosionSound`):
- Noise wash 2.2s, lowpass exp 900→45 Hz Q 1.4, distortion k=6, gain 0.55→0.001.
- Sine thump 0.7s, freq exp 95→28 Hz, gain 0.85→0.001.
- 6 crackle bursts (0.08s each) at random offsets 0.05..0.40s, bandpass 300..900 Hz Q 5.

**Supernova SFX** (`playSupernovaSound`):
- Stereo noise wash 7s, lowpass exp 2400→28 Hz Q 1.6, distortion k=8, gain 1.05→0.001.
- Sine whomp 0.9s, freq exp 140→16 Hz, gain 1.2→0.001.
- Sustained sub-sine 5s, freq exp 38→18 Hz, gain 0.72→0.001 (after 0.05s delay).
- 22 crackle bursts (0.10s each) over 2.4s, bandpass 180..1080 Hz Q 5.

Distortion curve helper (waveshaper):
```js
function makeDistortionCurve(amount) {
  const n = 256; const curve = new Float32Array(n); const k = amount;
  for (let i = 0; i < n; i++) {
    const x = (i / n) * 2 - 1;
    curve[i] = ((1 + k) * x) / (1 + k * Math.abs(x));
  }
  return curve;
}
```

---

## 16. CONTROLS — Quaternion-based 6DOF

Constants:
```js
const C_UNIT = 5000; const MAX_C = 100; const MAX_SPEED = C_UNIT * MAX_C; // 500000
const BASE_ACCEL = 3000; const MOUSE_SENS = 0.0006; const ROLL_RATE = 1.5;
const THROTTLE_SMOOTH = 0.85; const ROLL_SMOOTH = 0.20;
const BOOST_RAMP_TIME = 1.0;
const BRAKE_DUR = 0.33; const BRAKE_TAU = 0.067;
const MOUSE_EVENT_CLAMP = 80; const FRAME_DELTA_CAP = 0.30;
```

State: `keys = {}`, `speed = 0`, `throttleCmd = 0`, `rollCmd = 0`, `lastDriftDir = 1`, `brakeStart = -1`, `hudHidden = false`, `mouseDropCount = 0`.

Inputs:
- `keydown`: `Space` (not repeat) → `brakeStart = performance.now()/1000`; `KeyH` (not repeat) → toggle hud + crosshair display; always set `keys[e.code] = true`.
- `keyup`: `keys[e.code] = false`.
- `click-to-start` click → `initAudio()` then `document.body.requestPointerLock()`.
- `pointerlockchange`: on acquire, hide start btn + title overlay, `mouseDropCount = 3`, `introActive = false`. On release, show "► CLICK TO RESUME" button.
- `mousemove`: only when pointer-locked; skip first `mouseDropCount` events; clamp `movementX/Y` to ±80; `yawDelta -= dx * MOUSE_SENS; pitchDelta -= dy * MOUSE_SENS`.
- `mousedown` button 0: `fireLaser()`.

`updateShip(dt)`:
```text
1. Clamp accumulated yawDelta/pitchDelta to ±FRAME_DELTA_CAP.
2. smooth = 1 - exp(-dt / 0.035)
   apply yawDelta*smooth on AXIS_Y (right-multiply camQuat), pitchDelta*smooth on AXIS_X, subtract applied from delta.
3. rollTarget = (Q?1:0) - (E?1:0); rollCmd eases toward target with 1-exp(-dt/ROLL_SMOOTH).
   apply ROLL_RATE * rollCmd * dt on AXIS_Z.
4. camQuat.normalize(); camera.quaternion.copy(camQuat).
5. If brakeStart>=0: speed *= exp(-dt/BRAKE_TAU). After BRAKE_DUR, speed=0, brakeStart=-1, throttleCmd=0.
6. Else: throttleTarget = W-S. lastDriftDir from W/S or sign(speed) when |speed|>200.
   If Shift held: target = dir*MAX_SPEED, maxStep = (MAX_SPEED/BOOST_RAMP_TIME)*dt, step toward target, throttleCmd=dir.
   Else: throttleCmd eases (1-exp(-dt/THROTTLE_SMOOTH)) toward target; speed += BASE_ACCEL*throttleCmd*dt.
   clamp speed to ±MAX_SPEED.
7. Auto-brake: find min clearance to any active-system planet. If <50: speed *= 0.05^dt; if <200: k=(clearance-50)/150; speed *= (0.4+0.6*k)^dt.
8. Forward = (0,0,-1) rotated by camera.quaternion. camera.position += forward * speed * dt.
9. Strafe (A/D): direction = (∓1,0,0) rotated by camera.quaternion. camera.position += direction * (|speed|*0.5+80) * dt.
10. updateEngineAudio(|speed|/MAX_SPEED).
```

---

## 17. HUD

```js
const hud = document.getElementById('hud');
function updateHUD() {
  const cAbs = Math.abs(speed) / C_UNIT;
  const cStr = cAbs < 10 ? cAbs.toFixed(2) : cAbs.toFixed(1);
  const dir = speed > 0.5 ? '▶' : (speed < -0.5 ? '◀' : '·');
  const klass = currentNearStar ? currentNearStar.klass : '-';
  const braking = brakeStart >= 0 ? '<span class="v"> ◼</span>' : '';
  let nearestBody = '', nearestD = Infinity;
  if (currentNearStar) {
    const dStar = camera.position.distanceTo(currentNearStar.pos) - currentNearStar.radius;
    nearestD = Math.max(0, dStar);
    nearestBody = klass;
  }
  if (activeSystemStar && activeSystemStar.system) {
    for (const p of activeSystemStar.system.planets) {
      const d = camera.position.distanceTo(p.mesh.position) - p.radius;
      if (d < nearestD) {
        nearestD = Math.max(0, d);
        nearestBody = p.type === 'gas' ? 'gas' : (p.type === 'ice' ? 'ice' : 'rocky');
      }
    }
  }
  const distStr = nearestD === Infinity ? '—' : nearestD.toFixed(0);
  hud.innerHTML =
    `${dir} <span class="v">${cStr}</span><span class="dim">c</span>${braking}<br>` +
    `<span class="dim">→</span> ${nearestBody} <span class="dim">${distStr}u</span><br>` +
    `<span class="dim">W/S thrust · shift boost · space brake · A/D strafe · Q/E roll · LMB fire · H hide</span>`;
}
```

---

## 18. POST-PROCESSING

```js
const composer = new EffectComposer(renderer);
composer.addPass(new RenderPass(scene, camera));
const bloom = new UnrealBloomPass(new THREE.Vector2(innerWidth, innerHeight), 0.32, 0.38, 0.90);
composer.addPass(bloom);
const finishPass = new ShaderPass({
  uniforms: { tDiffuse: { value: null }, uVignette: { value: 0.42 }, uContrast: { value: 1.04 } },
  vertexShader: `
    varying vec2 vUv;
    void main() { vUv = uv; gl_Position = projectionMatrix * modelViewMatrix * vec4(position, 1.0); }
  `,
  fragmentShader: `
    uniform sampler2D tDiffuse; uniform float uVignette; uniform float uContrast;
    varying vec2 vUv;
    void main() {
      vec3 c = texture2D(tDiffuse, vUv).rgb;
      c = (c - 0.5) * uContrast + 0.5;
      c = max(c, 0.0);
      float lum = dot(c, vec3(0.299, 0.587, 0.114));
      vec3 shadowTint = vec3(0.92, 0.97, 1.05);
      vec3 highTint   = vec3(1.06, 0.99, 0.92);
      c *= mix(shadowTint, highTint, smoothstep(0.10, 0.85, lum));
      vec2 d = vUv - 0.5;
      float r = dot(d, d) * 2.0;
      float v = clamp(1.0 - r * uVignette, 0.0, 1.0);
      gl_FragColor = vec4(c * v, 1.0);
    }
  `
});
composer.addPass(finishPass);

addEventListener('resize', () => {
  camera.aspect = innerWidth / innerHeight;
  camera.updateProjectionMatrix();
  renderer.setSize(innerWidth, innerHeight);
  composer.setSize(innerWidth, innerHeight);
  bloom.setSize(innerWidth, innerHeight);
});
```

---

## 19. INTRO ORBIT + RENDER LOOP

Before the first pointer-lock, the camera slowly orbits the home system's centroid so the opening is a cinematic wide shot.

```js
updateNearStars();

const introCenter = home.pos.clone();
const introStart = camera.position.clone();
let introActive = true;

const clock = new THREE.Clock();
let frame = 0;
function animate() {
  const dt = Math.min(clock.getDelta(), 0.05);
  frame++;
  const t = performance.now() * 0.001;
  starMat.uniforms.uTime.value = t;
  starInstMat.uniforms.uTime.value = t;
  if (window.__anchorMat) window.__anchorMat.uniforms.uTime.value = t;
  for (let i = 0; i < ringSpinMaterials.length; i++) ringSpinMaterials[i].userData.uTime.value = t;
  if (introActive && !document.pointerLockElement) {
    const relX = introStart.x - introCenter.x;
    const relZ = introStart.z - introCenter.z;
    const r0 = Math.hypot(relX, relZ);
    const startAng = Math.atan2(relZ, relX);
    const a = startAng + t * 0.04;
    camera.position.set(
      introCenter.x + Math.cos(a) * r0,
      introStart.y + Math.sin(t * 0.08) * 80,
      introCenter.z + Math.sin(a) * r0
    );
    camera.lookAt(introCenter.x - 100, introCenter.y + 100, introCenter.z - 300);
    camQuat.copy(camera.quaternion);
  } else {
    updateShip(dt);
  }
  rebuildActiveStars(false);
  bgStarsGroup.position.copy(camera.position);
  if (frame % 10 === 0) updateNearStars();
  if (currentNearStar) {
    starLight.target.position.copy(camera.position);
    starLight.target.updateMatrixWorld();
  }
  updateActiveSystems(t);
  updateLaser(dt);
  updateExplosions(dt);
  updateHUD();
  composer.render();
  requestAnimationFrame(animate);
}
animate();
```

---

## 20. ASSEMBLY ORDER (write the file's sections in this exact order)

Inside `<script type="module">`:

1. Imports (R2)
2. Helpers (§2)
3. Renderer + Scene + Camera (§3)
4. Lighting (§4)
5. Star data + chunk streaming + home (§5 first half)
6. Far-star points renderer (§5.1)
7. Instanced star spheres + rebuild fns (§5.2)
8. `rebuildActiveStars(true)`
9. Background group: bg points (§6), anchor stars (§6.1), skydome (§6.2)
10. Asteroid geoms + belt materials + `makeBelt` (§7)
11. `makeSpinningRockMaterial` + `makeRingAsteroidsGroup` (§8)
12. Ring textures + `makeRingGeometry` (§9)
13. `paintPlanetSurface`, `makeAtmoMaterial`, `makeCloudMaterial`, `makeRingMaterial`, `makePlanet`, `makeShowcasePlanet` (§10)
14. `destructibles`, `buildSystem`, `activateSystem`, `deactivateSystem`, `updateActiveSystems` (§11)
15. `currentNearStar`, `updateNearStars` (§11.1)
16. Nebulae: `makeNurseryTexture`, `makeNebulaStarTex`, `spawnNebulae`, call `spawnNebulae()` (§12)
17. Black hole: 3 texture builders + `spawnBlackHole` + call it (§13)
18. Laser (§14.1) — but defer `playLaserSound` call to after audio is defined; OK to forward-declare via function-hoisting (use `function` declarations, not `const`).
19. Explosions: builders + `explode` + `supernova` + `updateExplosions` + `cleanupExplosion` + `disposeMesh` + `removeDestructible` + `destroyStar` (§14.2–14.7)
20. Audio (§15): `audioCtx`, `initAudio`, `makeDistortionCurve`, `playLaserSound`, `playExplosionSound`, `playSupernovaSound`, `updateEngineAudio`
21. Controls (§16): constants, state, event listeners, `updateShip`
22. HUD (§17)
23. Post-processing (§18)
24. Intro orbit + render loop (§19)

`playLaserSound`, `playExplosionSound`, `playSupernovaSound` must guard with `if (!audioCtx) return;`. They are called from §14 functions but audio is initialized later — that's fine because at call-time `audioCtx` will be null until the user clicks to start.

---

## 21. ANTI-BUG CHECKLIST — VERIFY EACH BEFORE EMITTING

- [ ] No `easyFade`, `disposeMesh` is defined, every identifier referenced is defined in this file.
- [ ] `logarithmicDepthBuffer: true` is enabled AND every ShaderMaterial that renders 3D world geometry has `#include <common>` + `#include <logdepthbuf_pars_vertex/fragment>` and `#include <logdepthbuf_vertex/fragment>` calls. (The skydome and pure 2D fullscreen passes are the exception — they don't need it.)
- [ ] `scene.add(camera)` is called (laser is camera child).
- [ ] `camera.far = 400000`, `near = 0.1`. NOT 200000 — far stars need 400000.
- [ ] `toneMapping = ACESFilmicToneMapping`, `toneMappingExposure = 0.28`.
- [ ] AmbientLight color `0x141a2b`, intensity `0.10`.
- [ ] DirectionalLight intensity `3.5`; its target is added to scene.
- [ ] `bgStarsGroup.position.copy(camera.position)` is called every frame in `animate()`.
- [ ] Skydome `renderOrder = -10`, `depthTest = false`, BackSide.
- [ ] Asteroid displacement uses `posNoise(x,y,z)`. `computeVertexNormals()` after displacement. `flatShading: true` on materials.
- [ ] `makeSpinningRockMaterial` patches via `onBeforeCompile`. Per-instance `aSpinAxis` is a `InstancedBufferAttribute(Float32Array, 4)`.
- [ ] Home star is at chunk (0,0,0), G class, color `0xfff2c0`.
- [ ] Showcase planet at home system index 1, radius 145, 3 moons.
- [ ] Planet shaders' uniforms `uPlanetCenterW` and `uSunDirW` are updated every frame in `updateActiveSystems`.
- [ ] `destructibles` includes planets AND moons (not the InstancedMesh star — that's handled separately in `fireLaser`).
- [ ] Bloom params: strength 0.32, radius 0.38, threshold 0.90.
- [ ] FinishPass shader vignette 0.42, contrast 1.04, split-tone teal/orange.
- [ ] HUD font: `Courier New`, 11px, color `rgba(34,255,68,0.78)`, top:10px left:12px. Includes lines for speed (▶/◀/· + c value), nearest body type+distance, and the keybinding hint with `shift boost · space brake`.
- [ ] No `new THREE.Vector3 / Color / Quaternion / Matrix4` inside `animate()` or anything it calls.
- [ ] On `click-to-start`: `initAudio()` THEN `requestPointerLock()`. Pointer-lock acquire sets `mouseDropCount = 3`, hides title overlay, sets `introActive = false`.
- [ ] `MAX_EXPLOSIONS = 6`; oldest dropped when exceeded. Supernova rec.duration = 18.0.
- [ ] All explosion sphere parts use `side: THREE.FrontSide` (no back-side bubble drawing when inside).
- [ ] All `CanvasTexture` instances set `colorSpace = THREE.SRGBColorSpace`.
- [ ] No `console.error` on load. Game responds to W/S/A/D/Q/E/Space/Shift/H/LMB after clicking start.

---

## 22. OUTPUT

Output the entire `galaxy.html` file content beginning with `<!DOCTYPE html>` and ending with `</html>`. **No code fences. No prose before or after. No section headers. Just the file.**
