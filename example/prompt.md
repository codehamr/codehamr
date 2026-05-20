# Procedural 3D Galaxy — Single-File Browser Experience

You are a senior creative-coding engineer with deep experience in real-time WebGL, Three.js, and shader work. Build one self-contained HTML file named `galaxy.html` that opens directly in a modern desktop browser and runs a procedurally generated 3D galaxy the player can fly through. No build step, no companion files, no external assets — everything lives inside this single HTML file: CSS in a `<style>` block, all logic in a `<script type="module">` block.

Use Three.js for rendering, loaded via an import map from a CDN, together with the matching post-processing add-ons (effect composer, render pass, bloom, and a custom shader pass). Do not introduce any other library.

The result should not feel like a demo. It should feel like the opening of a film: cinematic, atmospheric, and visually polished from the very first frame. Prefer beauty over feature count whenever you have to trade off.

---

## Player experience

When the page loads, a slow cinematic camera orbits around a warm yellow home star. A title overlay reads `GALAXY` with a small spaced-out subtitle such as "A PROCEDURAL UNIVERSE". A discreet prompt invites the player to click to start.

Clicking enters pointer-lock, fades out the title, stops the intro orbit, and hands the player full 6DOF control of a spaceship-like camera:

- `W` / `S` — forward and reverse thrust
- `A` / `D` — lateral strafe
- `Q` / `E` — roll
- Mouse — look (yaw + pitch)
- `Shift` — boost
- `Space` — hard brake
- Left mouse button — fire forward laser
- `H` — toggle the HUD

Orientation must be quaternion-based so there is no gimbal lock at any pitch. Thrust feels inertial — speed ramps up and decays smoothly, the brake decelerates visibly, and boost feels punchy. A small fixed crosshair sits in the center of the screen.

The HUD lives in the top-left corner in a terminal-green monospace style. It has exactly two lines:

1. A compact reminder of all control bindings.
2. A status line of the form `speed: <value>c <direction> · nearby: <body-class> <distance>u`. The speed value is a fraction of an in-scene "speed of light" constant chosen so that even boosted cruising stays well below 1c. Distance is the gap to the nearest body (star, planet, or moon), in scene units. The HUD must hide and re-show together with the crosshair via `H`.

---

## Universe — infinite, chunk-streamed, deterministic

Space is unbounded. Subdivide it into a 3D grid of cubic chunks identified by integer coordinates. A chunk's contents are determined entirely by its coordinates via a stable seeded hash — visiting the same place twice always produces the same stars.

Each chunk picks a region theme via a coarser super-chunk hash. Roughly four flavors:

- **void** — rare or zero stars, but when present they are very large
- **cluster** — many small stars packed tightly around a single anchor (open-cluster look)
- **normal** — the default, one or two random stars
- **giants** — uncommon, one or two very bright hot stars

Stars have a stellar class (M, K, G, F, A) drawn with a realistic frequency bias (cool red dwarfs common, hot blue-white giants rare). Each class has its own characteristic tint. The home star at the universe origin is always a yellow G-class so the opening frame is reliably warm.

As the camera moves across chunk boundaries, the active-star buffer rebuilds: thousands of stars stay live simultaneously across a generously sized loaded volume. Distance fades must be smooth — never a hard pop as a chunk loads or unloads.

Use a small fast deterministic PRNG (a 32-bit hash-style generator is fine) for all procedural decisions. Track the nearest navigable star to the camera every frame; once the player gets close enough, instantiate that star's planetary system.

---

## Stars — rendering

Stars are visualized in two layers that coexist over the entire visible range:

**Distant point sprite.** Every star in the loaded volume is drawn as a bright additive pixel point. The sprite has a sharp Gaussian core, a soft surrounding halo, optional diffraction-like cross spikes that scale in as the projected size grows, and a per-star twinkle that combines a fast and a slow sinusoid. Color matches the stellar class. The point fades smoothly between a near radius (where the glow layer takes over visually) and a very far one (where it dims to invisibility without snapping).

**Soft camera-facing glow billboard.** This is the dominant visual the moment a star is anywhere near the player. The look the player must see is a SOFT, BLURRY, GENTLY WABBERING HAZE that emanates from the star center. Build it from a multi-scale radial gradient — a bright tight core, a softer mid-falloff, and a wide outer haze — combined with a subtle slow angular wob (a low-frequency directional modulation around the disc) and a gentle pulse over time. Add a faint, slowly drifting "plume" component for a hint of irregular rays without ever forming hard streaks. Use additive blending and no depth write. The glow scales with the star's nominal radius so large stars bloom large.

What the glow must not look like:

- a solid 3D sphere
- a ring or donut (no visible silhouette edge)
- a hard-edged disc
- a constant unchanging halo

It is fine — and recommended — to keep an invisible 3D proxy sphere per star purely for raycast hit detection, as long as it does not contribute to the rendered image.

A single real-time directional light tracks the active star, so any planets in its system get a correct day-night terminator.

---

## Background sky

Two layers, both parented to a group that locks its translation to the camera every frame so they read as being at "infinite" distance — they don't translate as the player flies, only the player's rotation rotates them past:

- **Deep-space skydome.** An inverted sphere painted by a fragment shader. The base is a deep navy with a faint warm ambient floor so the void never reads as flat black. Over that, paint a procedural Milky Way galactic band cutting diagonally across the sky via FBM noise: broad soft cloud puffs, a few darker dust silhouettes carved into them, and a brighter warmer galactic-core bulge concentrated in one direction. The band is anchored in world space — flying around must not make it shift.

- **Background pinprick stars.** Several thousand faint dots distributed uniformly on a sphere, plus a sparse second layer of a few dozen bright "anchor" stars with strong diffraction spikes biased toward the galactic plane. Anchor stars twinkle subtly. Together these fill the horizon at every angle so the sky never feels empty regardless of where the chunk-streamed near stars happen to be.

---

## Planetary systems

When the player gets close to a star, build its system: three to eight planets at deterministically seeded orbital radii, eccentricities, and inclinations. Inclinations should genuinely vary in 3D — orbits must not all share the XZ plane.

Three planet types, picked with a realistic bias:

- **Rocky** — small to medium, varied palettes (desert, frozen tundra, volcanic, ocean, hellscape), procedurally painted surface (e.g. canvas-rendered biome maps via hash noise) with bumpy normals.
- **Ice** — cool blue/white tints, frosted look, sometimes lightly cratered.
- **Gas giant** — large, with animated banded clouds via an FBM-based shader. Often ringed.

Every planet carries a Fresnel-style atmosphere shell — a colored rim glow with a sun-side brightness bias and a quiet animated breathe. Many planets carry one or more moons in their own small orbits.

Ringed planets get two things: a textured banded ring disc whose shader respects the planet's shadow, and a real instanced field of 3D asteroid debris embedded inside the ring that orbits around the planet's spin axis. Different planets have different ring tilts.

Orbits, axial spin, ring rotation, and atmosphere/cloud animation all advance with time. Build each system lazily on approach and dispose it cleanly on departure: release geometries, materials, instanced meshes, and any debris fields.

Inside the home system at the universe origin, place a clearly showcase-quality planet — rings, moons, atmosphere — positioned so it's already on-screen during the intro orbit. The first frame must read as impressive.

---

## Asteroids

Pre-bake a small set of irregular asteroid geometries (icosahedron base with per-vertex displacement noise) and share them across the world via instanced meshes — never one mesh per rock. Provide several distinct rock materials in different tones (all rough, flat-shaded, non-metallic). Use these for both standalone asteroid belts inside systems and for the embedded debris inside planetary rings.

Each belt picks its own random plane normal so different belts tilt differently. Belt thickness, inner/outer radius, density, and material all vary per belt. Sprinkle a wider distribution of asteroid sizes — most small, some medium, a few large "named" boulders the player can fly past for a sense of scale.

---

## Nebulae

Place a small number of distant procedural nebulae across the galaxy. Each is a large, soft, semi-transparent FBM-painted volumetric-looking cloud in an evocative color palette — pinks, magentas, teals, deep blues. Edges fade out softly; cores have brighter knots. They are visual landmarks only — no collision, no interactivity. Their job is to give the player something to fly toward.

---

## Black hole landmark

Place one designated black hole somewhere reachable in the universe. It is a dark central disc surrounded by a glowing accretion disk shader: hot inner ring, color graded outward, with an animated swirl. The bloom pass should make the disk visible from a great distance.

---

## Combat — laser and destruction

The left mouse button fires a thin, additively blended laser bolt that travels forward from the camera and hits the first valid target along its ray. Use a single reusable Raycaster — do not allocate a new one per shot.

Hits on planets, moons, and asteroids spawn a multi-phase explosion: a burst of medium and large geometric fragments that drift outward and tumble, a cluster of additive sparks that streak then settle into a slow-drifting dust trail, and a few expanding ray-streaks for cinematic flair. After the burst, the target is cleanly disposed; destroying a planet also cleans up its moons and any ring system attached to it.

Hits on stars do small damage and produce a brief flare. Enough cumulative damage triggers a supernova: a bright shockwave + flare, then the star and its entire planetary system go dark and are removed from the active set. The chunk that contained the star is then rebuilt so the star is also gone from the streamed point layer.

---

## Audio — procedural, inline

All sound is synthesized via WebAudio. No audio files.

- A continuous engine drone whose pitch and gain track throttle and current speed.
- A short percussive laser blip on each shot.
- A muffled thump on explosions, with a deeper low-end punch on supernovae.
- A quiet ambient pad/drone underneath for atmosphere.

Route everything through a master gain at modest volume. Defer audio-context creation and resume until the first user gesture (the click-to-start) — many browsers refuse to start audio before then.

---

## Post-processing and color

Render through the effect composer with:

- A render pass.
- A tightly tuned bloom pass: a high enough threshold that only real light sources bloom, a moderate radius, and a controlled strength. Bloom should feel disciplined, not a smear.
- A final custom shader pass that applies a soft vignette, a mild contrast boost, and a gentle split-tone — slightly warm in highlights, slightly cool in shadows — so the image reads as a film frame and not a sterile WebGL render.

Configure the renderer with ACES filmic tone mapping, sRGB output, and a logarithmic depth buffer (the scene spans many orders of magnitude in scale and a linear depth buffer will z-fight badly). Any custom shaders you write must include the matching log-depth includes so transparent surfaces still compose correctly with the rest of the scene.

---

## Engineering quality bar

- **One file.** No companion CSS or JS. The HTML opens directly in Chrome, Firefox, or Safari and runs.
- **Performance target.** Aim for around 60 fps at 1080p on a mid-range integrated GPU. The animate loop and anything it calls must not allocate on the hot path — reuse a small pool of scratch vectors, quaternions, matrices, and colors declared once at module scope.
- **Determinism.** All procedural content is seeded by integer coordinates through a stable PRNG. Two visits to the same place produce the same result.
- **Cleanup.** When a system or destructible is torn down, dispose its geometries, materials, and instanced meshes properly. No memory leaks across long play sessions.
- **Resize.** Handle window resize for the renderer, the composer, and the camera aspect ratio.
- **User-gesture safety.** Pointer-lock requests and audio context resume only happen on a user gesture, never automatically on load.
- **Readable code.** Sections of the file should be clearly delimited. Comments explain *why* a non-obvious tuning was chosen, not what the code already says.

---

## Visual quality bar

The frame the player sees right after pressing start must look like a polished cinematic shot:

- The home star glowing as a soft, blurry, gently wabbering halo with no hard edge.
- A showcase planet drifting nearby with clearly visible rings, embedded ring debris, and at least one moon.
- The Milky Way arching diagonally across the sky with the warm galactic core bulge off in one direction.
- A scatter of headline anchor stars sparkling with subtle diffraction spikes against the deep navy void.
- Bloom carrying the highlights, vignette darkening the corners just enough, the whole frame tonally cohesive.

If at any point you have to choose between adding another feature and making the existing visuals look beautiful, choose beauty.

---

## Out of scope

- No multiplayer or networking.
- No external textures, fonts, models, or audio files — everything procedural and inline.
- No build step, no bundler, no TypeScript.
- No menus beyond the click-to-start overlay.

---

## Deliverable

Output exactly one file: `galaxy.html`, ready to open in a browser. Nothing else — no README, no commentary, no separate assets. The first line should be `<!DOCTYPE html>` and the last line should be `</html>`.
