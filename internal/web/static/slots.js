// The slot machine's front end: a canvas that draws real reels.
//
// The strips painted here are the machine's actual published reel strips,
// and every spin lands on the exact stops the server settled before a frame
// was drawn. This file decides nothing: no near-miss nudging, no timing
// tricks, nowhere to put either — it is a renderer for an outcome that
// already exists in the ledger.
(function () {
  'use strict';

  var machine = document.querySelector('.machine');
  if (!machine) return;

  var REELS = parseInt(machine.dataset.reels, 10);
  var ROWS = parseInt(machine.dataset.rows, 10);
  var STRIPS = JSON.parse(machine.dataset.strips);
  var FREESTRIPS = machine.dataset.freestrips ? JSON.parse(machine.dataset.freestrips) : null;
  var GLYPHS = JSON.parse(machine.dataset.glyphs);

  var form = document.getElementById('spin-form');
  var button = document.getElementById('spin-button');
  var stake = document.getElementById('stake');
  var message = document.getElementById('spin-message');
  var winLine = document.getElementById('spin-win');
  var balance = document.getElementById('balance');
  var canvas = document.getElementById('slot-canvas');
  var frame = document.querySelector('.arcade');
  var bigwin = document.getElementById('bigwin');
  var ctx = canvas.getContext('2d');

  // Geometry, in CSS pixels; the backing store scales by devicePixelRatio.
  var TILE = 76, GAP = 10, PAD = 14;
  var W = REELS * TILE + (REELS - 1) * GAP + PAD * 2;
  var H = ROWS * TILE + (ROWS - 1) * GAP + PAD * 2;
  var DPR = Math.min(window.devicePixelRatio || 1, 2);
  canvas.width = W * DPR;
  canvas.height = H * DPR;
  canvas.style.width = W + 'px';
  canvas.style.height = H + 'px';
  ctx.scale(DPR, DPR);

  function cellX(reel) { return PAD + reel * (TILE + GAP); }
  function cellY(row) { return PAD + row * (TILE + GAP); }

  // ------------------------------------------------------------------
  // Reel state
  // ------------------------------------------------------------------
  // pos is a float index into the strip: the tile drawn at the top row is
  // strip[floor(pos)], shifted by the fraction. Spinning increases pos.
  var reels = [];
  for (var r = 0; r < REELS; r++) {
    reels.push({ strip: STRIPS[r], pos: 0, vel: 0, tween: null, glow: 0 });
  }

  var particles = [];
  var highlights = [];   // {reel,row,phase}
  var paylines = [];     // arrays of {reel,row}
  var spinning = false;

  // ------------------------------------------------------------------
  // Drawing
  // ------------------------------------------------------------------
  function roundRect(x, y, w, h, radius) {
    ctx.beginPath();
    ctx.moveTo(x + radius, y);
    ctx.arcTo(x + w, y, x + w, y + h, radius);
    ctx.arcTo(x + w, y + h, x, y + h, radius);
    ctx.arcTo(x, y + h, x, y, radius);
    ctx.arcTo(x, y, x + w, y, radius);
    ctx.closePath();
  }

  function drawTile(x, y, glyph, blur) {
    // The chunky block: light top, saturated middle, a solid darker ledge.
    var g = ctx.createLinearGradient(0, y, 0, y + TILE);
    g.addColorStop(0, '#6a56d6');
    g.addColorStop(0.12, '#5343b8');
    g.addColorStop(0.9, '#3d2f92');
    g.addColorStop(1, '#2a1f6e');
    ctx.fillStyle = g;
    roundRect(x, y, TILE, TILE, 12);
    ctx.fill();
    ctx.strokeStyle = 'rgba(255,255,255,0.18)';
    ctx.lineWidth = 1;
    roundRect(x + 1, y + 1, TILE - 2, TILE - 2, 11);
    ctx.stroke();

    ctx.save();
    // Multi-character faces ("10", "9") take a smaller size than the emoji.
    var size = glyph.length > 1 && glyph.charCodeAt(0) < 128 ? 26 : 34;
    ctx.font = '900 ' + size + 'px "Segoe UI Emoji", "Noto Color Emoji", system-ui, sans-serif';
    ctx.textAlign = 'center';
    ctx.textBaseline = 'middle';
    ctx.shadowColor = 'rgba(0,0,0,0.45)';
    ctx.shadowOffsetY = 3;
    ctx.shadowBlur = 2;
    if (blur > 0.5) {
      ctx.globalAlpha = 0.4;
      ctx.fillText(glyph, x + TILE / 2, y + TILE / 2 - Math.min(blur * 3, 14));
      ctx.fillText(glyph, x + TILE / 2, y + TILE / 2 + Math.min(blur * 3, 14));
      ctx.globalAlpha = 0.55;
    }
    ctx.fillStyle = '#fff';
    ctx.fillText(glyph, x + TILE / 2, y + TILE / 2 + 1);
    ctx.restore();
  }

  function drawReel(r) {
    var reel = reels[r];
    var x = cellX(r);
    ctx.save();
    roundRect(x - 4, PAD - 6, TILE + 8, H - PAD * 2 + 12, 14);
    ctx.clip();

    var base = Math.floor(reel.pos);
    var frac = reel.pos - base;
    var offset = -frac * (TILE + GAP);
    for (var row = -1; row <= ROWS; row++) {
      var index = ((base + row) % reel.strip.length + reel.strip.length) % reel.strip.length;
      var glyph = GLYPHS[reel.strip[index]] || '?';
      drawTile(x, cellY(row) + offset, glyph, Math.abs(reel.vel) / 10);
    }
    ctx.restore();
  }

  function drawHighlights(now) {
    for (var i = 0; i < highlights.length; i++) {
      var h = highlights[i];
      var pulse = 0.55 + 0.45 * Math.sin(now / 180 + h.phase);
      var x = cellX(h.reel), y = cellY(h.row);
      ctx.save();
      ctx.strokeStyle = 'rgba(255,205,60,' + pulse.toFixed(3) + ')';
      ctx.lineWidth = 4;
      ctx.shadowColor = 'rgba(255,205,60,0.8)';
      ctx.shadowBlur = 16 * pulse;
      roundRect(x + 1, y + 1, TILE - 2, TILE - 2, 12);
      ctx.stroke();
      ctx.restore();
    }
  }

  function drawPaylines() {
    for (var i = 0; i < paylines.length; i++) {
      var line = paylines[i];
      ctx.save();
      ctx.strokeStyle = 'rgba(255,205,60,0.75)';
      ctx.lineWidth = 4;
      ctx.lineJoin = 'round';
      ctx.lineCap = 'round';
      ctx.shadowColor = 'rgba(255,205,60,0.6)';
      ctx.shadowBlur = 10;
      ctx.beginPath();
      for (var j = 0; j < line.length; j++) {
        var px = cellX(line[j].reel) + TILE / 2;
        var py = cellY(line[j].row) + TILE / 2;
        if (j === 0) ctx.moveTo(px, py); else ctx.lineTo(px, py);
      }
      ctx.stroke();
      ctx.restore();
    }
  }

  var PARTICLE_COLORS = ['#ffcd3c', '#ff9f1a', '#35d46a', '#4aa8ff', '#ff5252', '#ffffff'];

  function burst(reel, row, count) {
    var cx = cellX(reel) + TILE / 2, cy = cellY(row) + TILE / 2;
    for (var i = 0; i < count; i++) {
      var angle = Math.random() * Math.PI * 2;
      var speed = 90 + Math.random() * 200;
      particles.push({
        x: cx, y: cy,
        vx: Math.cos(angle) * speed, vy: Math.sin(angle) * speed - 140,
        rot: Math.random() * Math.PI, vrot: (Math.random() - 0.5) * 10,
        life: 1, color: PARTICLE_COLORS[(Math.random() * PARTICLE_COLORS.length) | 0],
        size: 4 + Math.random() * 5
      });
    }
  }

  function stepParticles(dt) {
    for (var i = particles.length - 1; i >= 0; i--) {
      var p = particles[i];
      p.vy += 420 * dt;
      p.x += p.vx * dt;
      p.y += p.vy * dt;
      p.rot += p.vrot * dt;
      p.life -= dt * 0.8;
      if (p.life <= 0 || p.y > H + 20) particles.splice(i, 1);
    }
  }

  function drawParticles() {
    for (var i = 0; i < particles.length; i++) {
      var p = particles[i];
      ctx.save();
      ctx.translate(p.x, p.y);
      ctx.rotate(p.rot);
      ctx.globalAlpha = Math.max(p.life, 0);
      ctx.fillStyle = p.color;
      ctx.fillRect(-p.size / 2, -p.size / 2, p.size, p.size * 0.7);
      ctx.restore();
    }
  }

  var lastFrame = 0;
  function frameLoop(now) {
    var dt = Math.min((now - lastFrame) / 1000, 0.05);
    lastFrame = now;

    // Tween the reels that are landing.
    for (var r = 0; r < REELS; r++) {
      var reel = reels[r];
      if (reel.tween) {
        var t = Math.min((now - reel.tween.t0) / reel.tween.dur, 1);
        // easeOutBack: sails a touch past the stop and springs back — the
        // mechanical clunk, drawn, not decided.
        var s = 1.4;
        var u = t - 1;
        var eased = 1 + (u * u * ((s + 1) * u + s));
        var previous = reel.pos;
        reel.pos = reel.tween.from + (reel.tween.to - reel.tween.from) * eased;
        reel.vel = (reel.pos - previous) / Math.max(dt, 0.001);
        if (t >= 1) {
          reel.pos = reel.tween.to;
          reel.vel = 0;
          if (reel.tween.done) reel.tween.done();
          reel.tween = null;
        }
      } else if (reel.vel !== 0) {
        reel.pos += reel.vel * dt;
      }
    }

    stepParticles(dt);

    ctx.clearRect(0, 0, W, H);
    for (var i = 0; i < REELS; i++) drawReel(i);
    drawPaylines();
    drawHighlights(now);
    drawParticles();

    requestAnimationFrame(frameLoop);
  }
  requestAnimationFrame(function (now) { lastFrame = now; requestAnimationFrame(frameLoop); });

  // ------------------------------------------------------------------
  // Spin choreography
  // ------------------------------------------------------------------
  function setStrips(strips) {
    for (var r = 0; r < REELS; r++) reels[r].strip = strips[r];
  }

  // spinTo runs one full spin onto the given stops and resolves when the
  // last reel has clunked home.
  function spinTo(stops, quick) {
    return new Promise(function (resolve) {
      highlights = [];
      paylines = [];
      var speed = quick ? 22 : 26;         // stops per second while cruising
      var lead = quick ? 350 : 650;        // ms before the first reel lands
      var stagger = quick ? 160 : 260;     // ms between landings
      var landing = quick ? 520 : 700;     // ms of landing tween
      var remaining = REELS;

      for (var r = 0; r < REELS; r++) {
        (function (r) {
          var reel = reels[r];
          reel.vel = speed;
          setTimeout(function () {
            var len = reel.strip.length;
            // Land ahead of the current position on the server's stop, at
            // least two full turns out so the stop feels earned.
            var current = reel.pos;
            var target = stops[r] % len;
            var base = Math.ceil(current / len) * len + 2 * len + target;
            reel.vel = 0;
            reel.tween = {
              from: current, to: base, t0: performance.now(), dur: landing,
              done: function () {
                reel.pos = ((base % len) + len) % len;
                remaining--;
                if (remaining === 0) resolve();
              }
            };
          }, lead + r * stagger);
        })(r);
      }
    });
  }

  function showWins(spin, stakeSat) {
    highlights = [];
    paylines = [];
    var wins = spin.wins || [];
    for (var i = 0; i < wins.length; i++) {
      var win = wins[i];
      var positions = win.positions || [];
      for (var j = 0; j < positions.length; j++) {
        highlights.push({ reel: positions[j].reel, row: positions[j].row, phase: i * 0.9 + j * 0.35 });
        burst(positions[j].reel, positions[j].row, 6);
      }
      if (win.kind === 'line' && positions.length > 1) paylines.push(positions);
    }
    if (spin.win_sat > 0 && frame) frame.classList.add('celebrating');
  }

  function fmtBTC(sat) {
    var whole = Math.floor(sat / 1e8);
    var fracPart = ('00000000' + (sat % 1e8)).slice(-8);
    return whole + '.' + fracPart;
  }

  function countUp(el, sat, dur) {
    var start = performance.now();
    function tick(now) {
      var t = Math.min((now - start) / dur, 1);
      el.textContent = 'Paid ' + fmtBTC(Math.round(sat * t)) + ' BTC';
      if (t < 1) requestAnimationFrame(tick);
    }
    requestAnimationFrame(tick);
  }

  function confetti(count) {
    for (var i = 0; i < count; i++) {
      var piece = document.createElement('span');
      piece.className = 'confetti-piece';
      piece.style.left = (Math.random() * 100) + '%';
      piece.style.background = PARTICLE_COLORS[(Math.random() * PARTICLE_COLORS.length) | 0];
      piece.style.setProperty('--drift', ((Math.random() - 0.5) * 160) + 'px');
      piece.style.setProperty('--spin', (360 + Math.random() * 540) + 'deg');
      piece.style.setProperty('--fall', (1.4 + Math.random() * 1.2) + 's');
      frame.appendChild(piece);
      (function (node) { setTimeout(function () { node.remove(); }, 2800); })(piece);
    }
  }

  function wait(ms) { return new Promise(function (resolve) { setTimeout(resolve, ms); }); }

  async function playRound(payload) {
    var round = payload.round;
    var stakeSat = payload.stake_sat;

    message.textContent = 'Spinning…';
    setStrips(STRIPS);
    await spinTo(round.base.stops, false);
    showWins(round.base, stakeSat);
    if (round.base.win_sat > 0) {
      message.textContent = 'Line up!';
      await wait(900);
    }

    var free = round.free || [];
    if (free.length) {
      message.textContent = free.length + ' free spins!';
      confetti(40);
      await wait(1100);
      if (FREESTRIPS) setStrips(FREESTRIPS);
      for (var i = 0; i < free.length; i++) {
        message.textContent = 'Free spin ' + (i + 1) + ' of ' + free.length;
        await spinTo(free[i].stops, true);
        showWins(free[i], stakeSat);
        await wait(free[i].win_sat > 0 ? 850 : 350);
      }
      setStrips(STRIPS);
    }

    balance.textContent = payload.balance_btc;
    if (payload.win_sat > 0) {
      winLine.className = 'win-line good';
      countUp(winLine, payload.win_sat, 800);
      confetti(Math.min(20 + Math.floor(payload.win_sat / stakeSat) * 4, 80));
      message.textContent = 'Spin #' + payload.nonce + ' — checkable once you publish your seed.';
      if (payload.win_sat >= stakeSat * 15 && bigwin) {
        bigwin.querySelector('.amount').textContent = payload.win_btc + ' BTC';
        bigwin.classList.add('show');
        setTimeout(function () { bigwin.classList.remove('show'); }, 2600);
      }
    } else {
      winLine.textContent = '';
      message.textContent = 'No win on spin #' + payload.nonce + '. Spin again?';
    }
  }

  form.addEventListener('submit', function (event) {
    event.preventDefault();
    if (spinning) return;
    spinning = true;
    button.disabled = true;
    winLine.textContent = '';
    winLine.className = 'win-line';
    if (frame) frame.classList.remove('celebrating');

    var body = new URLSearchParams();
    body.set('csrf', machine.dataset.csrf);
    body.set('stake', stake.value);

    fetch(form.action, {
      method: 'POST',
      headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
      body: body.toString(),
      credentials: 'same-origin'
    }).then(function (response) {
      return response.json().then(function (payload) {
        return { ok: response.ok, payload: payload };
      });
    }).then(function (result) {
      if (!result.ok) {
        message.textContent = result.payload.error || 'That spin could not be taken.';
        winLine.className = 'win-line bad';
        return Promise.resolve();
      }
      return playRound(result.payload);
    }).catch(function () {
      message.textContent = 'The connection dropped. Check your spin history before spinning again.';
      winLine.className = 'win-line bad';
    }).finally(function () {
      spinning = false;
      button.disabled = false;
    });
  });
})();
