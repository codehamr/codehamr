# Procedural 3D Galaxy — Single-File Browser Experience

Build me one self-contained HTML file called `galaxy.html` that opens directly in a modern desktop browser and runs a procedurally generated 3D space simulator the user can fly through. Treat this as a finished piece of work, not a tech demo — the first frame must feel cinematic, the controls must feel good, the audio must feel alive. Beauty and restraint matter more than feature count: if at any point you can't decide between adding more or polishing what's there, polish.

You are acting as a senior creative-coding engineer with strong WebGL, Three.js, and procedural-content experience. Write the file as something you would be proud to ship.

---

## Tech constraints

- Exactly one file. No build step, no bundler, no companion files, no external textures, no model files, no audio files.
- HTML with `<style>` inline and a single `<script type="module">` block.
- Use Three.js loaded from a CDN through an import map, together with the matching post-processing add-ons: effect composer, render pass, unreal-bloom pass, and a custom shader pass for the final grade. Do not pull in any other library.
- The page must run in current Chrome, Firefox, and Safari with no console errors on load or during play.
- Target around 60 fps at 1080p on a mid-range integrated GPU.

---

## Opening moment

When the page loads the camera is already inside the universe, slowly orbiting a warm yellow home star. A large title overlay reads `GALAXY`, with a small wide-letterspaced subtitle below it along the lines of `A PROCEDURAL UNIVERSE`. A discreet label in the lower half says something like `► CLICK TO START`. Both overlays sit in front of the 3D scene with soft green-neon styling and a faint glow.

Any click anywhere on the page must:

1. Initialize the Web Audio context (browsers refuse to start audio before a user gesture).
2. Request pointer lock on the document body.
3. Hide the title and the start label.
4. Stop the cinematic intro orbit and hand the player full control.

When the player presses `Escape` and pointer lock is released, show the start label again as `► CLICK TO RESUME`. Another click re-acquires pointer lock and resumes play.

The intro orbit is a slow drifting arc around the home star with a small vertical bob, framed so the hero planet (described below) is already on screen. The framing must read as a polished establishing shot — calm, symmetrical, no jitter.

---

## Ship controls and feel

Camera orientation is a quaternion you accumulate yourself — never lean on Euler angles or `lookAt` for control. The player must be able to roll fully upside down at any pitch with no gimbal pop.

Bindings:

- `W` / `S` — forward and reverse thrust.
- `A` / `D` — lateral strafe.
- `Q` / `E` — roll left and right.
- Mouse — yaw and pitch via pointer-lock relative movement.
- `Space` — hard brake.
- Left mouse button — fire forward laser.
- `H` — hide and reshow the HUD and crosshair.

Smooth raw mouse deltas with a short exponential time constant so high-DPI sensor jitter is absorbed without adding noticeable lag. Cap the per-event delta defensively. Right after pointer lock re-acquires, drop the first few mouse events — browsers sometimes feed a stale large delta on lock change.

Throttle behavior is the central feel knob. Get this right:

- Holding `W` or `S` follows a non-linear acceleration curve. The first short window is an eased ramp-in that reaches roughly a quarter of top speed, then a longer ramp lifts that toward full top speed. The intent: a quick tap nudges you, holding the key commits to faster and faster travel. Top speed is around thirty times the in-scene "speed of light" unit (a small named constant, used purely as a HUD readout).
- Releasing `W` / `S` preserves the current speed. Space has no friction; the ship coasts indefinitely.
- Re-pressing thrust mid-coast must continue from the current speed fraction along the same curve, not snap back to zero.
- `Space` is a hard exponential brake that stops the ship in about a fifth of a second and plays a distinct brake sound.
- When the camera gets close to a planet surface, automatically and quietly bleed speed so the player can't pile-drive through it.

Frame deltas must be clamped around 50 ms max so a tab-switch doesn't teleport the ship.

This universe is huge, so scale matters. The home star is on the order of a few hundred units in radius, planets a few tens of units, the loaded chunk volume is hundreds of thousands of units across.

---

## HUD

The HUD lives in the top-left corner. Small monospace font, terminal-green tint, soft glow, slightly wide letter-spacing. Two lines only:

1. A subdued reminder of the bindings.
2. A live status line: current speed as a fraction of the speed-of-light unit with a direction marker (`▶` forward, `◀` reverse, `·` at rest), plus the nearest body's class (stellar class letter for a star, or `gas` / `ice` / `rocky` for a planet) and its distance to surface in scene units.

A fixed `+` crosshair sits dead center in matching neon green. HUD and crosshair toggle together via `H`.

Keep it sparse. No bars, no boxes, no icons.

---

## Universe — infinite, chunk-streamed, deterministic

Space is unbounded. Subdivide it into a 3D grid of cubic chunks identified by integer coordinates. A chunk's contents are determined entirely by its integer coordinates through a stable seeded PRNG — visiting the same coordinate twice always produces exactly the same stars. Use a small fast 32-bit hash-style generator (mulberry32 or similar) seeded by mixing chunk coordinates with prime multipliers.

Keep a generously sized cube of chunks live around the camera at all times — large enough that thousands of stars are simultaneously active and the player never sees a hard pop on the horizon. Rebuild the active set only when the camera crosses a chunk boundary, not every frame. Cache chunk contents so re-entering a chunk later is instant and identical.

Above the chunk grid, layer a coarser "region" grid. Each region picks a theme from a coarser hash. Roughly four themes:

- **void** — rare or zero stars, but the ones that exist are very large.
- **cluster** — many small stars tightly bunched around an anchor.
- **normal** — default, one or two random stars.
- **giants** — uncommon, one or two very bright hot stars.

The chunk at the universe origin always contains the home star — always a yellow G-class so the opening frame is reliably warm.

Track the nearest non-dead star to the camera every few frames. When it changes, lazily build the new star's planetary system and tear the previous one down cleanly so memory does not grow.

---

## Star rendering

A star is rendered in two cooperating layers:

- **Far layer — point sprites.** Every star in the loaded volume is drawn as a single GL point with a custom shader. Sharp Gaussian core in the star's stellar-class color, projected size based on physical radius divided by distance and clamped so a close star can't balloon. Brightness is multiplied by a slow per-star twinkle — a single low-amplitude sine using a stable per-position seed, a few percent of amplitude, not a flicker. For the rare close large points, fade in a faint cross-shaped diffraction-spike pattern. Far points stay clean small discs, never glowing pillows.
- **Near layer — additive glow billboards.** Every star also exists as an instanced camera-facing billboard with a richer shader: tight bright core, softer mid glow, wide faint halo, vertical and horizontal diffraction spikes, fainter 45-degree spike pair. World-space size scales with physical radius. Subtle per-star pulse so different stars breathe at different rates.

Each star also has a corresponding invisible 3D sphere mesh sized larger than the visible glow, registered for raycasting only — the laser needs a real hit target per star. Use one instanced mesh for all stars; maintain an index-to-star table so a hit instance ID resolves back to a star object.

Stellar classes drive color and frequency (M most common, A rarest):

- M — cool red, small, dim.
- K — orange, medium-small.
- G — yellow-white, neutral. The home star is one of these.
- F — bright pure white.
- A — large blue-white, rare.

Brightness discipline matters. Bloom plus additive plus uncapped sprite size will turn the entire field into white mush. Hard upper bound on apparent point size, per-point output multiplier well below one, modest tone-mapping exposure, and bloom that bites only the brightest pixels.

A single real-time directional light tracks whichever star is currently nearest. Its color matches that star's tint, its position is the star's position, and its target follows the camera so planets always have a correct sun-side terminator.

---

## Background sky

Two layers, both parented to a group whose translation is locked to the camera every frame so the sky reads as infinite — rotation moves it past, position does not.

- **Procedural deep-space skydome.** An inverted sphere painted by a fragment shader. Deep navy base that brightens very slightly toward "up". Over that, a procedural Milky Way galactic band cutting diagonally across the sky via FBM noise: broad soft cloud puffs along the band, darker dust silhouettes carved into them, a warmer brighter galactic-core bulge concentrated in one direction. The band's tilt is anchored in world space — flying around does not make it shift.
- **Background pinprick stars.** Several thousand small dots distributed uniformly on a sphere out at the back distance, using the same stellar-class palette as the foreground but duller. Plus a sparse second layer of a few dozen brighter "anchor" stars biased toward the galactic plane, with very subtle twinkle and diffraction spikes only on the largest few.

---

## Planetary systems

When the player approaches a star, build its system: four to eight planets at deterministically seeded orbital radii and inclinations. Inclinations must genuinely vary in 3D — orbits should not all share one plane. Build lazily on approach, dispose cleanly on departure.

Three planet types, picked with realistic bias toward rocky and gas:

- **Rocky** — small to medium. Procedurally paint the surface into per-vertex colors using deterministic position-based noise. Mix continents and oceans and deserts with polar ice caps in the highest latitudes. Some carry a thin atmosphere shell.
- **Ice** — pale blues and whites with frosty subtle banding. Most carry a thin bluish atmosphere.
- **Gas giant** — large, with latitudinal bands painted into vertex colors from one of several Jovian-style palettes (warm ochre, cold blue-white, deep indigo, salmon-rust, pale lavender). Storm-like darker patches near the equator. Always carries an atmosphere shell and most carry an animated cloud layer painted on a slightly larger translucent sphere via FBM noise that drifts over time.

Every planet's atmosphere shell renders as a colored back-side rim glow with a sun-facing brightness bias and a low-amplitude shimmer animated over time so the atmosphere quietly breathes.

Many planets carry moons of varied flavors — grey rocky, icy white, volcanic ochre with a faint emissive glow, rust-red, bluish ocean. Each moon orbits on its own randomly tilted plane around its parent.

Ringed planets get a textured banded ring disc with several gaps drawn into the texture (multi-frequency sinusoidal bands plus sharp gap notches in a canvas-painted texture). The ring shader respects the planet's shadow — the ring darkens where the planet would block the sun.

Orbits, axial spin, ring rotation, atmosphere shimmer, and cloud animation all advance with time. As the camera approaches or recedes, planet meshes fade in and out over a short range using a smoothstep on distance — don't pop them in instantly.

---

## Showcase planet

Inside the home system at the universe origin, force one planet to be a clearly showcase-quality body. A large ringed gas giant with:

- Detailed banded rings that fill most of its frame, with visible gaps and a warm sandy tint, and the planet's real shadow falling across them.
- Three contrasting moons in clearly differentiated orbits — an ochre volcanic one with a faint emissive glow, an icy white one, and a smaller blue ocean one.
- A vibrant warm atmosphere.
- A cloud band layer.

Position this planet's orbital phase so it is already on-screen during the intro orbit. The first frame the player ever sees should make them stop and look. This is the calling card.

---

## Asteroids

Pre-bake a small set of irregular asteroid geometries — around ten variants. Start from an icosahedron with one or two subdivisions and displace each vertex outward by a deterministic position-based hash noise so neighboring vertices at the same world position move identically and the mesh stays watertight. Share these geometries across the world via instanced meshes — never one mesh per rock. Provide several distinct rough flat-shaded rock materials in different earthy tones.

Use them for asteroid belts inside planetary systems. Each system gets two to five belts. Each belt picks its own random plane normal so different belts tilt differently. Inner radius, outer radius, thickness, density, and material all vary per belt. Wide size distribution — mostly small pebbles, some medium chunks, a few rare large boulders.

---

## Nebulae

Scatter a small handful of distant nebulae across the local region — five is a good count, distributed reasonably uniformly on a sphere around the home system at a distance comparable to the back-skydome scale.

Each nebula is a soft semi-transparent stellar-nursery cloud, built from a procedurally painted canvas texture used as additive sprite billboards. The texture should mix a wide soft outer halo in a steel-blue or violet "shell" color, several brighter core blobs in a warm emission color (pink-red, orange, magenta, pale yellow, teal), a scattering of small bright pinprick highlights suggesting newborn stars embedded in the cloud, a few darker punched-out blobs for dust silhouettes, and a radial mask so edges fade out softly.

Use evocative color schemes — pink core with steel-blue shell, ember orange with deep blue, salmon with violet, magenta with midnight, emerald with royal. Render each as two stacked sprite layers at slightly different scales, plus a few small bright sprites for newborn stars near the core.

Nebulae are landmarks — no collision, no interactivity, no audio.

---

## Black hole landmark

Place one designated black hole near the home star — not at the home position, but reachable within a short flight. Build it from three parts:

- A tilted accretion-disc plane mesh textured with a procedurally painted swirling disc canvas — hot white near the inner edge, transitioning through gold and orange to deep magenta at the rim, with a punched-out central hole and a softly faded outer ring.
- A camera-facing sprite glow ring tinted warm gold for the photon sphere.
- A pure black camera-facing sprite on top with elevated render order so it cleanly punches the event horizon.

The bloom pass makes the disc visible from a great distance. No physics, no lensing — a clean static composite reads better than a half-implemented gimmick.

---

## Combat — laser and destruction

Left mouse button fires a thin additively blended green laser bolt that travels forward from the camera and hits the first valid target along its ray. Use a single reusable Raycaster — do not allocate per shot. The laser geometry is a thin elongated box parented to the camera, paired with a small soft green glow sprite at the muzzle. Both fade out over a short visible window after each shot.

Targets are stars (via the invisible instanced sphere mesh), planets, moons, and asteroids (via a destructibles list maintained as bodies enter and leave active systems).

Each shot plays a brief laser sound.

Hits on planets, moons, and asteroids spawn a multi-phase explosion built from cleanly separable parts whose sizes are clamped to sensible upper bounds:

- A short bright additive flash sphere expanding quickly and fading.
- An expanding camera-facing additive ring shockwave.
- A soft expanding spherical shock bubble, rendered front-side only so the player inside the bubble is not blinded.
- A burst of large tumbling fragments using the pre-baked asteroid meshes with an emissive material, scattering outward with random axes of spin and individual velocities.
- A denser cloud of smaller faster fragments behind them.
- A radial spark burst as a `THREE.Points` cloud with per-vertex velocities, biased into a few spike directions for a "frozen explosion" silhouette.
- A handful of expanding line-segment ray streaks where the head shoots outward fast and the tail follows with a small delay.

After the burst, the target disposes cleanly. Destroying a planet cascades cleanup to its atmosphere shell, cloud shell, ring disc, and all its moons. Cap the number of live explosions; retire the oldest cleanly when the cap is hit.

Hits on stars trigger a **supernova** — same building blocks but bigger, brighter, longer, multi-colored:

- Three stacked bright flashes in white, warm gold, and cool blue.
- Three stacked shock bubbles in cool blue, warm orange, pale gold.
- Five concentric shock rings in different stellar colors expanding at different rates.
- A heavier cloud of large tumbling fragments and an even denser cloud of medium fragments.
- A bright primary spark burst.
- Two ray-streak bursts at different scales — a big bright warm one and a smaller violet one.
- A long-lived dim violet remnant scattered out as slow-drifting sparks (use a sparks cloud not a sphere so the player flying through later is not blinded).

After the supernova, the star is dead. Mark it dead, zero out its glow billboard and invisible hit sphere instance matrices, force a rebuild of the streamed star points so the dead star is also gone from the far layer, and tear down its planetary system if it was the active one. Play a deep, long, distorted supernova sound.

---

## Audio — procedural, inline

All sound is synthesized with the Web Audio API at runtime. There are no audio files anywhere. Everything routes through a master gain at a modest level.

- **Pad drone.** Continuous low-frequency atmospheric bed — a few detuned sawtooth oscillators at low notes through a hard lowpass. Plays from audio init and never stops. Sits low under everything.
- **Engine.** A continuous voice responding to current throttle: two detuned sawtooth oscillators at the fundamental for a fat body, an octave-up square oscillator for bite, a lowpass filter whose cutoff opens with throttle so the voice brightens as the player pushes it, a waveshaper for warm clipping, a higher triangle whine that only fades in past about thirty percent throttle, a band-passed looping noise voice for airflow turbulence, and a slow sine LFO summed onto the master engine gain for a tremolo wobble. The fundamental pitch shifts with throttle so the engine clearly rises and falls.
- **Laser shot.** Short crackly filtered noise burst plus a fast descending sawtooth sweep. Tens of milliseconds long, bright, distinct.
- **Regular explosion.** Muffled punchy thump — noise burst with lowpass sweep from bright to deep, sub-sine pitch drop layered underneath, handful of small random crackles scheduled across the next half-second.
- **Supernova.** Long, deep, distorted wash with a heavy sub-bass whomp and a longer rumble tail. Pair of overlapping low sine pitch drops for body. Crackle field scattered across a couple of seconds.
- **Brake.** Short downward filtered noise sweep plus a low sine pitch drop, triggered once when `Space` is pressed.

The audio context must be created and resumed only on the first user gesture. Browsers will refuse to start it earlier.

---

## Post-processing and color

Render through an effect composer with three passes:

- The render pass.
- A tightly tuned bloom pass: high threshold so only real light sources bloom, moderate radius, controlled strength. Disciplined, not smeared.
- A final custom shader pass that does a mild contrast boost, a split-tone (slightly warm in highlights, slightly cool in shadows), and a soft circular vignette. The image should read as a film frame, not a sterile WebGL render.

Configure the renderer for ACES filmic tone mapping at a modest exposure (deliberately low — bright sources should bloom, not white-clip), sRGB output, and a logarithmic depth buffer. The scene spans many orders of magnitude in scale and a linear depth buffer will z-fight badly. Any custom shaders that participate in the world depth must include the matching log-depth shader-chunk includes.

Lighting is high-contrast deep-space: a very small ambient light just enough to keep shadowed surfaces from going pure black, plus a single bright directional light tracking the active star. No fill lights.

---

## Engineering quality bar

- **No allocations in the hot loop.** Declare a small pool of reusable scratch vectors, quaternions, matrices, and colors at module scope and reuse them.
- **Disable frustum culling** on the skydome, the streamed star points, the instanced star meshes, and the background star group — their bounding spheres aren't reliable.
- **Determinism.** All procedural content is seeded by integer coordinates through the stable PRNG. Same place always produces same result.
- **Resize.** Handle window resize for the renderer, the composer, the bloom pass, and the camera aspect.
- **Cleanup.** When a system or destructible is torn down, dispose its geometries and materials. No leaks across long sessions.
- **User-gesture safety.** Pointer lock and audio context only on a real click.
- **Section the file.** Use clear comment banners (renderer, star streaming, sky, planets, systems, nebulae, black hole, laser, explosions, audio, controls, HUD, post-processing, render loop). A reader should be able to scroll to the section they need.
- **Comments explain why, not what.** Reserve them for non-obvious tuning choices. Do not narrate every line.

---

## What "good" looks like

- The home star reads as a real glowing body — clean small core, controlled glow, restrained twinkle.
- The showcase planet drifts in frame with clearly visible banded rings and at least one contrasting moon.
- The Milky Way arcs diagonally with the warm galactic-core bulge off in one direction.
- A nebula glows somewhere on the horizon as a colored landmark.
- The background star field is dense but disciplined — small, clean, calm.
- Bloom carries the highlights, vignette darkens the corners, the whole frame is tonally cohesive.
- The ship moves smoothly with a committed throttle curve and a satisfying brake.
- The laser fires reliably and the supernova lands with real weight.
- The audio is alive — pad and engine always playing, events distinct and physical.

Common ways this look goes wrong:

- Stars rendered as huge fluffy glows with no defined core. Fix: sharp core, controlled halo, capped sprite size.
- Bloom turning the field into white mush. Fix: high threshold, modest tone-map exposure.
- Atmosphere shells leaking color across the whole sphere. Fix: true Fresnel rim with day-side bias.
- All orbits in one plane. Fix: randomized plane normals per planet and per moon.
- Explosions clipping the camera with a giant white sphere. Fix: front-side-only shock bubble and clamped sizes.

---

## Out of scope

- No multiplayer or networking.
- No external assets — everything procedural and inline.
- No menus beyond click-to-start and click-to-resume.
- No physics — collision is replaced by the gentle proximity speed-bleed.
- No save / load.

---

## Deliverable

Output exactly one file: `galaxy.html`, ready to open in a browser. Nothing else — no README, no commentary, no separate assets. First line is `<!DOCTYPE html>` and last line is `</html>`. Everything in between is the working program.
